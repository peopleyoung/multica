package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func modelTestHandler(t *testing.T) *Handler {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Queries: testHandler.Queries, TxStarter: testPool, TaskService: testHandler.TaskService, ModelSecretBox: box, LLM: llm.New(llm.Config{})}
}

func modelTestBody(name, endpoint, model, key string, primary bool) map[string]any {
	return map[string]any{"name": name, "base_url": endpoint, "model": model, "api_key": key, "is_default": primary, "enabled": true, "capabilities": llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}}
}

func saveModelForTest(t *testing.T, h *Handler, body map[string]any, id string, status int) workspaceModelResponse {
	t.Helper()
	method := http.MethodPost
	if id != "" {
		method = http.MethodPut
	}
	req := withURLParam(newRequest(method, "/api/workspaces/"+testWorkspaceID+"/models", body), "id", testWorkspaceID)
	req = testutil.WithURLParams(req, "id", testWorkspaceID, "modelId", id)
	var out workspaceModelResponse
	resp := testutil.Call(t, h.SaveWorkspaceModel, req).Want(status)
	if status == 200 || status == 201 {
		resp.JSON(&out)
		if key, _ := body["api_key"].(string); key != "" && strings.Contains(resp.Body.String(), key) {
			t.Fatal("credential echoed")
		}
		if id == "" {
			dbfx.Cleanup(t, "DELETE FROM workspace_model WHERE id=$1", out.ID)
		}
	}
	return out
}

func TestWorkspaceModelsCredentialsPermissionsAndLifecycle(t *testing.T) {
	h := modelTestHandler(t)
	ctx := context.Background()
	firstBody := modelTestBody("first", "https://example.test/v1", "same-model", "fixture-key", false)
	firstCaps := firstBody["capabilities"].(llm.Capabilities)
	firstCaps.API = "responses"
	firstBody["capabilities"] = firstCaps
	first := saveModelForTest(t, h, firstBody, "", 201)
	if !first.Default || !first.HasAPIKey {
		t.Fatal("first enabled model must be the default and have a credential")
	}
	stored, err := h.Queries.GetWorkspaceModel(ctx, db.GetWorkspaceModelParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(first.ID)})
	if err != nil || bytes.Contains(stored.ApiKeyEncrypted, []byte("fixture-key")) {
		t.Fatal("credential not encrypted")
	}
	plain, err := h.ModelSecretBox.Open(stored.ApiKeyEncrypted)
	if err != nil || string(plain) != "fixture-key" {
		t.Fatal("encrypted credential cannot be read")
	}
	body := modelTestBody("renamed", "https://example.test/v1", "same-model", "", true)
	delete(body, "api_key")
	updated := saveModelForTest(t, h, body, first.ID, 200)
	if updated.Capabilities.API != "responses" {
		t.Fatal("older client erased the saved model protocol")
	}
	stored, _ = h.Queries.GetWorkspaceModel(ctx, db.GetWorkspaceModelParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(first.ID)})
	plain, _ = h.ModelSecretBox.Open(stored.ApiKeyEncrypted)
	if string(plain) != "fixture-key" {
		t.Fatal("omitted key erased credential")
	}
	req := withURLParam(newRequest("GET", "/models", nil), "id", testWorkspaceID)
	var list []workspaceModelResponse
	response := testutil.Call(t, h.ListWorkspaceModels, req).Want(200).JSON(&list)
	if len(list) != 1 || strings.Contains(response.Body.String(), "fixture-key") || strings.Contains(response.Body.String(), "encrypted") {
		t.Fatal("unsafe model listing")
	}
	for _, role := range []string{"member", "admin"} {
		userID := dbfx.User(t, "models-"+role+"@example.test", role)
		dbfx.Member(t, testWorkspaceID, userID, role)
		req := withURLParam(newRequest("POST", "/models", modelTestBody(role, "https://example.test", "m", "", false)), "id", testWorkspaceID)
		req.Header.Set("X-User-ID", userID)
		expected := 403
		if role == "admin" {
			expected = 201
		}
		out := testutil.Call(t, h.SaveWorkspaceModel, req).Want(expected)
		if expected == 201 {
			id := out.Map()["id"].(string)
			dbfx.Cleanup(t, "DELETE FROM workspace_model WHERE id=$1", id)
		}
	}
	req = withURLParam(newRequest("POST", "/models", body), "id", testWorkspaceID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", uuid.NewString())
	testutil.Call(t, h.SaveWorkspaceModel, req).Want(403)
	req = testutil.WithURLParams(newRequest("DELETE", "/models", nil), "id", testWorkspaceID, "modelId", first.ID)
	testutil.Call(t, h.DeleteWorkspaceModel, req).Want(409)
	second := saveModelForTest(t, h, modelTestBody("second", "https://example.test/v1", "same-model", "", true), "", 201)
	rt := dbfx.Runtime(t, "model lifecycle", testutil.Cols{"provider": "native"})
	dbfx.Agent(t, "model lifecycle", rt, testutil.Cols{"model": first.ID})
	testutil.Call(t, h.DeleteWorkspaceModel, req).Want(409)
	body["is_default"], body["enabled"] = false, false
	saveModelForTest(t, h, body, first.ID, 409)
	if _, err := h.workspaceModelClient(ctx, parseUUID(uuid.NewString()), second.ID); err == nil {
		t.Fatal("cross-workspace model accepted")
	}
	body["enabled"] = true
	body["api_key"] = ""
	cleared := saveModelForTest(t, h, body, first.ID, 200)
	if cleared.HasAPIKey {
		t.Fatal("explicit empty key did not clear")
	}
	unused := saveModelForTest(t, h, modelTestBody("unused", "https://example.test", "m", "", false), "", 201)
	req = testutil.WithURLParams(req, "id", testWorkspaceID, "modelId", unused.ID)
	testutil.Call(t, h.DeleteWorkspaceModel, req).Want(204)
}

// Two real daemon backends must use their assigned endpoint, model and key.
// Editing agent/model settings after configuration must not change the run.
func TestWorkspaceModelsRouteNativeRunsAndPinConfiguration(t *testing.T) {
	h := modelTestHandler(t)
	const machine = "multi-model-test"
	const token = "mdt_multi_model_fixture"
	rt := dbfx.Runtime(t, "multi model", testutil.Cols{"provider": "native", "daemon_id": machine})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine)))
		})
	})
	router.Get("/api/daemon/native/config", h.NativeModelConfig)
	router.Post("/api/daemon/tasks/{taskId}/native/inference", h.NativeInference)
	router.Post("/api/daemon/tasks/{taskId}/native/session", h.AcquireNativeSession)
	router.Post("/api/daemon/tasks/{taskId}/native/checkpoint/{operation}", h.NativeCheckpoint)
	api := httptest.NewServer(router)
	defer api.Close()
	for i := range 3 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			key, model := fmt.Sprintf("key-%d", i), fmt.Sprintf("model-%d", i)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Model != model || r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("wrong per-agent model or credential")
				}
				calls++
				if i == 2 {
					if r.URL.Path != "/responses" {
						t.Error("saved Responses protocol did not reach the model transport")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			modelBody := modelTestBody(model, upstream.URL, model, key, false)
			if i == 2 {
				caps := modelBody["capabilities"].(llm.Capabilities)
				caps.API = "responses"
				modelBody["capabilities"] = caps
			}
			conf := saveModelForTest(t, h, modelBody, "", 201)
			a := dbfx.Agent(t, "model-agent-"+fmt.Sprint(i), rt, testutil.Cols{"model": conf.ID})
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			chatID := dbfx.ChatSession(t, a)
			taskID := dbfx.Task(t, a, testutil.Cols{"chat_session_id": chatID, "runtime_id": rt, "status": "running", "attempt": 1, "dispatched_at": dispatched})
			task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: a, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token}
			dbfx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", a)
			dir := t.TempDir()
			backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBackend()
			changed := modelTestBody(model, "http://127.0.0.1:1", "wrong-model", "wrong-key", conf.Default)
			saveModelForTest(t, h, changed, conf.ID, 200)
			if _, err := testPool.Exec(context.Background(), "UPDATE agent SET model='' WHERE id=$1", a); err != nil {
				t.Fatal(err)
			}
			session, err := backend.Execute(context.Background(), "hello", agent.ExecOptions{Cwd: dir})
			if err != nil {
				t.Fatal(err)
			}
			for range session.Messages {
			}
			result := <-session.Result
			if result.Status != "completed" || result.Output != "done" || calls != 1 {
				t.Fatalf("selected model not executed: %+v calls=%d", result, calls)
			}
			row, err := h.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
			if err != nil || len(row.NativeModelConfig) == 0 || bytes.Contains(row.NativeModelConfig, []byte(key)) {
				t.Fatal("missing or plaintext run snapshot")
			}
			public, _ := json.Marshal(row)
			if bytes.Contains(public, []byte("native_model_config")) || bytes.Contains(public, []byte("credential")) {
				t.Fatal("snapshot leaked in task JSON")
			}
		})
	}
}
