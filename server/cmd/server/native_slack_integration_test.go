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
	"strconv"
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

type nativeSlackTransport func(*http.Request) (*http.Response, error)

func (f nativeSlackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeSlackThreadReplyThroughServerRouter(t *testing.T) {
	t.Setenv("MULTICA_LLM_API_KEY", "")
	t.Setenv("MULTICA_LLM_BASE_URL", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	key := bytes.Repeat([]byte{0x73}, secretbox.KeySize)
	t.Setenv("MULTICA_SLACK_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("xoxb-native-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	appID := "native-slack-" + dbid.NewV7().String()
	config, err := json.Marshal(map[string]string{"app_id": appID, "team_id": "fixture-team", "bot_user_id": "fixture-bot", "bot_token_encrypted": base64.StdEncoding.EncodeToString(encrypted)})
	if err != nil {
		t.Fatal(err)
	}
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-slack-"+dbid.NewV7().String(), "mdt_"+dbid.NewV7().String()
	runtimeID := fx.Runtime(t, "native slack", dbfx.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native slack", runtimeID)
	fx.Insert(t, "daemon_token", dbfx.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	installation := fx.Insert(t, "channel_installation", dbfx.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "slack", "config": string(config), "installer_user_id": testUserID, "status": "active"})
	fx.Insert(t, "channel_user_binding", dbfx.Cols{"installation_id": installation, "workspace_id": testWorkspaceID, "channel_type": "slack", "channel_user_id": "sender", "multica_user_id": testUserID})
	for _, table := range []string{"channel_inbound_dedup", "channel_task_delivery", "channel_outbound_message"} {
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE installation_id=$1", installation)
	}
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
	var mu sync.Mutex
	replies, reactionAdds, reactionRemoves := 0, 0, 0
	rootTimestamp := strconv.FormatInt(time.Now().Unix(), 10) + ".000001"
	messageTimestamp := strings.TrimSuffix(rootTimestamp, "1") + "2"
	originalTransport := http.DefaultTransport
	http.DefaultTransport = nativeSlackTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "127.0.0.1" || r.URL.Hostname() == "localhost" {
			return originalTransport.RoundTrip(r)
		}
		if r.URL.Host != "slack.com" {
			return nil, fmt.Errorf("unexpected provider host")
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Header.Get("Authorization") != "Bearer xoxb-native-fixture" && r.FormValue("token") != "xoxb-native-fixture" {
			return nil, fmt.Errorf("wrong Slack bot identity")
		}
		mu.Lock()
		defer mu.Unlock()
		body := `{"ok":true}`
		switch r.URL.Path {
		case "/api/reactions.add", "/api/reactions.remove":
			if r.FormValue("channel") != "fixture-channel" || r.FormValue("timestamp") != messageTimestamp {
				return nil, fmt.Errorf("reaction left source message")
			}
			if strings.HasSuffix(r.URL.Path, ".add") {
				reactionAdds++
			} else {
				reactionRemoves++
			}
		case "/api/chat.postMessage":
			if r.FormValue("channel") != "fixture-channel" || r.FormValue("thread_ts") != rootTimestamp || r.FormValue("text") != "Native threaded reply." {
				return nil, fmt.Errorf("Slack reply lost content or thread")
			}
			replies++
			body = `{"ok":true,"channel":"fixture-channel","ts":"1710000000.000003","message":{"text":"Native threaded reply."}}`
		default:
			return nil, fmt.Errorf("unexpected Slack API method %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
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
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Slack acceptance"},"finish_reason":"stop"}]}`)
			return
		}
		step++
		if input.Model != "fixture-model" {
			t.Error("Slack selected another model")
		}
		var prompt strings.Builder
		for _, message := range input.Messages {
			prompt.WriteString(message.Content)
		}
		if !strings.Contains(prompt.String(), `"channel_type":"slack"`) || !strings.Contains(prompt.String(), `"in_thread":true`) || !strings.Contains(prompt.String(), "group room; not private") {
			t.Error("native prompt lost Slack audience or thread")
		}
		for _, tool := range input.Tools {
			if tool.Function.Name == "multica_upload_artifact" {
				t.Error("text-only Slack run exposed file delivery")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Native threaded reply.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", HTTPClient: &http.Client{Transport: originalTransport}, Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, _ := json.Marshal(map[string]string{"team_id": "fixture-team", "api_app_id": appID, "event_type": "app_mention", "channel_type": "channel"})
	msg := channel.InboundMessage{EventID: "native-slack-question", MessageID: messageTimestamp, Type: channel.MsgTypeText, Text: "Reply in this thread.", CommandText: "Reply in this thread.", AddressedToBot: true, Raw: raw, Source: channel.Source{ChannelType: "slack", ChatType: channel.ChatTypeGroup, ChatID: "fixture-channel", ThreadID: rootTimestamp, SenderID: "sender"}}
	for range 2 {
		if err := h.ChannelRouter.Handle(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if !h.ChannelRouter.Drain(ctx) {
			t.Fatal("Slack ingress did not drain")
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
	call := func(path string, body any) *http.Request {
		req := dbfx.JSONRequest(http.MethodPost, path, body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
		return req
	}
	var claimed struct{ Tasks []daemon.Task }
	dbfx.Call(t, router.ServeHTTP, call("/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimed)
	if len(claimed.Tasks) != 1 || claimed.Tasks[0].ChatChannelType != "slack" || claimed.Tasks[0].ChatChannelDeliversFiles || !claimed.Tasks[0].ChatInThread {
		t.Fatal("Slack claim lost thread or delivery capability")
	}
	task := claimed.Tasks[0]
	dbfx.Call(t, router.ServeHTTP, call("/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
	api := httptest.NewServer(router)
	defer api.Close()
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
	result := <-session.Result
	if result.Status != "completed" || step != 1 {
		t.Fatalf("Slack native execution failed: %+v steps=%d", result, step)
	}
	for range 2 {
		dbfx.Call(t, router.ServeHTTP, call("/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
	}
	mu.Lock()
	defer mu.Unlock()
	if replies != 1 || reactionAdds != 1 || reactionRemoves != 1 || fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1", chatID) != 1 {
		t.Fatalf("Slack replay changed delivery: replies=%d reactions=%d/%d", replies, reactionAdds, reactionRemoves)
	}
}
