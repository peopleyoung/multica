package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"net/http/httptest"
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
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/realtime"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeFeishuThreadReplyThroughServerRouter(t *testing.T) {
	t.Setenv("MULTICA_LLM_API_KEY", "")
	t.Setenv("MULTICA_LLM_BASE_URL", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	key := bytes.Repeat([]byte{0x73}, secretbox.KeySize)
	t.Setenv("MULTICA_LARK_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("native-feishu-secret"))
	if err != nil {
		t.Fatal(err)
	}
	appID := "native-feishu-" + dbid.NewV7().String()
	config, err := json.Marshal(map[string]string{"app_id": appID, "region": "feishu", "bot_open_id": "ou_bot", "bot_union_id": "on_bot", "app_secret_encrypted": base64.StdEncoding.EncodeToString(encrypted)})
	if err != nil {
		t.Fatal(err)
	}
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-feishu-"+dbid.NewV7().String(), "mdt_"+dbid.NewV7().String()
	runtimeID := fx.Runtime(t, "native feishu", dbfx.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native feishu", runtimeID)
	fx.Insert(t, "daemon_token", dbfx.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	installation := fx.Insert(t, "channel_installation", dbfx.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "feishu", "config": string(config), "installer_user_id": testUserID, "status": "active"})
	fx.Insert(t, "channel_user_binding", dbfx.Cols{"installation_id": installation, "workspace_id": testWorkspaceID, "channel_type": "feishu", "channel_user_id": "ou_sender", "multica_user_id": testUserID})
	for _, table := range []string{"channel_inbound_dedup", "channel_task_delivery", "channel_outbound_message"} {
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE installation_id=$1", installation)
	}
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
	var mu sync.Mutex
	replies, reactionAdds, reactionRemoves := 0, 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"code":0,"data":{}}`
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["app_id"] != appID || input["app_secret"] != "native-feishu-secret" {
				t.Error("Feishu token request lost app identity")
			}
			body = `{"code":0,"tenant_access_token":"native-feishu-token","expire":7200}`
		case r.Header.Get("Authorization") != "Bearer native-feishu-token":
			t.Error("Feishu request lost tenant credential")
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/open-apis/bot/v3/info":
			body = `{"code":0,"bot":{"open_id":"ou_bot","union_id":"on_bot"}}`
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages/om_source/reactions":
			reactionAdds++
			body = `{"code":0,"data":{"reaction_id":"native-reaction"}}`
		case r.Method == http.MethodDelete && r.URL.Path == "/open-apis/im/v1/messages/om_source/reactions/native-reaction":
			reactionRemoves++
		case r.Method == http.MethodGet && r.URL.Path == "/open-apis/im/v1/messages/om_source/reactions":
			body = `{"code":0,"data":{"items":[],"has_more":false}}`
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages/om_source/reply":
			var input struct {
				MsgType  string `json:"msg_type"`
				Content  string
				InThread bool `json:"reply_in_thread"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			var content struct{ Text string }
			if err := json.Unmarshal([]byte(input.Content), &content); err != nil {
				t.Error(err)
			}
			if input.MsgType != "text" || !input.InThread || content.Text != `<at user_id="ou_sender"></at> Native threaded reply.` {
				t.Error("Feishu reply lost text, initiator mention or thread")
			}
			replies++
			body = `{"code":0,"data":{"message_id":"om_reply"}}`
		default:
			t.Errorf("unexpected Feishu HTTP method %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
		fmt.Fprint(w, body)
	}))
	defer provider.Close()
	t.Setenv("MULTICA_LARK_HTTP_BASE_URL", provider.URL)
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
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Feishu acceptance"},"finish_reason":"stop"}]}`)
			return
		}
		step++
		if input.Model != "fixture-model" {
			t.Error("Feishu selected another model")
		}
		var prompt strings.Builder
		for _, message := range input.Messages {
			prompt.WriteString(message.Content)
		}
		if !strings.Contains(prompt.String(), `"channel_type":"feishu"`) || !strings.Contains(prompt.String(), "group room; not private") {
			t.Error("native prompt lost Feishu audience")
		}
		for _, tool := range input.Tools {
			if tool.Function.Name == "multica_upload_artifact" {
				t.Error("text-only Feishu run exposed file delivery")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Native threaded reply.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, _ := json.Marshal(lark.InboundMessage{AppID: appID, EventType: "im.message.receive_v1", MessageID: "om_source", CreateTime: fmt.Sprint(time.Now().UnixMilli()), ThreadID: "omt_thread"})
	msg := channel.InboundMessage{EventID: "native-feishu-question", MessageID: "om_source", Type: channel.MsgTypeText, Text: "Reply in this thread.", CommandText: "Reply in this thread.", AddressedToBot: true, Raw: raw, Source: channel.Source{ChannelType: "feishu", ChatType: channel.ChatTypeGroup, ChatID: "fixture-channel", ThreadID: "omt_thread", SenderID: "ou_sender"}}
	for range 2 {
		if err := h.ChannelRouter.Handle(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if !h.ChannelRouter.Drain(ctx) {
			t.Fatal("Feishu ingress did not drain")
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
	if len(claimed.Tasks) != 1 || claimed.Tasks[0].ChatChannelType != "feishu" || claimed.Tasks[0].ChatChannelDeliversFiles {
		t.Fatal("Feishu claim lost delivery capability")
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
		t.Fatalf("Feishu native execution failed: %+v steps=%d", result, step)
	}
	for range 2 {
		dbfx.Call(t, router.ServeHTTP, call("/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
	}
	mu.Lock()
	defer mu.Unlock()
	if replies != 1 || reactionAdds != 1 || reactionRemoves != 1 || fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1", chatID) != 1 {
		t.Fatalf("Feishu replay changed delivery: replies=%d reactions=%d/%d", replies, reactionAdds, reactionRemoves)
	}
}
