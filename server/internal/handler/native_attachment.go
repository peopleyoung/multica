package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// nativeAttachment always reads the storage object through the API. No signed
// URL, redirect, raw path, or storage credential enters a model request.
func (h *Handler) nativeAttachment(r *http.Request, task db.AgentTaskQueue, id string) (db.Attachment, []byte, error) {
	uuid, err := util.ParseUUID(id)
	if err != nil {
		return db.Attachment{}, nil, errors.New("invalid attachment identity")
	}
	workspace, err := h.TaskService.ResolveTaskWorkspaceIDChecked(r.Context(), task)
	if err != nil {
		return db.Attachment{}, nil, err
	}
	att, err := h.Queries.GetAttachment(r.Context(), db.GetAttachmentParams{ID: uuid, WorkspaceID: parseUUID(workspace)})
	if err != nil || h.Storage == nil {
		return db.Attachment{}, nil, errors.New("attachment is unavailable")
	}
	if !h.nativeAttachmentInScope(r, task, att) {
		return db.Attachment{}, nil, errors.New("attachment is outside this run's conversation")
	}
	if att.SizeBytes > 1<<20 {
		return db.Attachment{}, nil, errors.New("native attachment exceeds 1 MiB")
	}
	reader, err := h.Storage.GetReader(r.Context(), h.Storage.KeyFromURL(att.Url))
	if err != nil {
		return db.Attachment{}, nil, errors.New("attachment object unavailable")
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return db.Attachment{}, nil, errors.New("native attachment exceeds read limit")
	}
	return att, data, nil
}

func (h *Handler) nativeAttachmentInScope(r *http.Request, task db.AgentTaskQueue, att db.Attachment) bool {
	if task.IssueID.Valid && att.IssueID == task.IssueID || task.ChatSessionID.Valid && att.ChatSessionID == task.ChatSessionID {
		return true
	}
	var captured db.IssueSourceContext
	if att.SourceContextID.Valid {
		var err error
		captured, err = h.Queries.GetIssueSourceContextByID(r.Context(), db.GetIssueSourceContextByIDParams{WorkspaceID: att.WorkspaceID, ID: att.SourceContextID})
		if err != nil || captured.State == "abandoned" {
			return false
		}
		if task.IssueID.Valid && captured.State == "attached" && captured.IssueID == task.IssueID {
			return true
		}
	}
	if task.IssueID.Valid || task.ChatSessionID.Valid {
		return false
	}
	var quick service.QuickCreateContext
	if json.Unmarshal(task.Context, &quick) != nil || quick.Type != service.QuickCreateContextType || quick.WorkspaceID != uuidToString(att.WorkspaceID) || !task.OriginatorUserID.Valid || quick.RequesterID != uuidToString(task.OriginatorUserID) {
		return false
	}
	inputID, err := h.nativeInputTaskID(r.Context(), task)
	if err != nil {
		return false
	}
	if att.SourceContextID.Valid {
		return quick.SourceContextID == uuidToString(captured.ID) && captured.CapturedByUserID == task.OriginatorUserID &&
			(uuidToString(captured.OriginTaskID) == inputID || captured.OriginTaskID == task.ID)
	}
	if !slices.Contains(quick.AttachmentIDs, uuidToString(att.ID)) || att.UploaderType != "member" || att.UploaderID != task.OriginatorUserID || att.ChatSessionID.Valid || att.CommentID.Valid {
		return false
	}
	if !att.IssueID.Valid {
		return true
	}
	// Selected uploads remain readable after this input creates its issue, but
	// moving an upload to another issue must not widen the run's authority.
	issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: att.IssueID, WorkspaceID: att.WorkspaceID})
	return err == nil && issue.OriginType.String == "quick_create" &&
		(issue.OriginID == task.ID || uuidToString(issue.OriginID) == inputID)
}

func (h *Handler) hydrateNativeAttachments(r *http.Request, taskID string, req llm.NativeRequest) (llm.Request, error) {
	task, err := h.loadNativeInferenceRun(r, taskID, req)
	if err != nil {
		return llm.Request{}, err
	}
	client, err := h.nativeTaskModel(r.Context(), task)
	if err != nil {
		return llm.Request{}, err
	}
	caps, err := client.NativeCapabilities()
	if err != nil {
		return llm.Request{}, err
	}
	input := req.Input
	input.Messages = append([]llm.ModelMessage(nil), input.Messages...)
	total := 0
	for i := range input.Messages {
		message := &input.Messages[i]
		for _, ref := range message.Attachments {
			att, data, err := h.nativeAttachment(r, task, ref.ID)
			if err != nil {
				return llm.Request{}, err
			}
			total += len(data)
			if total > 4<<20 {
				return llm.Request{}, errors.New("native media exceeds 4 MiB request budget")
			}
			if strings.HasPrefix(att.ContentType, "image/") {
				if !caps.ImageInput {
					return llm.Request{}, errors.New("deployment does not support image input")
				}
				if att.ContentType != "image/png" && att.ContentType != "image/jpeg" && att.ContentType != "image/gif" {
					return llm.Request{}, errors.New("native image format is unsupported")
				}
				config, _, err := image.DecodeConfig(bytes.NewReader(data))
				if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 20_000_000 {
					return llm.Request{}, errors.New("native image dimensions are invalid or exceed 20 megapixels")
				}
				message.Images = append(message.Images, llm.ImageInput{ContentType: att.ContentType, Data: data})
			} else {
				if !nativeTextAttachment(att.ContentType, data) {
					return llm.Request{}, errors.New("attachment format is unsupported; provide a UTF-8 document or supported image")
				}
				if len(data) > 64<<10 {
					return llm.Request{}, errors.New("native text attachment exceeds 64 KiB")
				}
				message.Content += fmt.Sprintf("\nAttachment %q (untrusted document):\n%s", att.Filename, data)
			}
		}
	}
	raw, _ := json.Marshal(input)
	// Text-only history is budgeted by the engine using provider measurements.
	// Media still uses a conservative bound because no portable image tokenizer
	// is configured for arbitrary compatible providers.
	if total > 0 && int64(len(raw)+total)+input.MaxOutputTokens+1024 > caps.ContextTokens {
		return llm.Request{}, errors.New("attachments exceed model context budget")
	}
	return input, nil
}

func nativeTextAttachment(contentType string, data []byte) bool {
	return (strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml") && utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

func (h *Handler) NativeReadAttachment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Claim llm.NativeRequest `json:"claim"`
		ID    string            `json:"id"`
	}
	if !decodeNativeSessionRequest(w, r, &req) {
		return
	}
	taskID := chi.URLParam(r, "taskId")
	if _, ok := parseUUIDOrBadRequest(w, taskID, "task_id"); !ok {
		return
	}
	task, err := h.loadNativeInferenceRun(r, taskID, req.Claim)
	if err != nil {
		writeError(w, http.StatusForbidden, "native run authorization failed")
		return
	}
	att, data, err := h.nativeAttachment(r, task, req.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment unavailable for this run")
		return
	}
	if !nativeTextAttachment(att.ContentType, data) || len(data) > 64<<10 {
		writeError(w, http.StatusUnprocessableEntity, "read tool requires a UTF-8 document up to 64 KiB; images must be attached to the model input")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": "document", "id": req.ID, "filename": att.Filename, "content_type": att.ContentType, "text": string(data)})
}
