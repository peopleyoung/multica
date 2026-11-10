package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeArtifactUploadsUseActualStorageAndRunScope(t *testing.T) {
	for _, surface := range []string{"issue", "chat", "text channel"} {
		t.Run(surface, func(t *testing.T) {
			ctx := context.Background()
			machine, daemonToken, taskToken := "native-artifact-"+uuid.NewString(), "mdt_"+uuid.NewString(), "mat_"+uuid.NewString()
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			runtimeID := dbfx.Runtime(t, "native artifact", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentID := dbfx.Agent(t, "native artifact", runtimeID)
			cols := testutil.Cols{"runtime_id": runtimeID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "originator_user_id": testUserID, "accountable_user_id": testUserID}
			issueID, chatID := "", ""
			if surface == "issue" {
				issueID = dbfx.Issue(t, "native artifact", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
				cols["issue_id"] = issueID
			} else {
				chatID = dbfx.ChatSession(t, agentID)
				cols["chat_session_id"] = chatID
			}
			taskID := dbfx.Task(t, agentID, cols)
			dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
			dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "task_id": taskID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
			dbfx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
			dbfx.Cleanup(t, "DELETE FROM attachment WHERE uploader_id=$1", agentID)
			storage := &mockStorage{}
			step, attachmentID := 0, ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Messages []struct{ Role, Content string }
					Tools    []struct{ Function struct{ Name string } }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				step++
				found := false
				for _, tool := range input.Tools {
					found = found || tool.Function.Name == "multica_upload_artifact"
				}
				if found != (surface != "text channel") {
					t.Errorf("upload tool does not match delivery surface: %s", surface)
				}
				delta, finish := map[string]any{"content": "File uploaded."}, "stop"
				if surface == "text channel" {
					delta["content"] = "Text result; this channel cannot receive files."
				} else if step == 1 {
					delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "upload-once", "type": "function", "function": map[string]any{"name": "multica_upload_artifact", "arguments": `{"path":"result.txt"}`}}}}, "tool_calls"
				} else {
					last := input.Messages[len(input.Messages)-1]
					var output struct{ ID, Filename string }
					if json.Unmarshal([]byte(last.Content), &output) != nil || last.Role != "tool" || output.ID == "" || output.Filename != "result.txt" || strings.Contains(last.Content, "cdn.example.com") || strings.Contains(last.Content, "download_url") {
						t.Errorf("upload result is missing metadata or leaked a storage capability: %s", last.Content)
					}
					attachmentID = output.ID
				}
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
			}))
			defer upstream.Close()
			h := *testHandler
			h.Storage = storage
			h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
			api := httptest.NewServer(nativeBusinessTestRouter(&h, machine))
			defer api.Close()
			task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: agentID, IssueID: issueID, ChatSessionID: chatID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken}
			if surface == "text channel" {
				task.ChatChannelType = "wecom"
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("actual artifact bytes"), 0600); err != nil {
				t.Fatal(err)
			}
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
			if result := <-session.Result; result.Status != "completed" {
				t.Fatalf("artifact run failed: %+v", result)
			}
			if surface == "text channel" {
				if step != 1 || dbfx.Count(t, "SELECT count(*) FROM attachment WHERE uploader_id=$1", agentID) != 0 {
					t.Fatal("text-only surface uploaded an artifact")
				}
				return
			}
			att, err := h.Queries.GetAttachment(ctx, db.GetAttachmentParams{ID: parseUUID(attachmentID), WorkspaceID: parseUUID(testWorkspaceID)})
			if err != nil || uuidToString(att.UploaderID) != agentID || att.UploaderType != "agent" || step != 2 {
				t.Fatalf("artifact lost producing agent: %+v error=%v steps=%d", att, err, step)
			}
			if surface == "issue" && uuidToString(att.IssueID) != issueID || surface == "chat" && (uuidToString(att.ChatSessionID) != chatID || uuidToString(att.TaskID) != taskID) {
				t.Fatalf("artifact crossed its run's result surface: %+v", att)
			}
			reader, err := storage.GetReader(ctx, storage.KeyFromURL(att.Url))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != "actual artifact bytes" || dbfx.Count(t, "SELECT count(*) FROM attachment WHERE uploader_id=$1", agentID) != 1 || dbfx.Count(t, "SELECT count(*) FROM native_session WHERE agent_id=$1 AND (SELECT count(*) FROM jsonb_each(write_outcomes))=1", agentID) != 1 {
				t.Fatal("artifact bytes, single upload, or durable receipt missing")
			}
		})
	}
}
