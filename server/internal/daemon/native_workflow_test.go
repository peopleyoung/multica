package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeAutomationContextExcludesWebhookCredentials(t *testing.T) {
	output, err := nativeAutomationContext(`{"autopilot":{"id":"automation","description":"morning digest","future_secret":"private"},"triggers":[{"kind":"webhook","webhook_token":"private-token","webhook_url":"https://private.invalid","webhook_path":"/private","signing_secret_hint":"private-hint","timezone":"Asia/Shanghai"}],"collaborators":["private-collaborator"]}`)
	if err != nil || strings.Contains(output, "private") || !strings.Contains(output, "morning digest") || !strings.Contains(output, "Asia/Shanghai") {
		t.Fatalf("automation context exposed credentials or lost instructions: %s error=%v", output, err)
	}
}

func TestNativeQuickCreateBindsPickerFieldsAndNeverRepeatsCreation(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues" || r.Header.Get("Authorization") != "Bearer mat_run" {
			t.Error("quick-create changed its authority or endpoint")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid issue request")
		}
		for key, want := range map[string]string{"title": "Create this", "priority": "high", "due_date": "2026-12-01", "parent_issue_id": "parent", "project_id": "project", "assignee_type": "squad", "assignee_id": "squad", "origin_type": "quick_create", "origin_id": "run"} {
			if body[key] != want {
				t.Errorf("%s = %v, want %s", key, body[key], want)
			}
		}
		attachments, ok := body["attachment_ids"].([]any)
		if !ok || len(attachments) != 1 || attachments[0] != "selected-attachment" {
			t.Error("quick-create lost selected attachments")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	h := &nativeToolHost{control: NewClient(srv.URL), task: Task{ID: "run", AgentID: "agent", WorkspaceID: "workspace", AuthToken: "mat_run", QuickCreatePrompt: "new objective", QuickCreatePriority: "high", QuickCreateDueDate: "2026-12-01", ProjectID: "project", ParentIssueID: "parent", SquadID: "squad", QuickCreateAttachmentIDs: []string{"selected-attachment"}}}
	call := llm.ToolCall{Name: "multica_create_issue", Arguments: json.RawMessage(`{"title":"Create this","priority":"low","parent_issue_id":"wrong-parent","project_id":"wrong-project","due_date":"2027-01-01","attachment_ids":["wrong-attachment"]}`)}
	if _, err := h.Execute(context.Background(), call); !errors.Is(err, tools.ErrOutcomeUnknown) {
		t.Fatalf("uncertain creation was not preserved: %v", err)
	}
	if _, err := h.Execute(context.Background(), call); err == nil || requests != 1 {
		t.Fatalf("quick-create repeated: requests=%d error=%v", requests, err)
	}
}

func TestNativeContextAndCommentReadsKeepTaskScopeAndPagination(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer mat_run" || r.Header.Get("X-Workspace-ID") != "workspace" {
			t.Error("context read used a different authority")
		}
		switch r.URL.Path {
		case "/api/chat/thread":
			if r.URL.Query().Get("id") != "123.456" || r.URL.Query().Get("limit") != "30" {
				t.Error("thread cursor was not preserved")
			}
			_, _ = w.Write([]byte(`{"messages":[],"next_cursor":"older"}`))
		case "/api/issues/issue/comments":
			if r.URL.Query().Get("thread") != "comment" || r.URL.Query().Get("before") != "2026-11-06T01:00:00Z" || r.URL.Query().Get("before_id") != "older" {
				t.Error("comment thread or cursor was dropped")
			}
			w.Header().Set("X-Multica-Next-Before", "2026-11-05T01:00:00Z")
			w.Header().Set("X-Multica-Next-Before-Id", "oldest")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected context endpoint %s", r.URL)
		}
	}))
	defer srv.Close()
	h := &nativeToolHost{control: NewClient(srv.URL), task: Task{WorkspaceID: "workspace", AuthToken: "mat_run", ChatSessionID: "chat", ChatChannelType: "slack"}}
	if _, err := h.Execute(context.Background(), llm.ToolCall{Name: "multica_read_context", Arguments: json.RawMessage(`{"resource":"chat_thread","thread_id":"123.456"}`)}); err != nil {
		t.Fatal(err)
	}
	output, err := h.Execute(context.Background(), llm.ToolCall{Name: "multica_list_comments", Arguments: json.RawMessage(`{"issue_id":"issue","thread_id":"comment","before":"2026-11-06T01:00:00Z","before_id":"older"}`)})
	var page struct {
		NextBeforeID string `json:"next_before_id"`
	}
	if err != nil || json.Unmarshal([]byte(output), &page) != nil || page.NextBeforeID != "oldest" {
		t.Fatalf("comment pagination lost: %s %v", output, err)
	}
	for _, args := range []string{`{"resource":"chat_history","session_id":"other"}`, `{"resource":"agents","before":"cursor"}`, `{"resource":"autopilot"}`, `{"resource":"members","url":"https://other.invalid"}`} {
		if _, err := h.Execute(context.Background(), llm.ToolCall{Name: "multica_read_context", Arguments: json.RawMessage(args)}); err == nil {
			t.Fatalf("invalid context read reached API: %s", args)
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}
