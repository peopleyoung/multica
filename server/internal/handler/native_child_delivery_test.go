package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Exercise the Native task credential, write receipts, revision checks, child
// reply thread and explicit parent-squad handoff together. Awaiting review is
// not a closed child and must not require accepting the parent or its siblings.
func TestNativeChildDeliveryUpdatesReviewAndNotifiesParent(t *testing.T) {
	ctx := context.Background()
	const machine = "native-child-delivery"
	const daemonToken, taskToken = "mdt_child_delivery", "mat_child_delivery"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "Native child worker", testutil.Cols{"provider": "native", "daemon_id": machine})
	workerID := dbfx.Agent(t, "Native child worker", runtimeID)
	leaderID := dbfx.Agent(t, "Native child coordinator", testRuntimeID)
	squadID := dbfx.Squad(t, "Native child coordination", leaderID)
	dbfx.SquadMember(t, squadID, "agent", workerID)
	parentID := dbfx.Issue(t, "Coordinate training and evaluation", testutil.Cols{
		"status": "in_progress", "assignee_type": "squad", "assignee_id": squadID,
	})
	childID := dbfx.Issue(t, "Deliver verified training artifacts", testutil.Cols{
		"status": "todo", "assignee_type": "agent", "assignee_id": workerID, "parent_issue_id": parentID,
	})
	siblingID := dbfx.Issue(t, "Independent evaluation still awaits coordination", testutil.Cols{
		"status": "todo", "parent_issue_id": parentID,
	})
	triggerID := dbfx.Comment(t, childID, "Verify the existing training artifacts and deliver the result")
	taskID := dbfx.Task(t, workerID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": childID, "status": "running", "attempt": 1,
		"dispatched_at": dispatched, "trigger_comment_id": triggerID,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": workerID, "task_id": taskID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, workerID)
	for _, id := range []string{childID, parentID} {
		dbfx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", id)
	}
	step := 0
	var childReply, parentReport CommentResponse
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		step++
		name := ""
		args := map[string]any{"issue_id": childID}
		switch step {
		case 1, 4:
			name = "multica_get_issue"
			if step == 4 {
				if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &childReply); err != nil {
					t.Error(err)
				}
			}
		case 2, 5:
			var issue IssueResponse
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &issue); err != nil {
				t.Error(err)
			}
			name, args["status"], args["expected_revision"] = "multica_update_issue_status", "in_progress", issue.Revision
			if step == 5 {
				args["status"] = "in_review"
			}
		case 3:
			name, args["content"] = "multica_create_comment", "Verified training artifacts are ready for independent acceptance."
		case 6:
			name, args["issue_id"] = "multica_get_issue", parentID
		case 7:
			var parent IssueResponse
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &parent); err != nil {
				t.Error(err)
			}
			if parent.AssigneeID == nil || *parent.AssigneeID != squadID {
				t.Error("parent assignment was not available to the Native worker")
			}
			name, args["issue_id"] = "multica_create_comment", parentID
			args["content"] = fmt.Sprintf("[@Coordinator](mention://squad/%s) Child %s is in_review; evidence comment %s. Please coordinate independent evaluation.", squadID, childID, childReply.ID)
		case 8:
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &parentReport); err != nil {
				t.Error(err)
			}
		default:
			t.Error("unexpected Native child delivery step")
		}
		delta, finish := map[string]any{"content": "Child delivered for review; parent coordinator notified."}, "stop"
		if name != "" {
			raw, _ := json.Marshal(args)
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("child-delivery-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}}, "tool_calls"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})}
	api := httptest.NewServer(nativeBusinessTestRouter(h, machine))
	defer api.Close()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, IssueID: childID, AgentID: workerID, WorkspaceID: testWorkspaceID, TriggerCommentID: triggerID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken}
	dir := t.TempDir()
	backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(ctx, daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || step != 8 {
		t.Fatalf("Native delivery failed: status=%s error=%s step=%d", result.Status, result.Error, step)
	}
	for id, want := range map[string]string{childID: "in_review", parentID: "in_progress", siblingID: "todo"} {
		issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(id))
		if err != nil || issue.Status != want {
			t.Fatalf("issue %s status=%s, want %s; error=%v", id, issue.Status, want, err)
		}
	}
	if childReply.ParentID == nil || *childReply.ParentID != triggerID || parentReport.ParentID != nil || parentReport.SourceTaskID == nil || *parentReport.SourceTaskID != taskID {
		t.Fatal("child reply or parent handoff lost its thread or source task")
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND status='queued' AND is_leader_task=TRUE AND squad_id=$3 AND trigger_comment_id=$4 AND delegated_from_task_id=$5`, parentID, leaderID, squadID, parentReport.ID, taskID); got != 1 {
		t.Fatalf("expected one parent coordinator handoff, got %d", got)
	}
}
