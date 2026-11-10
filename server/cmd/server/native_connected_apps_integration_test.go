package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/handler"
	composiointeg "github.com/multica-ai/multica/server/internal/integrations/composio"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	composiosdk "github.com/multica-ai/multica/server/pkg/composio"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/remotemcp"
	"github.com/multica-ai/multica/server/pkg/remotemcp/remotemcptest"
)

// Exercise the enqueue-time overlay, production claim gate, collision
// precedence, native tool discovery, and credential boundary.
// No Composio account or external network is used.
func TestNativeConnectedAppsThroughServerRouter(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		enabled, enqueue, active, allowed, denied, boot bool
	}{
		{name: "snapshot-enabled", enabled: true},
		{name: "snapshot-disabled"},
		{name: "enqueue-owner-app", enabled: true, enqueue: true, active: true, allowed: true},
		{name: "startup-owner-app", enabled: true, enqueue: true, active: true, allowed: true, boot: true},
		{name: "enqueue-flag-disabled", enqueue: true, active: true, allowed: true},
		{name: "enqueue-revoked", enabled: true, enqueue: true, allowed: true},
		{name: "enqueue-not-allowlisted", enabled: true, enqueue: true, active: true},
		{name: "enqueue-invocation-denied", enabled: true, enqueue: true, active: true, allowed: true, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMPOSIO_API_KEY", "")
			t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
			enabled := tc.enabled
			hasOverlay := enabled && (!tc.enqueue || tc.active && tc.allowed)
			serverName := "connected_fixture"
			if tc.enqueue {
				serverName = "composio"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fixture := remotemcptest.NewServer()
			defer fixture.Close()
			endpoint := fixture.URL
			if tc.enqueue {
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("x-api-key") != remotemcptest.Credential {
						t.Error("Composio MCP lost project credential")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					r.Header.Set("Authorization", "Bearer "+remotemcptest.Credential)
					fixture.Config.Handler.ServeHTTP(w, r)
				}))
				defer proxy.Close()
				endpoint = proxy.URL
			}
			t.Setenv(remotemcp.DevOriginsEnv, endpoint)
			config := func(url, credential string) []byte {
				data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{serverName: map[string]any{"type": "http", "url": url, "headers": map[string]string{"Authorization": "Bearer " + credential}}}})
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			base := config("https://stale.invalid/mcp", "stale-fixture-credential")
			overlay := config(endpoint, remotemcptest.Credential)
			if tc.enqueue {
				overlay, _ = json.Marshal(map[string]any{"mcpServers": map[string]any{serverName: map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"x-api-key": remotemcptest.Credential}}}})
			}
			machine, daemonToken := "native-apps-"+uuid.NewString(), "mdt_"+uuid.NewString()
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			runtimeID := fx.Runtime(t, "native apps", testutil.Cols{"provider": "native", "daemon_id": machine, "visibility": "public"})
			agentCols := testutil.Cols{"mcp_config": base}
			ownerID := testUserID
			if tc.enqueue {
				ownerID = fx.User(t, "Connected app owner", "native-apps-"+uuid.NewString()+"@example.test")
				fx.Member(t, testWorkspaceID, ownerID, "member")
				agentCols["owner_id"], agentCols["permission_mode"] = ownerID, "public_to"
				if tc.denied {
					agentCols["permission_mode"] = "private"
				}
				agentCols["composio_toolkit_allowlist"] = []string{"github"}
				if !tc.allowed {
					agentCols["composio_toolkit_allowlist"] = []string{"notion"}
				}
				status := "active"
				if !tc.active {
					status = "revoked"
				}
				for _, conn := range []struct{ user, toolkit, account, status string }{
					{ownerID, "github", "ca_owner", status},
					{ownerID, "slack", "ca_unselected", "active"},
					{testUserID, "github", "ca_originator_" + uuid.NewString(), "active"},
				} {
					fx.Insert(t, "user_composio_connection", testutil.Cols{"user_id": conn.user, "toolkit_slug": conn.toolkit, "auth_config_id": "ac_fixture", "connected_account_id": conn.account, "composio_user_id": conn.user, "status": conn.status})
				}
			}
			agentID := fx.Agent(t, "native apps", runtimeID, agentCols)
			if tc.enqueue && !tc.denied {
				fx.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": agentID, "target_type": "workspace", "target_id": testWorkspaceID})
			}
			taskID := ""
			if !tc.enqueue {
				issueID := fx.Issue(t, "Native connected app write")
				taskID = fx.Task(t, agentID, testutil.Cols{
					"issue_id": issueID, "runtime_id": runtimeID, "runtime_mcp_overlay": overlay,
					"runtime_connected_apps": []byte(`[{"provider":"composio","server_name":"connected_fixture","toolkit_slug":"github","toolkit_name":"GitHub"}]`),
				})
			}
			fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
			fx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
			fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
			step := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for _, secret := range []string{remotemcptest.Credential, "stale-fixture-credential", "fixture-upstream-key", daemonToken, fixture.URL, endpoint} {
					if strings.Contains(string(raw), secret) {
						t.Error("integration credential or endpoint entered model request")
					}
				}
				var input struct {
					Model    string
					Messages []struct{ Role, Content string }
					Tools    []struct {
						Function struct{ Name, Description string }
					}
				}
				if err := json.Unmarshal(raw, &input); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if input.Model != "fixture-model" || !strings.Contains(string(raw), "## Connected Apps") || !strings.Contains(string(raw), "GitHub (`github`) via MCP server `"+serverName+"`") {
					t.Error("native model lost fixed model or connected-app context")
				}
				delta, finish := map[string]any{"content": "Connected app updated."}, "stop"
				if step == 0 {
					toolName := ""
					for _, tool := range input.Tools {
						if strings.Contains(tool.Function.Description, serverName+" / fixture.write") {
							toolName = tool.Function.Name
						}
					}
					if toolName == "" {
						t.Error("connected app write tool was not discovered")
					}
					delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "connected-write", "type": "function", "function": map[string]any{"name": toolName, "arguments": `{"value":"native-connected-write"}`}}}}, "tool_calls"
				} else if step != 1 || input.Messages[len(input.Messages)-1].Role != "tool" || !strings.Contains(input.Messages[len(input.Messages)-1].Content, "written") {
					t.Error("connected app tool result did not reach model")
				}
				step++
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
			}))
			defer upstream.Close()
			t.Setenv("MULTICA_LLM_API_KEY", "fixture-upstream-key")
			t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
			t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
			flags := featureflag.NewStaticProvider()
			flags.Set(featureflags.ComposioMCPApps, featureflag.Rule{Default: enabled})
			sessionRequests := 0
			var svc *composiointeg.Service
			var composioHTTP *http.Client
			if tc.enqueue {
				// Inject the real service and SDK at their existing seam; only the
				// upstream URL changes. OAuth is represented by persisted connection rows.
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/tool_router/session" || r.Header.Get("x-api-key") != remotemcptest.Credential {
						t.Error("Composio session lost endpoint or credential")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					var input struct {
						UserID            string `json:"user_id"`
						Toolkits          struct{ Enable []string }
						ConnectedAccounts map[string][]string `json:"connected_accounts"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.UserID != ownerID || len(input.Toolkits.Enable) != 1 || input.Toolkits.Enable[0] != "github" || len(input.ConnectedAccounts) != 1 || len(input.ConnectedAccounts["github"]) != 1 || input.ConnectedAccounts["github"][0] != "ca_owner" {
						t.Error("Composio session escaped owner allowlist or pinned account")
					}
					sessionRequests++
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"session_id": "native-session", "mcp": map[string]string{"type": "http", "url": endpoint}})
				}))
				defer provider.Close()
				if tc.boot {
					t.Setenv("COMPOSIO_API_KEY", remotemcptest.Credential)
					t.Setenv("COMPOSIO_STATE_SECRET", "fixture-state-secret")
					t.Setenv("COMPOSIO_CALLBACK_BASE_URL", "http://localhost")
					original := http.DefaultTransport
					composioHTTP = &http.Client{Transport: nativeTelegramTransport(func(r *http.Request) (*http.Response, error) {
						if r.URL.Host == strings.TrimPrefix(provider.URL, "http://") || r.URL.Host == strings.TrimPrefix(upstream.URL, "http://") || r.URL.Host == strings.TrimPrefix(endpoint, "http://") {
							return original.RoundTrip(r)
						}
						if r.URL.Host != "backend.composio.dev" || r.URL.Path != "/api/v3.1/tool_router/session" {
							return nil, fmt.Errorf("unexpected external request in connected-app fixture")
						}
						clone := r.Clone(r.Context())
						clone.URL.Scheme, clone.URL.Host, clone.URL.Path = "http", strings.TrimPrefix(provider.URL, "http://"), "/tool_router/session"
						return original.RoundTrip(clone)
					})}
				} else {
					client, err := composiosdk.NewClient(composiosdk.Options{APIKey: remotemcptest.Credential, BaseURL: provider.URL})
					if err != nil {
						t.Fatal(err)
					}
					svc, err = composiointeg.NewService(client, db.New(testPool), composiointeg.Config{StateSecret: []byte("fixture-state-secret"), CallbackBaseURL: "http://localhost"})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{
				FeatureFlags:       featureflag.NewService(flags),
				ComposioHTTPClient: composioHTTP,
				LLMCapabilities:    llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 64},
			})
			if tc.boot {
				if h.Composio == nil || h.TaskService.Composio != h.Composio {
					t.Fatal("startup did not wire the same connected-app service into handlers and enqueue")
				}
			} else if tc.enqueue {
				h.Composio, h.TaskService.Composio = svc, svc
			}
			if tc.enqueue {
				// Keep auxiliary title/suggestion work out of this task-only acceptance.
				h.TaskService.QuickActions = nil
				req := testutil.JSONRequest(http.MethodPost, "/api/issues", map[string]any{"title": "Native connected app write", "status": "todo", "assignee_type": "agent", "assignee_id": agentID})
				req.Header.Set("Authorization", "Bearer "+testToken)
				req.Header.Set("X-Workspace-ID", testWorkspaceID)
				req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
				if tc.denied {
					testutil.Call(t, router.ServeHTTP, req).Want(http.StatusForbidden)
					if sessionRequests != 0 || fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1", agentID) != 0 || len(fixture.Writes()) != 0 {
						t.Fatal("unauthorized invocation created a connected-app session or run")
					}
					return
				}
				var issue handler.IssueResponse
				testutil.Call(t, router.ServeHTTP, req).Want(http.StatusCreated).JSON(&issue)
				for _, table := range []string{"issue", "activity_log", "comment", "agent_task_queue"} {
					column := "issue_id"
					if table == "issue" {
						column = "id"
					}
					fx.Cleanup(t, "DELETE FROM "+table+" WHERE "+column+"=$1", issue.ID)
				}
				fx.QueryRow(t, "SELECT id FROM agent_task_queue WHERE issue_id=$1", issue.ID).Scan(&taskID)
				var persistedOverlay, apps []byte
				fx.QueryRow(t, "SELECT runtime_mcp_overlay,runtime_connected_apps FROM agent_task_queue WHERE id=$1", taskID).Scan(&persistedOverlay, &apps)
				var persistedApps []struct {
					ServerName string `json:"server_name"`
				}
				if len(apps) != 0 {
					if err := json.Unmarshal(apps, &persistedApps); err != nil {
						t.Fatal(err)
					}
				}
				if hasOverlay {
					if sessionRequests != 1 || !strings.Contains(string(persistedOverlay), endpoint) || len(persistedApps) != 1 || persistedApps[0].ServerName != "composio" {
						t.Fatal("enqueue did not persist the owner session before claim")
					}
				} else if sessionRequests != 0 || len(persistedOverlay) != 0 && string(persistedOverlay) != "{}" && string(persistedOverlay) != "null" || len(persistedApps) != 0 {
					t.Fatalf("gated enqueue created a session or overlay: sessions=%d overlay=%s apps=%s", sessionRequests, persistedOverlay, apps)
				}
			}

			call := func(path string, body any) *http.Request {
				req := testutil.JSONRequest(http.MethodPost, path, body)
				req.Header.Set("Authorization", "Bearer "+daemonToken)
				req.Header.Set("X-Workspace-ID", testWorkspaceID)
				req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
				return req
			}
			var claimed struct{ Tasks []daemon.Task }
			testutil.Call(t, router.ServeHTTP, call("/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimed)
			if len(claimed.Tasks) != 1 || claimed.Tasks[0].ID != taskID || claimed.Tasks[0].Agent == nil {
				t.Fatal("native connected-app task was not claimed")
			}
			task := claimed.Tasks[0]
			var actual, expected any
			if err := json.Unmarshal(task.Agent.McpConfig, &actual); err != nil {
				t.Fatal(err)
			}
			want := base
			if hasOverlay {
				want = overlay
			}
			if err := json.Unmarshal(want, &expected); err != nil {
				t.Fatal(err)
			}
			actualJSON, _ := json.Marshal(actual)
			expectedJSON, _ := json.Marshal(expected)
			if string(actualJSON) != string(expectedJSON) || len(task.ConnectedApps) != map[bool]int{true: 1, false: 0}[hasOverlay] {
				t.Fatal("claim did not preserve connected-app gate or overlay precedence")
			}
			if !hasOverlay {
				if step != 0 || len(fixture.Writes()) != 0 {
					t.Fatal("disabled apps performed work")
				}
				return
			}
			testutil.Call(t, router.ServeHTTP, call("/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
			api := httptest.NewServer(router)
			defer api.Close()
			dir := t.TempDir()
			backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir(), MCPConfig: task.Agent.McpConfig})
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
			if result.Status != "completed" || result.Output != "Connected app updated." || step != 2 {
				t.Fatalf("native connected-app run failed: %+v steps=%d", result, step)
			}
			if writes := fixture.Writes(); len(writes) != 1 || writes[0] != "native-connected-write" {
				t.Fatalf("unexpected MCP writes: %v", writes)
			}
			testutil.Call(t, router.ServeHTTP, call("/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
			var checkpoint string
			if err := testPool.QueryRow(ctx, "SELECT checkpoint::text FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID).Scan(&checkpoint); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{remotemcptest.Credential, "stale-fixture-credential", "fixture-upstream-key", daemonToken, fixture.URL, endpoint} {
				if strings.Contains(checkpoint, secret) {
					t.Error("integration secret entered durable model history")
				}
			}
		})
	}
}
