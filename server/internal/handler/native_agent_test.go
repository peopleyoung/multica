package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeRuntimeConfigurationHealth(t *testing.T) {
	runtime := db.AgentRuntime{Provider: "native"}
	missing := (&Handler{}).runtimeResponse(runtime)
	metadata := missing.Metadata.(map[string]any)
	if metadata["native_model_configured"] != false || metadata["native_model"] != nil {
		t.Fatalf("unconfigured runtime reports a model: %+v", metadata)
	}
	client := llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}})
	configured := (&Handler{LLM: client}).runtimeResponse(runtime).Metadata.(map[string]any)
	if configured["native_model_configured"] != true || configured["native_model"] != "fixture-model" {
		t.Fatalf("configured runtime lost its fixed model: %+v", configured)
	}
}

func TestNativeAgentModelCatalogAndOverrideValidation(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "native catalog", testutil.Cols{"provider": "native"})
	h := &Handler{Queries: testHandler.Queries, LLM: llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}})}
	r := testutil.WithURLParams(newRequest(http.MethodPost, "/api/runtimes/"+runtimeID+"/models", nil), "runtimeId", runtimeID)
	var catalog ModelListRequest
	testutil.Call(t, h.InitiateListModels, r).Want(http.StatusOK).JSON(&catalog)
	if catalog.Status != ModelListCompleted || catalog.Supported || len(catalog.Models) != 1 || catalog.Models[0].ID != "fixture-model" || !catalog.Models[0].Default {
		t.Fatalf("native catalog permits model selection: %+v", catalog)
	}
	// Validation runs before any insert or execution. Installed clients that
	// still expose manual entry get a recoverable error instead of silent loss.
	testutil.Call(t, h.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{
		"name": "native override", "runtime_id": runtimeID, "model": "another-model",
	})).Want(http.StatusBadRequest)
	agentID := dbfx.Agent(t, "native override", runtimeID)
	testutil.Call(t, h.UpdateAgent, testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+agentID, map[string]any{"model": "another-model"}), "id", agentID)).Want(http.StatusBadRequest)
	testutil.Call(t, h.CreateAgentBuilderSession, newRequest(http.MethodPost, "/api/agent-builder/sessions", map[string]any{
		"runtime_id": runtimeID, "model": "another-model",
	})).Want(http.StatusBadRequest)
	testutil.Call(t, h.CreateMikaAgent, withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/agents/mika", map[string]any{
		"runtime_id": runtimeID, "language": "en", "model": "another-model",
	}))).Want(http.StatusBadRequest)
}
