package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeBusinessToolsPreserveStatusAndCommentAttribution(t *testing.T) {
	const daemonToken, taskToken, machine = "mdt_business_fixture", "mat_business_fixture", "native-business-fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native business", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native business", runtimeID)
	issueID := dbfx.Issue(t, "native business", testutil.Cols{"status": "todo"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "originator_user_id": testUserID, "accountable_user_id": testUserID})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "task_id": taskID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	dbfx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
	step, revision := 0, int64(0)
	var commentID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		step++
		name := ""
		args := map[string]any{"issue_id": issueID}
		switch step {
		case 1:
			name = "multica_get_issue"
		case 2:
			var issue IssueResponse
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &issue); err != nil {
				t.Error(err)
			}
			revision = issue.Revision
			name, args["status"], args["expected_revision"] = "multica_update_issue_status", "in_review", revision
		case 3:
			name, args["content"] = "multica_create_comment", "Native business reply"
		case 4:
			var comment CommentResponse
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &comment); err != nil {
				t.Error(err)
			}
			commentID = comment.ID
			if comment.AuthorType != "agent" || comment.AuthorID != agentID || comment.SourceTaskID == nil || *comment.SourceTaskID != taskID {
				t.Errorf("native comment lost attribution: %+v", comment)
			}
			name = "multica_list_comments"
		case 5:
			name, args = "multica_list_projects", map[string]any{}
		case 6:
		default:
			t.Error("unexpected native business step")
		}
		delta, finish := map[string]any{"content": "Business operations completed."}, "stop"
		if name != "" {
			raw, _ := json.Marshal(args)
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("business-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}}, "tool_calls"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})}
	api := httptest.NewServer(nativeBusinessTestRouter(h, machine))
	defer api.Close()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: agentID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken}
	dir := t.TempDir()
	backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(context.Background(), "Read the issue, update its status, and reply.", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || step != 6 || commentID == "" {
		t.Fatalf("native business loop failed: %+v step=%d", result, step)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil || issue.Status != "in_review" || issue.Revision <= revision {
		t.Fatalf("status write missing: %+v error=%v", issue, err)
	}
	comment, err := testHandler.Queries.GetComment(context.Background(), parseUUID(commentID))
	if err != nil || comment.Content != "Native business reply" || uuidToString(comment.AuthorID) != agentID || uuidToString(comment.SourceTaskID) != taskID {
		t.Fatalf("comment attribution missing: %+v error=%v", comment, err)
	}
}

func nativeBusinessTestRouter(h *Handler, machine string) http.Handler {
	router := chi.NewRouter()
	router.Route("/api/daemon", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine)))
			})
		})
		r.Get("/native/config", h.NativeModelConfig)
		r.Post("/tasks/{taskId}/native/inference", h.NativeInference)
		r.Post("/tasks/{taskId}/native/session", h.AcquireNativeSession)
		r.Post("/tasks/{taskId}/native/checkpoint/{operation}", h.NativeCheckpoint)
		r.Post("/tasks/{taskId}/native/write-result", h.NativeWriteResult)
		r.Post("/tasks/{taskId}/native/attachment", h.NativeReadAttachment)
	})
	router.Group(func(r chi.Router) {
		r.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
		r.Use(middleware.RequireWorkspaceMember(testHandler.Queries))
		r.Use(h.NativeWrite)
		r.Post("/api/upload-file", h.UploadFile)
		r.Get("/api/issues/{id}", testHandler.GetIssue)
		r.Put("/api/issues/{id}", testHandler.UpdateIssue)
		r.Post("/api/issues/{id}/comments", testHandler.CreateComment)
		r.Get("/api/issues/{id}/comments", testHandler.ListComments)
		r.Get("/api/projects", testHandler.ListProjects)
		r.Post("/api/projects", h.CreateProject)
		r.Get("/api/projects/{id}", h.GetProject)
		r.Put("/api/projects/{id}", h.UpdateProject)
		r.Get("/api/projects/{id}/resources", h.ListProjectResources)
		r.Post("/api/projects/{id}/resources", h.CreateProjectResource)
		r.Get("/api/agents", h.ListAgents)
		r.Post("/api/agents", h.CreateAgent)
		r.Get("/api/agents/{id}", h.GetAgent)
		r.Put("/api/agents/{id}", h.UpdateAgent)
		r.Post("/api/agents/{id}/skills/add", h.AddAgentSkills)
		r.Get("/api/skills", h.ListSkills)
		r.Get("/api/squads", h.ListSquads)
		r.Post("/api/squads", h.CreateSquad)
		r.Get("/api/squads/{id}", h.GetSquad)
		r.Put("/api/squads/{id}", h.UpdateSquad)
		r.Get("/api/squads/{id}/members", h.ListSquadMembers)
		r.Post("/api/squads/{id}/members", h.AddSquadMember)
		r.Post("/api/issues", testHandler.CreateIssue)
		r.Get("/api/chat/history", h.GetChatChannelHistory)
		r.Get("/api/chat/thread", h.GetChatThread)
		r.Get("/api/autopilots/{id}", testHandler.GetAutopilot)
		r.Get("/api/autopilots", h.ListAutopilots)
		r.Post("/api/autopilots", h.CreateAutopilot)
		r.Patch("/api/autopilots/{id}", h.UpdateAutopilot)
		r.Post("/api/autopilots/{id}/triggers", h.CreateAutopilotTrigger)
	})
	return router
}

func TestNativeWorkflowContextUsesRunIdentity(t *testing.T) {
	for _, kind := range []string{"chat", "automation"} {
		t.Run(kind, func(t *testing.T) {
			const machine = "native-workflow-context"
			daemonToken, taskToken := "mdt_workflow_"+kind, "mat_workflow_"+kind
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			runtimeID := dbfx.Runtime(t, "native context", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentID := dbfx.Agent(t, "native context", runtimeID)
			cols := testutil.Cols{"runtime_id": runtimeID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "originator_user_id": testUserID, "accountable_user_id": testUserID}
			task := daemon.Task{AgentID: agentID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken}
			resource, want := "chat_history", "Prior human objective"
			if kind == "chat" {
				task.ChatSessionID = dbfx.ChatSession(t, agentID)
				cols["chat_session_id"] = task.ChatSessionID
				dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": task.ChatSessionID, "role": "user", "content": want})
			} else {
				resource, want = "autopilot", "Morning digest instructions"
				task.AutopilotID = dbfx.Insert(t, "autopilot", testutil.Cols{"workspace_id": testWorkspaceID, "title": "Native digest", "description": want, "assignee_id": agentID, "execution_mode": "run_only", "created_by_type": "member", "created_by_id": testUserID})
				task.AutopilotRunID = dbfx.Insert(t, "autopilot_run", testutil.Cols{"autopilot_id": task.AutopilotID, "source": "manual", "status": "running"})
				cols["autopilot_run_id"] = task.AutopilotRunID
				dbfx.Insert(t, "autopilot_trigger", testutil.Cols{"autopilot_id": task.AutopilotID, "kind": "webhook", "webhook_token": "never-in-model-history"})
			}
			task.ID = dbfx.Task(t, agentID, cols)
			task.NativeInputTaskID = task.ID
			dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
			dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "task_id": task.ID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
			dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
			step := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				step++
				delta, finish := map[string]any{"content": "Context read."}, "stop"
				if step == 1 {
					args, _ := json.Marshal(map[string]string{"resource": resource})
					delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "read-context", "type": "function", "function": map[string]any{"name": "multica_read_context", "arguments": string(args)}}}}, "tool_calls"
				} else if step != 2 || !strings.Contains(input.Messages[len(input.Messages)-1].Content, want) || strings.Contains(input.Messages[len(input.Messages)-1].Content, "never-in-model-history") {
					t.Errorf("context lost its task scope or leaked a trigger credential: %+v", input.Messages)
				}
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
			}))
			defer upstream.Close()
			h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})}
			api := httptest.NewServer(nativeBusinessTestRouter(h, machine))
			defer api.Close()
			dir := t.TempDir()
			backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBackend()
			session, err := backend.Execute(context.Background(), daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir})
			if err != nil {
				t.Fatal(err)
			}
			for range session.Messages {
			}
			if result := <-session.Result; result.Status != "completed" || step != 2 {
				t.Fatalf("native %s context failed: %+v step=%d", kind, result, step)
			}
		})
	}
}

func TestNativeQuickCreatePreservesOriginPickerAndAssignmentTrigger(t *testing.T) {
	for _, assignee := range []string{"agent", "squad"} {
		t.Run(assignee, func(t *testing.T) { runNativeQuickCreate(t, assignee) })
	}
}

func runNativeQuickCreate(t *testing.T, assignee string) {
	t.Helper()
	const daemonToken, taskToken, machine = "mdt_quick_create_fixture", "mat_quick_create_fixture", "native-quick-create"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native quick create", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native quick create", runtimeID)
	squadID, expectedAssignee := "", agentID
	if assignee == "squad" {
		squadID = dbfx.Squad(t, "native quick create", agentID)
		dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "leader"})
		expectedAssignee = squadID
	}
	projectID := dbfx.Project(t, "native quick create")
	parentID := dbfx.Issue(t, "native quick create parent", testutil.Cols{"project_id": projectID})
	// Fixtures reserve numbers directly; align the workspace allocator before
	// exercising the production create transaction.
	dbfx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter,
		(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id=$1)) WHERE id=$1`, testWorkspaceID)
	quick := service.QuickCreateContext{Type: service.QuickCreateContextType, Prompt: "Capture this objective", WorkspaceID: testWorkspaceID, RequesterID: testUserID, Priority: "high", DueDate: "2026-12-01", ProjectID: projectID, ParentIssueID: parentID, SquadID: squadID}
	raw, err := json.Marshal(quick)
	if err != nil {
		t.Fatal(err)
	}
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "originator_user_id": testUserID, "accountable_user_id": testUserID, "context": raw})
	selectedID := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "uploader_type": "member", "uploader_id": testUserID, "filename": "selected.txt", "url": "https://cdn.example.com/selected", "content_type": "text/plain", "size_bytes": int64(14)})
	quick.AttachmentIDs = []string{selectedID}
	anchorID := dbfx.Comment(t, parentID, "Historical objective")
	quick.SourceContextID = dbfx.Insert(t, "issue_source_context", testutil.Cols{"id": uuid.NewString(), "workspace_id": testWorkspaceID, "origin_task_id": taskID, "source_issue_id": parentID, "anchor_comment_id": anchorID, "captured_by_user_id": testUserID, "snapshot_version": int16(1), "snapshot": []byte(`{"version":1,"source_issue":{"title":"Historical objective"}}`), "capture_digest": "native-quick-create-fixture", "state": "pending"})
	capturedID := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "source_context_id": quick.SourceContextID, "uploader_type": "member", "uploader_id": testUserID, "filename": "captured.txt", "url": "https://cdn.example.com/captured", "content_type": "text/plain", "size_bytes": int64(14)})
	raw, _ = json.Marshal(quick)
	dbfx.Exec(t, "UPDATE agent_task_queue SET context=$2 WHERE id=$1", taskID, raw)
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "task_id": taskID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	dbfx.Cleanup(t, "DELETE FROM issue WHERE origin_type='quick_create' AND origin_id=$1", taskID)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1 AND id<>$2", agentID, taskID)
	step := 0
	var created IssueResponse
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		step++
		delta, finish := map[string]any{"content": "Created the objective."}, "stop"
		name, args := "", ""
		if step == 1 || step == 2 || step == 4 {
			attachmentID := capturedID
			if step == 1 {
				attachmentID = selectedID
			}
			name, args = "multica_read_attachment", fmt.Sprintf(`{"attachment_id":%q}`, attachmentID)
		} else if step == 3 {
			name, args = "multica_create_issue", `{"title":"Native captured objective","description":"A bounded objective","priority":"low","due_date":"2027-01-01"}`
		}
		if step == 2 || step == 3 || step == 5 {
			if !strings.Contains(input.Messages[len(input.Messages)-1].Content, "captured facts") {
				t.Errorf("native quick-create could not read its selected/captured document: %s", input.Messages[len(input.Messages)-1].Content)
			}
		}
		if step == 4 {
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &created); err != nil {
				t.Error(err)
			}
		} else if step > 5 {
			t.Error("unexpected native quick-create step")
		}
		if name != "" {
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("capture-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "tool_calls"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, Storage: &mockStorage{files: map[string][]byte{"selected": []byte("captured facts"), "captured": []byte("captured facts")}}, LLM: llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})}
	api := httptest.NewServer(nativeBusinessTestRouter(h, machine))
	defer api.Close()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: agentID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken, QuickCreatePrompt: quick.Prompt, QuickCreatePriority: quick.Priority, QuickCreateDueDate: quick.DueDate, ProjectID: projectID, ParentIssueID: parentID, SquadID: squadID, QuickCreateAttachmentIDs: quick.AttachmentIDs, QuickCreateSourceContext: json.RawMessage(fmt.Sprintf(`{"source_issue":{"title":"Historical objective"},"attachments":[{"id":%q}]}`, capturedID))}
	dir := t.TempDir()
	backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(context.Background(), daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || step != 5 || created.ID == "" {
		t.Fatalf("native quick-create failed: %+v step=%d", result, step)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(created.ID))
	if err != nil || issue.Priority != "high" || !issue.DueDate.Valid || issue.DueDate.Time.Format("2006-01-02") != quick.DueDate || uuidToString(issue.ProjectID) != projectID || uuidToString(issue.ParentIssueID) != parentID || uuidToString(issue.AssigneeID) != expectedAssignee || issue.AssigneeType.String != assignee || uuidToString(issue.CreatorID) != agentID || issue.CreatorType != "agent" || issue.OriginType.String != "quick_create" || uuidToString(issue.OriginID) != taskID {
		t.Fatalf("native quick-create lost origin or picker fields: %+v error=%v", issue, err)
	}
	var attached bool
	if err := testPool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM issue_source_context WHERE id=$1 AND issue_id=$2 AND state='attached') AND EXISTS (SELECT 1 FROM attachment WHERE id=$3 AND issue_id=$2)`, quick.SourceContextID, created.ID, selectedID).Scan(&attached); err != nil || !attached {
		t.Fatalf("native quick-create lost captured context or selected upload: attached=%t error=%v", attached, err)
	}
	var assigned int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND status='queued'", created.ID, agentID).Scan(&assigned); err != nil || assigned != 1 {
		t.Fatalf("native create lost the existing assignment trigger: count=%d error=%v", assigned, err)
	}
	if squadID != "" {
		var routedSquad string
		var leader bool
		if err := testPool.QueryRow(context.Background(), "SELECT squad_id::text, is_leader_task FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND status='queued'", created.ID, agentID).Scan(&routedSquad, &leader); err != nil || routedSquad != squadID || !leader {
			t.Fatalf("native creation lost squad leader routing: squad=%s leader=%t error=%v", routedSquad, leader, err)
		}
	}
}
