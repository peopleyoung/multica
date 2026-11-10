package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeInputAttachmentsSelectCurrentSurfaceAndCapturedClones(t *testing.T) {
	capture := json.RawMessage(`{"source_issue":{"attachments":[{"id":"clone-issue","source_attachment_id":"source-issue","content_type":"image/png"},{"id":"document","content_type":"text/plain"}]},"comment_thread":[{"attachments":[{"id":"clone-comment","source_attachment_id":"source-comment","content_type":"image/jpeg"},{"id":"clone-issue","content_type":"image/png"}]}]}`)
	for _, tt := range []struct {
		name string
		task Task
		want []string
	}{
		{"chat", Task{ChatSessionID: "chat", ChatMessageAttachments: []ChatAttachmentMeta{{ID: "chat-file"}, {ID: "chat-file"}}, QuickCreateAttachmentIDs: []string{"other"}, QuickCreateSourceContext: capture}, []string{"chat-file"}},
		{"selected_and_cloned", Task{QuickCreatePrompt: "create", QuickCreateAttachmentIDs: []string{"selected", "selected"}, QuickCreateSourceContext: capture}, []string{"selected", "clone-issue", "clone-comment"}},
		{"issue_ignores_stale_capture", Task{IssueID: "issue", QuickCreatePrompt: "create", QuickCreateAttachmentIDs: []string{"other"}, QuickCreateSourceContext: capture}, nil},
		{"automation_ignores_stale_capture", Task{AutopilotRunID: "automation", QuickCreateSourceContext: capture}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := nativeInputAttachments(tt.task)
			var ids []string
			for _, ref := range refs {
				ids = append(ids, ref.ID)
			}
			if err != nil || !reflect.DeepEqual(ids, tt.want) {
				t.Fatalf("input attachments=%v want=%v error=%v", ids, tt.want, err)
			}
		})
	}
	for _, task := range []Task{
		{QuickCreatePrompt: "create", QuickCreateSourceContext: json.RawMessage(`{"source_issue":`)},
		{QuickCreatePrompt: "create", QuickCreateAttachmentIDs: []string{"1", "2", "3"}, QuickCreateSourceContext: capture},
		{ChatSessionID: "chat", ChatMessageAttachments: []ChatAttachmentMeta{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}, {ID: "5"}}},
	} {
		if _, err := nativeInputAttachments(task); err == nil {
			t.Fatal("invalid or oversized attachment selection was silently accepted")
		}
	}
}

func TestNativeArtifactsUseTaskIdentityAndDeliverySurface(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("artifact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	requests := 0
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/upload-file" || r.Header.Get("Authorization") != "Bearer mat_run" || r.Header.Get("X-Workspace-ID") != "workspace" {
			t.Error("artifact used another identity or endpoint")
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("task_id") != "run" || r.FormValue("issue_id") != "" || r.FormValue("chat_session_id") != "" {
			t.Error("artifact supplied an arbitrary conversation target")
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if string(data) != "artifact bytes" {
			t.Errorf("uploaded wrong artifact: %q", data)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"attachment-1","filename":"result.txt"}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("owner-must-not-be-used")
	h := &nativeToolHost{Environment: host, control: client, task: Task{ID: "run", AuthToken: "mat_run", WorkspaceID: "workspace", ChatSessionID: "chat", ChatChannelType: "slack"}}
	call := llm.ToolCall{Name: "multica_upload_artifact", Arguments: json.RawMessage(`{"path":"result.txt"}`)}
	if _, err := h.Execute(context.Background(), call); err == nil || requests != 0 {
		t.Fatal("uploaded to a channel without delivery capability")
	}
	for _, definition := range h.Definitions() {
		if definition.Name == call.Name {
			t.Fatal("unsupported channel advertised artifact delivery")
		}
	}
	h.task.ChatChannelDeliversFiles = true
	output, err := h.Execute(context.Background(), call)
	var result struct{ Type, ID, DownloadPath string }
	if err != nil || json.Unmarshal([]byte(output), &result) != nil || result.Type != "attachment" || result.ID != "attachment-1" {
		t.Fatalf("artifact result: %s %v", output, err)
	}
	for _, path := range []string{"../outside", "/absolute", "."} {
		call.Arguments, _ = json.Marshal(map[string]string{"path": path})
		if _, err := h.Execute(context.Background(), call); err == nil {
			t.Fatalf("unconfined artifact accepted: %s", path)
		}
	}
	if requests != 1 {
		t.Fatalf("invalid artifacts reached API: %d", requests)
	}
	call.Arguments = json.RawMessage(`{"path":"result.txt"}`)
	status = http.StatusServiceUnavailable
	if _, err := h.Execute(context.Background(), call); !errors.Is(err, tools.ErrOutcomeUnknown) || requests != 2 {
		t.Fatalf("uncertain upload retried or lost: requests=%d error=%v", requests, err)
	}
}
