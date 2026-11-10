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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/wecom"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/realtime"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/llm"
	dto "github.com/prometheus/client_model/go"
)

// Exercise production ingress and outbound wiring over a local aibot socket.
func TestNativeWecomArtifactDeliveryThroughServerRouter(t *testing.T) {
	t.Setenv("MULTICA_LLM_API_KEY", "")
	t.Setenv("MULTICA_LLM_BASE_URL", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "")
	key := bytes.Repeat([]byte{0x84}, secretbox.KeySize)
	t.Setenv("MULTICA_WECOM_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("fixture-secret"))
	if err != nil {
		t.Fatal(err)
	}
	botID := "native-wecom-" + dbid.NewV7().String()
	config, err := json.Marshal(map[string]string{"app_id": botID, "bot_id": botID, "secret_encrypted": base64.StdEncoding.EncodeToString(encrypted)})
	if err != nil {
		t.Fatal(err)
	}
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-wecom-"+dbid.NewV7().String(), "mdt_"+dbid.NewV7().String()
	runtimeID := fx.Runtime(t, "native wecom", dbfx.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native wecom", runtimeID)
	fx.Insert(t, "daemon_token", dbfx.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	installation := fx.Insert(t, "channel_installation", dbfx.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "wecom", "config": string(config), "installer_user_id": testUserID, "status": "active"})
	fx.Insert(t, "channel_user_binding", dbfx.Cols{"installation_id": installation, "workspace_id": testWorkspaceID, "channel_type": "wecom", "channel_user_id": "sender", "multica_user_id": testUserID})
	for _, table := range []string{"channel_inbound_dedup", "channel_task_delivery", "channel_outbound_message"} {
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE installation_id=$1", installation)
	}
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM attachment WHERE uploader_id=$1", agentID)

	var mu sync.Mutex
	openings, replies, uploads, chunks, finishes, fileSends := 0, 0, 0, 0, 0, 0
	streamID := ""
	socketStopped := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(socketStopped)
		upgrader := websocket.Upgrader{}
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer socket.Close()
		for {
			var frame struct {
				Cmd     string
				Headers struct {
					ReqID string `json:"req_id"`
				}
				Body struct {
					BotID          string `json:"bot_id"`
					Secret         string
					Type, Filename string
					TotalSize      int    `json:"total_size"`
					TotalChunks    int    `json:"total_chunks"`
					UploadID       string `json:"upload_id"`
					ChunkIndex     int    `json:"chunk_index"`
					Data           string `json:"base64_data"`
					ChatID         string `json:"chatid"`
					ChatType       int    `json:"chat_type"`
					MsgType        string `json:"msgtype"`
					Stream         struct {
						ID, Content string
						Finish      bool
					}
					File struct {
						MediaID string `json:"media_id"`
					}
				}
			}
			if err := socket.ReadJSON(&frame); err != nil {
				return
			}
			ackBody := map[string]any{}
			mu.Lock()
			switch frame.Cmd {
			case "aibot_subscribe":
				if frame.Body.BotID != botID || frame.Body.Secret != "fixture-secret" {
					t.Error("WeCom subscribe lost credentials")
				}
			case "aibot_respond_msg":
				if frame.Headers.ReqID != "native-source-req" || frame.Body.MsgType != "stream" || frame.Body.Stream.ID == "" {
					t.Error("WeCom reply lost its source stream")
				}
				if frame.Body.Stream.Finish {
					replies++
					if frame.Body.Stream.ID != streamID || frame.Body.Stream.Content != "Native report attached." {
						t.Error("WeCom final reply did not close the original bubble")
					}
				} else {
					openings++
					streamID = frame.Body.Stream.ID
				}
			case "aibot_upload_media_init":
				uploads++
				if frame.Body.Type != "file" || frame.Body.Filename != "report.txt" || frame.Body.TotalSize != len("native report bytes") || frame.Body.TotalChunks != 1 {
					t.Error("WeCom upload lost file metadata")
				}
				ackBody["upload_id"] = "native-upload"
			case "aibot_upload_media_chunk":
				chunks++
				data, err := base64.StdEncoding.DecodeString(frame.Body.Data)
				if err != nil || string(data) != "native report bytes" || frame.Body.UploadID != "native-upload" || frame.Body.ChunkIndex != 0 {
					t.Error("WeCom upload lost file bytes")
				}
			case "aibot_upload_media_finish":
				finishes++
				if frame.Body.UploadID != "native-upload" {
					t.Error("WeCom finished another upload")
				}
				ackBody["media_id"] = "native-media"
			case "aibot_send_msg":
				fileSends++
				if replies != 1 || finishes != 1 || frame.Body.ChatID != "fixture-chat" || frame.Body.ChatType != 1 || frame.Body.MsgType != "file" || frame.Body.File.MediaID != "native-media" {
					t.Errorf("WeCom file lost destination, media identity or reply ordering: replies=%d finishes=%d body=%+v", replies, finishes, frame.Body)
				}
			default:
				t.Errorf("unexpected WeCom command %q", frame.Cmd)
			}
			mu.Unlock()
			if err := socket.WriteJSON(map[string]any{"headers": map[string]string{"req_id": frame.Headers.ReqID}, "errcode": 0, "body": ackBody}); err != nil {
				return
			}
			if frame.Cmd == "aibot_subscribe" {
				callback := map[string]any{"cmd": "aibot_msg_callback", "headers": map[string]string{"req_id": "native-source-req"}, "body": map[string]any{"msgid": "native-source", "aibotid": botID, "chatid": "fixture-chat", "chattype": "single", "from": map[string]string{"userid": "sender"}, "msgtype": "text", "text": map[string]string{"content": "Create a report file."}}}
				for range 2 {
					if err := socket.WriteJSON(callback); err != nil {
						return
					}
				}
			}
		}
	}))
	defer provider.Close()

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
			t.Error("Wecom claim lost file capability or fixed model")
		}
		delta, finish := map[string]any{"content": "Native report attached."}, "stop"
		if step == 1 {
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "wecom-report", "type": "function", "function": map[string]any{"name": "multica_upload_artifact", "arguments": `{"path":"report.txt"}`}}}}, "tool_calls"
		} else if step != 2 || !strings.Contains(input.Messages[len(input.Messages)-1].Content, "report.txt") {
			t.Error("Wecom artifact tool did not return its uploaded result")
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	senders := wecom.NewSendersRegistry()
	deliveryMetrics := metrics.NewWecomMetrics()
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{WecomSenders: senders, WecomMetrics: deliveryMetrics})
	h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registry := channel.NewRegistry()
	wecom.RegisterWecom(registry, wecom.ChannelDeps{Credentials: h.WecomCredentials, Senders: senders, Dedup: wecom.NewDeduper(h.WecomStore), WSURL: "ws" + strings.TrimPrefix(provider.URL, "http")})
	handled := make(chan error, 2)
	connection, err := registry.Build(channel.Config{Type: wecom.TypeWecom, ID: parseUUID(installation), Raw: config, Handler: func(ctx context.Context, msg channel.InboundMessage) error {
		err := h.ChannelRouter.Handle(ctx, msg)
		handled <- err
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- connection.Connect(ctx) }()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("WeCom socket did not stop")
		}
		select {
		case <-socketStopped:
		case <-time.After(5 * time.Second):
			t.Error("WeCom provider did not stop")
		}
	}()
	for range 2 {
		select {
		case err := <-handled:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("WeCom callback did not enter production router")
		}
	}
	if !h.ChannelRouter.Drain(ctx) {
		t.Fatal("WeCom ingress did not drain")
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
	if len(claimed.Tasks) != 1 || claimed.Tasks[0].ChatChannelType != "wecom" || !claimed.Tasks[0].ChatChannelDeliversFiles {
		t.Fatalf("Wecom did not claim one native file-capable run: %+v", claimed)
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
		t.Fatalf("native Wecom artifact run failed: status=%s steps=%d", result.Status, step)
	}
	for range 2 {
		dbfx.Call(t, router.ServeHTTP, call(http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
	}
	// Wait for the subscriber to consume the provider ACK before tearing down.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var counter dto.Metric
		if err := deliveryMetrics.AttachmentDelivered.Write(&counter); err != nil {
			t.Fatal(err)
		}
		if counter.GetCounter().GetValue() == 1 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("WeCom did not acknowledge artifact delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if openings != 1 || replies != 1 || uploads != 1 || chunks != 1 || finishes != 1 || fileSends != 1 || fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1", chatID) != 1 || fx.Count(t, "SELECT count(*) FROM attachment WHERE task_id=$1 AND chat_message_id IS NOT NULL", task.ID) != 1 {
		t.Fatalf("WeCom replay changed run or delivery: openings=%d replies=%d uploads=%d chunks=%d finishes=%d files=%d", openings, replies, uploads, chunks, finishes, fileSends)
	}
}
