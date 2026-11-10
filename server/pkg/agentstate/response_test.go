package agentstate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBusinessResponseDoesNotPersistAttachmentCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		upload     bool
	}{
		{"upload", `{"id":"file-1","filename":"result.txt","url":"secret-storage","download_url":"secret-capability","future_token":"secret-future"}`, true},
		{"issue", `{"id":"issue-1","revision":9007199254740993,"description":"https://example.com/user-text","attachments":[{"id":"file-1","filename":"result.txt","url":"secret-storage","download_url":"secret-capability","attachment_download_url":"secret-download","future_token":"secret-future"}]}`, false},
		{"comments", `[{"content":"https://example.com/user-text","replies":[{"attachments":[{"id":"file-1","filename":"result.txt","url":"secret-storage"}]}]}]`, false},
		{"chat", `{"messages":[{"attachments":[{"id":"file-1","filename":"result.txt","download_url":"secret-capability"}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projected, err := ProjectBusinessResponse(json.RawMessage(tc.body), tc.upload)
			if err != nil || strings.Contains(string(projected), "secret-") || !strings.Contains(string(projected), `"download_path":"/api/attachments/file-1/download"`) || !strings.Contains(string(projected), `"filename":"result.txt"`) {
				t.Fatalf("projection=%s error=%v", projected, err)
			}
			if strings.Contains(tc.body, "user-text") && !strings.Contains(string(projected), "https://example.com/user-text") {
				t.Fatalf("user-authored URL was removed: %s", projected)
			}
			if tc.name == "issue" && !strings.Contains(string(projected), "9007199254740993") {
				t.Fatalf("revision lost integer precision: %s", projected)
			}
		})
	}
}
