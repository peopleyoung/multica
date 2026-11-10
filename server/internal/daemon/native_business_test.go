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

func TestNativeBusinessToolsUseOnlyTaskIdentityAndFixedOperations(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer mat_run" || r.Header.Get("X-Workspace-ID") != "workspace" {
			t.Error("business tool used another identity")
		}
		if r.URL.Path != "/api/issues/MUL-123" || r.Method != http.MethodPut {
			t.Errorf("unexpected business operation: %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["status"] != "in_review" || body["expected_revision"] != float64(4) {
			t.Errorf("business write changed fields: %+v", body)
		}
		_, _ = w.Write([]byte(`{"status":"in_review"}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("owner-token-must-never-be-used")
	host := &nativeToolHost{control: client, task: Task{AuthToken: "mat_run", WorkspaceID: "workspace"}}
	call := llm.ToolCall{Name: "multica_update_issue_status", Arguments: json.RawMessage(`{"issue_id":"MUL-123","status":"in_review","expected_revision":4}`)}
	if _, err := host.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"issue_id":"../secrets","status":"done","expected_revision":4}`,
		`{"issue_id":"MUL-123","status":"done"}`,
		`{"issue_id":"MUL-123","status":"done","expected_revision":null}`,
		`{"issue_id":"MUL-123","status":"done","expected_revision":4,"workspace_id":"other"}`,
		`{"issue_id":"MUL-123","status":42,"expected_revision":4}`,
	} {
		call.Arguments = json.RawMessage(args)
		if _, err := host.Execute(context.Background(), call); err == nil {
			t.Fatalf("invalid input accepted: %s", args)
		}
	}
	if requests != 1 {
		t.Fatalf("invalid operations reached API: %d", requests)
	}
}

func TestNativeBusinessWriteFailureNeverRetriesAndPreservesUnknownOutcome(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private server details must not reach model"))
			}))
			defer srv.Close()
			host := &nativeToolHost{control: NewClient(srv.URL), task: Task{AuthToken: "mat_run", WorkspaceID: "workspace"}}
			_, err := host.Execute(context.Background(), llm.ToolCall{Name: "multica_create_comment", Arguments: json.RawMessage(`{"issue_id":"MUL-1","content":"reply"}`)})
			if err == nil || requests != 1 || strings.Contains(err.Error(), "private server") || errors.Is(err, tools.ErrOutcomeUnknown) != (status >= 500) {
				t.Fatalf("unsafe write failure: requests=%d error=%v", requests, err)
			}
		})
	}
}

func TestNativeCommentDefaultsToCurrentTriggerWithoutReusingOldThread(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			ParentID string `json:"parent_id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ParentID != "current-trigger" {
			t.Error("comment targeted the wrong thread")
		}
		_, _ = w.Write([]byte(`{"id":"created"}`))
	}))
	defer srv.Close()
	host := &nativeToolHost{control: NewClient(srv.URL), task: Task{AuthToken: "mat_run", WorkspaceID: "workspace", IssueID: "issue", IssueIdentifier: "MUL-1", TriggerCommentID: "current-trigger"}}
	for _, args := range []string{`{"issue_id":"issue","content":"reply"}`, `{"issue_id":"MUL-1","content":"reply"}`} {
		if _, err := host.Execute(context.Background(), llm.ToolCall{Name: "multica_create_comment", Arguments: json.RawMessage(args)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := host.Execute(context.Background(), llm.ToolCall{Name: "multica_create_comment", Arguments: json.RawMessage(`{"issue_id":"issue","content":"reply","parent_id":"previous-trigger"}`)}); err == nil || !strings.Contains(err.Error(), "current-trigger") {
		t.Fatal("stale parent did not receive actionable correction")
	}
	if calls != 2 {
		t.Fatal("stale parent reached API")
	}
}
