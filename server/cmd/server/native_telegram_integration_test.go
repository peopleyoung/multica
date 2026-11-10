package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/realtime"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type nativeTelegramTransport func(*http.Request) (*http.Response, error)

func (f nativeTelegramTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Use the production router's Telegram wiring, including object storage and
// terminal-delivery workers. Every provider request terminates in this test.
func TestNativeTelegramArtifactDeliveryThroughServerRouter(t *testing.T) {
	t.Setenv("MULTICA_LLM_API_KEY", "")
	t.Setenv("MULTICA_LLM_BASE_URL", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "")
	key := bytes.Repeat([]byte{0x84}, secretbox.KeySize)
	t.Setenv("MULTICA_TELEGRAM_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("123:fixture-secret"))
	if err != nil {
		t.Fatal(err)
	}
	botID := "native-telegram-" + dbid.NewV7().String()
	config, err := json.Marshal(map[string]string{"app_id": botID, "bot_token_encrypted": base64.StdEncoding.EncodeToString(encrypted)})
	if err != nil {
		t.Fatal(err)
	}
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-telegram-"+dbid.NewV7().String(), "mdt_"+dbid.NewV7().String()
	runtimeID := fx.Runtime(t, "native telegram", dbfx.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native telegram", runtimeID)
	fx.Insert(t, "daemon_token", dbfx.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	installation := fx.Insert(t, "channel_installation", dbfx.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "telegram", "config": string(config), "installer_user_id": testUserID, "status": "active"})
	fx.Insert(t, "channel_user_binding", dbfx.Cols{"installation_id": installation, "workspace_id": testWorkspaceID, "channel_type": "telegram", "channel_user_id": "sender", "multica_user_id": testUserID})
	for _, table := range []string{"channel_inbound_dedup", "channel_task_delivery", "channel_reply_delivery"} {
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE installation_id=$1", installation)
	}
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM attachment WHERE uploader_id=$1", agentID)

	var mu sync.Mutex
	textSends, fileSends := 0, 0
	sent := make(chan struct{}, 2)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = nativeTelegramTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "127.0.0.1" || r.URL.Hostname() == "localhost" {
			return originalTransport.RoundTrip(r)
		}
		if r.URL.Host != "api.telegram.org" || !strings.HasPrefix(r.URL.Path, "/bot123:fixture-secret/") {
			return nil, fmt.Errorf("unexpected provider host or bot identity")
		}
		result := `true`
		switch filepath.Base(r.URL.Path) {
		case "sendChatAction":
		case "sendMessage":
			var body struct {
				ChatID int64  `json:"chat_id"`
				Text   string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChatID != 42 || body.Text != "Native report attached." {
				return nil, fmt.Errorf("unexpected Telegram reply: chat=%d text=%q error=%v", body.ChatID, body.Text, err)
			}
			mu.Lock()
			textSends++
			mu.Unlock()
			result = `{"message_id":91,"chat":{"id":42},"text":"Native report attached."}`
		case "sendDocument":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				return nil, err
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("document")
			if err != nil {
				return nil, err
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil || string(data) != "native report bytes" || header.Filename != "report.txt" || r.FormValue("chat_id") != "42" {
				return nil, fmt.Errorf("Telegram artifact lost its bytes, filename or destination")
			}
			mu.Lock()
			fileSends++
			mu.Unlock()
			sent <- struct{}{}
			result = `{"message_id":92,"chat":{"id":42}}`
		default:
			return nil, fmt.Errorf("unexpected Telegram method")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":` + result + `}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	step := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model    string
			Stream   bool
			Messages []struct{ Role, Content string }
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if !input.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Native report"},"finish_reason":"stop"}]}`)
			return
		}
		step++
		found := false
		for _, tool := range input.Tools {
			found = found || tool.Function.Name == "multica_upload_artifact"
		}
		if !found || input.Model != "fixture-model" {
			t.Error("Telegram claim lost file capability or fixed model")
		}
		delta, finish := map[string]any{"content": "Native report attached."}, "stop"
		if step == 1 {
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "telegram-report", "type": "function", "function": map[string]any{"name": "multica_upload_artifact", "arguments": `{"path":"report.txt"}`}}}}, "tool_calls"
		} else if step != 2 || !strings.Contains(input.Messages[len(input.Messages)-1].Content, "report.txt") {
			t.Error("Telegram artifact tool did not return its uploaded result")
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", HTTPClient: &http.Client{Transport: originalTransport}, Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.TelegramOutbound.Start(ctx)
	defer func() {
		cancel()
		if !h.TelegramOutbound.WaitWithTimeout(5 * time.Second) {
			t.Error("Telegram outbound workers did not stop")
		}
	}()
	raw, _ := json.Marshal(map[string]string{"bot_id": botID, "event_type": "message"})
	msg := channel.InboundMessage{EventID: "native-telegram-question", MessageID: "42:17", Type: channel.MsgTypeText, Text: "Create a report file.", CommandText: "Create a report file.", AddressedToBot: true, Raw: raw, Source: channel.Source{ChannelType: "telegram", ChatType: channel.ChatTypeP2P, ChatID: "42", SenderID: "sender"}}
	for range 2 {
		if err := h.ChannelRouter.Handle(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if !h.ChannelRouter.Drain(ctx) {
			t.Fatal("Telegram inbound work did not drain")
		}
	}
	var chatID string
	if err := testPool.QueryRow(ctx, "SELECT chat_session_id FROM channel_chat_session_binding WHERE installation_id=$1", installation).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"chat_session", "channel_chat_session_binding", "channel_chat_context_generation", "chat_message", "agent_task_queue"} {
		column := "chat_session_id"
		if table == "chat_session" {
			column = "id"
		}
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE "+column+"=$1", chatID)
	}
	call := func(method, path string, body any) *http.Request {
		req := dbfx.JSONRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
		return req
	}
	var claimed struct{ Tasks []daemon.Task }
	dbfx.Call(t, router.ServeHTTP, call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimed)
	if len(claimed.Tasks) != 1 || claimed.Tasks[0].ChatChannelType != "telegram" || !claimed.Tasks[0].ChatChannelDeliversFiles {
		t.Fatalf("Telegram did not claim one native file-capable run: %+v", claimed)
	}
	task := claimed.Tasks[0]
	dbfx.Call(t, router.ServeHTTP, call(http.MethodPost, "/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
	api := httptest.NewServer(router)
	defer api.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("native report bytes"), 0600); err != nil {
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
	result := <-session.Result
	if result.Status != "completed" || step != 2 {
		t.Fatalf("native Telegram artifact run failed: status=%s steps=%d", result.Status, step)
	}
	for range 2 {
		dbfx.Call(t, router.ServeHTTP, call(http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
	}
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("native artifact did not reach the fake Telegram provider")
	}
	cancel()
	if !h.TelegramOutbound.WaitWithTimeout(5 * time.Second) {
		t.Fatal("Telegram delivery did not settle")
	}
	mu.Lock()
	defer mu.Unlock()
	if textSends != 1 || fileSends != 1 || fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1", chatID) != 1 || fx.Count(t, "SELECT count(*) FROM attachment WHERE task_id=$1 AND chat_message_id IS NOT NULL", task.ID) != 1 {
		t.Fatalf("Telegram replay duplicated its run, reply or artifact: text=%d files=%d", textSends, fileSends)
	}
}
