package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// TestWorkspaceModel probes draft settings without persisting configuration.
// An omitted key reuses the saved secret; an explicit empty key tests keyless access.
func (h *Handler) TestWorkspaceModel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workspace := chi.URLParam(r, "id")
	if !h.requireModelWriter(w, r, workspace) {
		return
	}
	var req struct {
		BaseURL      string           `json:"base_url"`
		Model        string           `json:"model"`
		APIKey       *string          `json:"api_key"`
		Capabilities llm.Capabilities `json:"capabilities"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid model test configuration")
		return
	}
	key := ""
	if selection := chi.URLParam(r, "modelId"); selection != "" {
		id, ok := parseUUIDOrBadRequest(w, selection, "model_id")
		if !ok {
			return
		}
		row, err := h.Queries.GetWorkspaceModel(r.Context(), db.GetWorkspaceModelParams{WorkspaceID: parseUUID(workspace), ID: id})
		if err != nil {
			writeError(w, 404, "model not found")
			return
		}
		if req.APIKey == nil && len(row.ApiKeyEncrypted) > 0 {
			if h.ModelSecretBox == nil {
				writeError(w, 503, "model credential encryption is not configured")
				return
			}
			plain, err := h.ModelSecretBox.Open(row.ApiKeyEncrypted)
			if err != nil {
				writeError(w, 503, "cannot read model credential")
				return
			}
			key = string(plain)
		}
	}
	if req.APIKey != nil {
		key = *req.APIKey
	}
	// A probe must be possible before tool support has been verified. This
	// temporary capability never changes the saved checkbox or model config.
	req.Capabilities.StreamingToolCalls = true
	retries, _ := llm.Retries(0)
	config := llm.Config{BaseURL: strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"), DefaultModel: strings.TrimSpace(req.Model), APIKey: key, Capabilities: req.Capabilities, Admission: h.ModelAdmission, MaxRetries: retries}
	if config.BaseURL == "" || len(config.BaseURL) > 2048 || config.DefaultModel == "" || len(config.DefaultModel) > 200 || len(key) > 8192 || config.Validate() != nil {
		writeError(w, 400, "invalid model endpoint or capabilities")
		return
	}
	ctx := llm.WithCallScope(r.Context(), llm.CallScope{WorkspaceID: workspace, Purpose: "connection_test"})
	// All attempts share the workspace quota and ledger with real executions.
	result := llm.New(config).TestConnection(ctx)
	if ctx.Err() == context.Canceled {
		return
	}
	writeJSON(w, 200, result)
}
