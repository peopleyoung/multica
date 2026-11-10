package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestJobShortHandleRetainsInternalAdmissionAndEarlyLogAcrossRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir, journal := t.TempDir(), t.TempDir()
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	first := jobCall(t, h, "owner", "launch", "shell", `{"command":"printf early-error; head -c 300000 /dev/zero; printf tail","wait_ms":2000}`)
	id := first["job_id"].(string)
	if len(id) != 22 || !strings.HasPrefix(id, "shell-") || !strings.HasPrefix(first["output"].(string), "early-error") || first["output_truncated"] != false {
		t.Fatal("short handle or retained early log missing")
	}
	query, _ := json.Marshal(map[string]any{"job_id": id, "text": "early-error"})
	search := jobCall(t, h, "owner", "search", "job_search", string(query))
	if len(search["matches"].([]any)) != 1 {
		t.Fatal("early error cannot be searched")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, err = New(dir, Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, handle := range []string{id, jobID("owner", "launch")} {
		query, _ := json.Marshal(map[string]any{"job_id": handle, "wait_ms": 0})
		out := jobCall(t, h, "owner", "read", "job_output", string(query))
		if !strings.HasPrefix(out["output"].(string), "early-error") {
			t.Fatal("restart lost log or canonical admission identity")
		}
	}
}

func TestJobCompletionNoticesAreDerivedFromCommittedHistory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	h, err := New(t.TempDir(), Permissions{Shell: true}, os.Environ(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	jobCall(t, h, "owner", "launch", "shell", `{"command":"printf complete","wait_ms":2000}`)
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "owner"}, "")
	notices, err := h.JobNotifications(ctx, nil)
	if err != nil || len(notices) != 1 {
		t.Fatalf("missing notification: %v", err)
	}
	again, err := h.JobNotifications(ctx, nil)
	if err != nil || len(again) != 1 || again[0] != notices[0] {
		t.Fatal("uncommitted notification was consumed")
	}
	again, err = h.JobNotifications(ctx, []llm.ModelMessage{{Role: "user", Content: notices[0]}})
	if err != nil || len(again) != 0 {
		t.Fatal("committed notification delivered twice")
	}
	other := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "other"}, "")
	if out, err := h.JobNotifications(other, nil); err != nil || len(out) != 0 {
		t.Fatal("notification crossed ownership")
	}
}

func TestLargeFilePaginationAndConfinement(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("0123456789", 200000)
	if err := os.WriteFile(filepath.Join(dir, "large"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := New(dir, Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var collected strings.Builder
	for offset := int64(0); ; {
		raw, _ := json.Marshal(map[string]any{"path": "large", "offset": offset, "limit": 65536})
		out, err := h.Execute(context.Background(), llm.ToolCall{Name: "read_file", Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Output string `json:"output"`
			Next   int64  `json:"next_offset"`
			More   bool   `json:"has_more"`
		}
		if json.Unmarshal([]byte(out), &page) != nil || len(out) > outputLimit+512 {
			t.Fatal("invalid or unbounded page")
		}
		collected.WriteString(page.Output)
		if !page.More {
			break
		}
		if page.Next <= offset {
			t.Fatal("pagination stalled")
		}
		offset = page.Next
	}
	if collected.String() != content {
		t.Fatal("pagination lost or repeated bytes")
	}
	for _, args := range []string{`{"path":"../outside","offset":0}`, `{"path":"large","offset":-1}`, `{"path":"large","limit":65537}`} {
		if _, err := h.Execute(context.Background(), llm.ToolCall{Name: "read_file", Arguments: json.RawMessage(args)}); err == nil {
			t.Fatal("invalid page accepted")
		}
	}
}

func TestFilePagePreservesUnicodeAndBoundsEscapedOutput(t *testing.T) {
	for _, content := range []string{strings.Repeat("汉字", 20000), strings.Repeat("\x00", 70000)} {
		t.Run("page", func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "text"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			h, err := New(dir, Permissions{}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			var collected strings.Builder
			for offset := int64(0); ; {
				raw, _ := json.Marshal(map[string]any{"path": "text", "offset": offset})
				out, err := h.Execute(context.Background(), llm.ToolCall{Name: "read_file", Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				var page struct {
					Output string `json:"output"`
					Next   int64  `json:"next_offset"`
					More   bool   `json:"has_more"`
				}
				if json.Unmarshal([]byte(out), &page) != nil || len(out) > outputLimit+512 {
					t.Fatal("invalid encoded page")
				}
				collected.WriteString(page.Output)
				if !page.More {
					break
				}
				if page.Next <= offset {
					t.Fatal("stalled page")
				}
				offset = page.Next
			}
			if collected.String() != content {
				t.Fatal("pagination split or lost text")
			}
		})
	}
}
