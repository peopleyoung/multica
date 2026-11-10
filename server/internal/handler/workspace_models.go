package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Model IDs are workspace-owned configuration identities, not upstream names.
// Only this resolver decrypts credentials for authorized server-side calls.
func (h *Handler) workspaceModelRow(ctx context.Context, workspace pgtype.UUID, selection string) (*db.WorkspaceModel, error) {
	var row db.WorkspaceModel
	var err error
	if selection == "" {
		row, err = h.Queries.GetDefaultWorkspaceModel(ctx, workspace)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
	} else if selection == h.LLM.DefaultModel() {
		return nil, nil
	} else {
		id, parseErr := uuid.Parse(selection)
		if parseErr != nil {
			return nil, errors.New("selected model is unavailable")
		}
		row, err = h.Queries.GetWorkspaceModel(ctx, db.GetWorkspaceModelParams{WorkspaceID: workspace, ID: pgtype.UUID{Bytes: id, Valid: true}})
	}
	if err != nil || !row.Enabled {
		return nil, errors.New("selected model is unavailable")
	}
	return &row, nil
}

func (h *Handler) workspaceModelClient(ctx context.Context, workspace pgtype.UUID, selection string) (*llm.Client, error) {
	row, err := h.workspaceModelRow(ctx, workspace, selection)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return h.LLM, nil
	}
	return h.modelRowClient(*row)
}

func (h *Handler) modelRowClient(row db.WorkspaceModel) (*llm.Client, error) {
	var caps llm.Capabilities
	if json.Unmarshal(row.Capabilities, &caps) != nil {
		return nil, errors.New("invalid model capabilities")
	}
	key := ""
	if len(row.ApiKeyEncrypted) > 0 {
		if h.ModelSecretBox == nil {
			return nil, errors.New("model encryption is not configured")
		}
		raw, err := h.ModelSecretBox.Open(row.ApiKeyEncrypted)
		if err != nil {
			return nil, errors.New("cannot read model credential")
		}
		key = string(raw)
	}
	return llm.New(llm.Config{BaseURL: row.BaseUrl, APIKey: key, DefaultModel: row.UpstreamModel, Capabilities: caps, Admission: h.ModelAdmission, MaxRetries: h.ModelMaxRetries}), nil
}

// The task snapshot contains ciphertext only. It never appears in task DTOs.
// Concurrent initial requests all use the first snapshot committed by SQL.
type taskModelSnapshot struct {
	Model      db.WorkspaceModel `json:"model"`
	Credential []byte            `json:"credential"`
}

func (h *Handler) nativeTaskModel(ctx context.Context, task db.AgentTaskQueue) (*llm.Client, error) {
	current, err := h.Queries.GetAgentTask(ctx, task.ID)
	if err != nil {
		return nil, errors.New("run is unavailable")
	}
	raw := current.NativeModelConfig
	if len(raw) == 0 {
		agent, err := h.Queries.GetAgent(ctx, current.AgentID)
		if err != nil {
			return nil, errors.New("agent is unavailable")
		}
		row, err := h.workspaceModelRow(ctx, agent.WorkspaceID, agent.Model.String)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return h.LLM, nil
		}
		raw, err = json.Marshal(taskModelSnapshot{Model: *row, Credential: row.ApiKeyEncrypted})
		if err != nil {
			return nil, err
		}
		raw, err = h.Queries.PinTaskNativeModel(ctx, db.PinTaskNativeModelParams{ID: current.ID, AgentID: current.AgentID, Config: raw})
		if err != nil {
			return nil, errors.New("cannot pin run model")
		}
	}
	var snapshot taskModelSnapshot
	if json.Unmarshal(raw, &snapshot) != nil {
		return nil, errors.New("invalid run model")
	}
	snapshot.Model.ApiKeyEncrypted = snapshot.Credential
	return h.modelRowClient(snapshot.Model)
}

type workspaceModelResponse struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	BaseURL      string           `json:"base_url"`
	Model        string           `json:"model"`
	HasAPIKey    bool             `json:"has_api_key"`
	Capabilities llm.Capabilities `json:"capabilities"`
	Default      bool             `json:"is_default"`
	Enabled      bool             `json:"enabled"`
}

func modelResponse(row db.WorkspaceModel) workspaceModelResponse {
	var caps llm.Capabilities
	_ = json.Unmarshal(row.Capabilities, &caps)
	return workspaceModelResponse{ID: uuidToString(row.ID), Name: row.Name, BaseURL: row.BaseUrl, Model: row.UpstreamModel, HasAPIKey: len(row.ApiKeyEncrypted) > 0, Capabilities: caps, Default: row.IsDefault, Enabled: row.Enabled}
}

func (h *Handler) ListWorkspaceModels(w http.ResponseWriter, r *http.Request) {
	workspace := chi.URLParam(r, "id")
	if _, ok := h.workspaceMember(w, r, workspace); !ok {
		return
	}
	rows, err := h.Queries.ListWorkspaceModels(r.Context(), parseUUID(workspace))
	if err != nil {
		writeError(w, 500, "cannot list models")
		return
	}
	out := make([]workspaceModelResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, modelResponse(row))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

// Human administrators alone manage credentials; agent actors cannot turn
// workspace configuration into an arbitrary outbound credentialed request.
func (h *Handler) requireModelWriter(w http.ResponseWriter, r *http.Request, workspace string) bool {
	member, ok := h.workspaceMember(w, r, workspace)
	if !ok {
		return false
	}
	actor, _ := h.resolveActor(r, requestUserID(r), workspace)
	if actor == "agent" || !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, 403, "only workspace administrators can manage models")
		return false
	}
	return true
}

func (h *Handler) SaveWorkspaceModel(w http.ResponseWriter, r *http.Request) {
	workspace := chi.URLParam(r, "id")
	if !h.requireModelWriter(w, r, workspace) {
		return
	}
	if h.ModelSecretBox == nil {
		writeError(w, 503, "model credential encryption is not configured")
		return
	}
	var req struct {
		Name         string           `json:"name"`
		BaseURL      string           `json:"base_url"`
		Model        string           `json:"model"`
		APIKey       *string          `json:"api_key"`
		Capabilities llm.Capabilities `json:"capabilities"`
		Default      bool             `json:"is_default"`
		Enabled      bool             `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid model configuration")
		return
	}
	req.Name, req.BaseURL, req.Model = strings.TrimSpace(req.Name), strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"), strings.TrimSpace(req.Model)
	if req.Name == "" || len(req.Name) > 100 || req.BaseURL == "" || req.Model == "" || len(req.Model) > 200 || len(req.BaseURL) > 2048 {
		writeError(w, 400, "name, API base URL and model are required")
		return
	}
	config := llm.Config{BaseURL: req.BaseURL, DefaultModel: req.Model, Capabilities: req.Capabilities}
	if req.APIKey != nil {
		config.APIKey = *req.APIKey
		if len(*req.APIKey) > 8192 {
			writeError(w, 400, "API key is too long")
			return
		}
	}
	if config.Validate() != nil || req.Capabilities.Validate() != nil {
		writeError(w, 400, "invalid model endpoint or capabilities")
		return
	}
	if req.Default && !req.Enabled {
		writeError(w, 400, "the default model must be enabled")
		return
	}
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	existingID := chi.URLParam(r, "modelId")
	if existingID != "" {
		var ok bool
		id, ok = parseUUIDOrBadRequest(w, existingID, "model_id")
		if !ok {
			return
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "cannot save model")
		return
	}
	defer tx.Rollback(context.Background())
	q := h.Queries.WithTx(tx)
	// Serialize changes to the default and assignment/deletion decisions.
	var locked string
	if tx.QueryRow(r.Context(), `SELECT id::text FROM workspace WHERE id=$1 FOR UPDATE`, parseUUID(workspace)).Scan(&locked) != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	rows, err := q.ListWorkspaceModels(r.Context(), parseUUID(workspace))
	if err != nil {
		writeError(w, 500, "cannot save model")
		return
	}
	var encrypted []byte
	if existingID != "" {
		old, err := q.GetWorkspaceModel(r.Context(), db.GetWorkspaceModelParams{WorkspaceID: parseUUID(workspace), ID: id})
		if err != nil {
			writeError(w, 404, "model not found")
			return
		}
		encrypted = old.ApiKeyEncrypted
		// Installed clients may not know the model's API selector. Omission
		// preserves it; selecting chat_completions explicitly resets it.
		if req.Capabilities.API == "" {
			var previous llm.Capabilities
			if json.Unmarshal(old.Capabilities, &previous) != nil {
				writeError(w, 500, "cannot read model capabilities")
				return
			}
			req.Capabilities.API = previous.API
		}
		if old.IsDefault && !req.Default {
			writeError(w, 409, "select another default model first")
			return
		}
		assigned, err := q.WorkspaceModelAssigned(r.Context(), db.WorkspaceModelAssignedParams{WorkspaceID: parseUUID(workspace), Model: pgtype.Text{String: existingID, Valid: true}})
		if err != nil {
			writeError(w, 500, "cannot validate model assignments")
			return
		}
		if assigned && !req.Enabled {
			writeError(w, 409, "reassign agents before disabling this model")
			return
		}
	}
	if len(rows) == 0 {
		req.Default = true
		if !req.Enabled {
			writeError(w, 400, "the first model must be enabled")
			return
		}
	}
	if req.APIKey != nil {
		encrypted = []byte{}
		if *req.APIKey != "" {
			encrypted, err = h.ModelSecretBox.Seal([]byte(*req.APIKey))
			if err != nil {
				writeError(w, 500, "cannot encrypt model credential")
				return
			}
		}
	}
	if encrypted == nil {
		encrypted = []byte{}
	}
	if req.Default {
		if q.ClearDefaultWorkspaceModel(r.Context(), parseUUID(workspace)) != nil {
			writeError(w, 500, "cannot save default model")
			return
		}
	}
	caps, _ := json.Marshal(req.Capabilities)
	row, err := q.PutWorkspaceModel(r.Context(), db.PutWorkspaceModelParams{ID: id, WorkspaceID: parseUUID(workspace), Name: req.Name, BaseUrl: req.BaseURL, UpstreamModel: req.Model, ApiKeyEncrypted: encrypted, Capabilities: caps, IsDefault: req.Default, Enabled: req.Enabled})
	if err != nil || tx.Commit(r.Context()) != nil {
		writeError(w, 500, "cannot save model")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if existingID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, modelResponse(row))
}

func (h *Handler) DeleteWorkspaceModel(w http.ResponseWriter, r *http.Request) {
	workspace := chi.URLParam(r, "id")
	if !h.requireModelWriter(w, r, workspace) {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "modelId"), "model_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "cannot delete model")
		return
	}
	defer tx.Rollback(context.Background())
	q := h.Queries.WithTx(tx)
	var locked string
	if tx.QueryRow(r.Context(), `SELECT id::text FROM workspace WHERE id=$1 FOR UPDATE`, parseUUID(workspace)).Scan(&locked) != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	row, err := q.GetWorkspaceModel(r.Context(), db.GetWorkspaceModelParams{WorkspaceID: parseUUID(workspace), ID: id})
	if err != nil {
		writeError(w, 404, "model not found")
		return
	}
	assigned, err := q.WorkspaceModelAssigned(r.Context(), db.WorkspaceModelAssignedParams{WorkspaceID: parseUUID(workspace), Model: pgtype.Text{String: uuidToString(id), Valid: true}})
	if err != nil {
		writeError(w, 500, "cannot validate model assignments")
		return
	}
	if assigned || row.IsDefault {
		writeError(w, 409, "reassign agents and select another default model before deleting")
		return
	}
	if _, err = q.DeleteWorkspaceModel(r.Context(), db.DeleteWorkspaceModelParams{WorkspaceID: parseUUID(workspace), ID: id}); err != nil || tx.Commit(r.Context()) != nil {
		writeError(w, 500, "cannot delete model")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
