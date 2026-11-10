package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/modelcontrol"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestWorkspaceModelConnectionCredentialsNoWriteAndAccounting(t *testing.T) {
	h := modelTestHandler(t)
	h.ModelAdmission = modelcontrol.New(testPool, modelcontrol.DefaultLimits(), nil)
	dbfx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", testWorkspaceID)
	var calls atomic.Int32
	var credential atomic.Value
	credential.Store("Bearer saved-fixture-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != credential.Load().(string) || r.URL.Path != "/responses" {
			t.Error("test ignored saved/draft credential or protocol")
		}
		var body struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "draft-model" {
			t.Error("test ignored current draft model")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"private-upstream-detail saved-fixture-key","type":"authentication_error"}}`)
	}))
	defer upstream.Close()
	model := saveModelForTest(t, h, modelTestBody("stored model", "https://saved.example.test/v1", "saved-model", "saved-fixture-key", false), "", 201)
	before, err := h.Queries.GetWorkspaceModel(context.Background(), db.GetWorkspaceModelParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(model.ID)})
	if err != nil {
		t.Fatal(err)
	}
	caps := model.Capabilities
	caps.API, caps.StreamingToolCalls = "responses", false
	body := map[string]any{"base_url": upstream.URL, "model": "draft-model", "capabilities": caps}
	for _, tc := range []struct {
		name, key, expected string
		includeKey          bool
	}{
		{"saved secret", "", "Bearer saved-fixture-key", false},
		{"replacement", "replacement-fixture-key", "Bearer replacement-fixture-key", true},
		{"clear secret", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential.Store(tc.expected)
			if tc.includeKey {
				body["api_key"] = tc.key
			} else {
				delete(body, "api_key")
			}
			req := testutil.WithURLParams(newRequest("POST", "/models/test", body), "id", testWorkspaceID, "modelId", model.ID)
			var result llm.ConnectionTest
			response := testutil.Call(t, h.TestWorkspaceModel, req).Want(200).JSON(&result)
			if result.OK || result.Code != "model_authentication_failed" || result.Stage != "text" || strings.Contains(response.Body.String(), "fixture-key") || strings.Contains(response.Body.String(), "private-upstream-detail") {
				t.Fatalf("unsafe failure result: %+v", result)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("diagnostic response may be cached")
			}
		})
	}
	if calls.Load() != 3 {
		t.Fatalf("diagnostic retried requests: %d", calls.Load())
	}
	after, err := h.Queries.GetWorkspaceModel(context.Background(), db.GetWorkspaceModelParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(model.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if before.Name != after.Name || before.BaseUrl != after.BaseUrl || before.UpstreamModel != after.UpstreamModel || !bytes.Equal(before.Capabilities, after.Capabilities) || !bytes.Equal(before.ApiKeyEncrypted, after.ApiKeyEncrypted) {
		t.Fatal("test saved draft configuration or credential")
	}
	var accounted int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM model_call WHERE workspace_id=$1 AND purpose='connection_test' AND task_id IS NULL AND finished_at IS NOT NULL AND outcome='failed' AND http_status=401`, parseUUID(testWorkspaceID)).Scan(&accounted); err != nil || accounted != 3 {
		t.Fatalf("diagnostic calls bypassed shared ledger: count=%d error=%v", accounted, err)
	}
	// New drafts have the same transport without creating a model row.
	credential.Store("Bearer draft-fixture-key")
	body["api_key"] = "draft-fixture-key"
	req := testutil.WithURLParams(newRequest("POST", "/models/test", body), "id", testWorkspaceID)
	testutil.Call(t, h.TestWorkspaceModel, req).Want(200)
	models, err := h.Queries.ListWorkspaceModels(context.Background(), parseUUID(testWorkspaceID))
	if err != nil || len(models) != 1 {
		t.Fatal("draft test created a model")
	}
}

func TestWorkspaceModelConnectionAuthorizationAndValidation(t *testing.T) {
	h := modelTestHandler(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := saveModelForTest(t, h, modelTestBody("saved", server.URL, "m", "fixture-key", false), "", 201)
	body := map[string]any{"base_url": server.URL, "model": "m", "capabilities": model.Capabilities}
	request := func() *http.Request {
		return testutil.WithURLParams(newRequest("POST", "/models/test", body), "id", testWorkspaceID, "modelId", model.ID)
	}
	member := dbfx.User(t, "connection-member@example.test", "Member")
	dbfx.Member(t, testWorkspaceID, member, "member")
	req := request()
	req.Header.Set("X-User-ID", member)
	testutil.Call(t, h.TestWorkspaceModel, req).Want(403)
	req = request()
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", uuid.NewString())
	testutil.Call(t, h.TestWorkspaceModel, req).Want(403)
	other := dbfx.Workspace(t, "Other", "model-connection-"+uuid.NewString())
	foreign := uuid.NewString()
	dbfx.Insert(t, "workspace_model", testutil.Cols{"id": foreign, "workspace_id": other, "name": "foreign", "base_url": server.URL, "upstream_model": "m", "capabilities": json.RawMessage(`{}`)})
	req = testutil.WithURLParams(request(), "id", testWorkspaceID, "modelId", foreign)
	testutil.Call(t, h.TestWorkspaceModel, req).Want(404)
	req = testutil.WithURLParams(request(), "id", testWorkspaceID, "modelId", "bad-id")
	testutil.Call(t, h.TestWorkspaceModel, req).Want(400)
	body["base_url"] = "https://secret:credential@example.test"
	testutil.Call(t, h.TestWorkspaceModel, request()).Want(400)
	body["base_url"], body["unknown"] = server.URL, true
	testutil.Call(t, h.TestWorkspaceModel, request()).Want(400)
	if calls.Load() != 0 {
		t.Fatal("unauthorized or invalid probe reached upstream")
	}
}

func TestWorkspaceModelConnectionSharesAdmissionScope(t *testing.T) {
	controller := modelcontrol.New(testPool, modelcontrol.DefaultLimits(), nil)
	dbfx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", testWorkspaceID)
	call := llm.Call{Scope: llm.CallScope{WorkspaceID: testWorkspaceID, Purpose: "connection_test"}, Model: "fixture", ReservedTokens: 100}
	finish, err := controller.Acquire(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	defer finish(context.Background(), llm.CallResult{Outcome: "cancelled"})
	h := modelTestHandler(t)
	h.ModelAdmission = controller
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}
	body := map[string]any{"base_url": server.URL, "model": "m", "capabilities": caps}
	req := testutil.WithURLParams(newRequest("POST", "/models/test", body), "id", testWorkspaceID)
	var result llm.ConnectionTest
	testutil.Call(t, h.TestWorkspaceModel, req).Want(200).JSON(&result)
	if result.OK || result.Code != "model_quota_exceeded" || requests.Load() != 0 {
		t.Fatalf("probe bypassed quota: %+v", result)
	}
}
