package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Persist identities only. The API resolves media bytes under the current run's
// authority; captured media always uses the immutable clone, never its source.
func nativeInputAttachments(task Task) ([]llm.AttachmentRef, error) {
	var refs []llm.AttachmentRef
	seen := make(map[string]bool)
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			refs = append(refs, llm.AttachmentRef{ID: id})
		}
	}
	if task.ChatSessionID != "" {
		for _, attachment := range task.ChatMessageAttachments {
			add(attachment.ID)
		}
	} else if task.IssueID == "" && task.QuickCreatePrompt != "" {
		for _, id := range task.QuickCreateAttachmentIDs {
			add(id)
		}
		if len(task.QuickCreateSourceContext) > 0 {
			// Decode only this claim's snapshot metadata. Text documents remain
			// available on demand through multica_read_attachment.
			type owner struct {
				Attachments []ChatAttachmentMeta `json:"attachments"`
			}
			var snapshot struct {
				SourceIssue   owner   `json:"source_issue"`
				CommentThread []owner `json:"comment_thread"`
			}
			if err := json.Unmarshal(task.QuickCreateSourceContext, &snapshot); err != nil {
				return nil, errors.New("captured attachment metadata is invalid; capture the source again")
			}
			for _, entry := range append([]owner{snapshot.SourceIssue}, snapshot.CommentThread...) {
				for _, attachment := range entry.Attachments {
					if strings.HasPrefix(attachment.ContentType, "image/") {
						add(attachment.ID)
					}
				}
			}
		}
	}
	if len(refs) > 4 {
		return nil, errors.New("native model input supports at most four selected attachments and captured images; reduce the input attachments")
	}
	return refs, nil
}

func nativeCanUploadArtifact(task Task) bool {
	if task.ChatSessionID != "" {
		return task.ChatChannelType == "" || execenv.ChannelCarriesFiles(task.ChatChannelType, task.ChatChannelDeliversFiles)
	}
	return task.IssueID != ""
}

func (h *nativeToolHost) attachment(ctx context.Context, call llm.ToolCall) (string, error) {
	if !strings.HasPrefix(h.task.AuthToken, "mat_") || h.task.WorkspaceID == "" {
		return "", errors.New("attachment tools require a task credential")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil || len(args) != 1 {
		return "", errors.New("attachment tool requires one argument")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if call.Name == "multica_read_attachment" {
		var id string
		if json.Unmarshal(args["attachment_id"], &id) != nil || !nativeResourceID(id) {
			return "", errors.New("invalid attachment identifier")
		}
		client := NewClient(h.control.baseURL)
		client.client.Transport = h.control.client.Transport
		client.SetToken(h.task.RemoteMCPDaemonToken)
		var output json.RawMessage
		err := client.postJSON(ctx, "/api/daemon/tasks/"+h.task.ID+"/native/attachment", map[string]any{"claim": llm.NativeRequest{Attempt: h.task.Attempt, DispatchedAt: h.task.DispatchedAt}, "id": id}, &output)
		if err != nil {
			return "", errors.New("attachment is unavailable or not a supported text document for this run")
		}
		return string(output), nil
	}
	if !nativeCanUploadArtifact(h.task) {
		return "", errors.New("this run's delivery surface does not accept file uploads; describe the result in text")
	}
	var path string
	if json.Unmarshal(args["path"], &path) != nil || !filepath.IsLocal(path) || path == "." {
		return "", errors.New("artifact requires a relative file path")
	}
	data, err := h.Environment.ReadArtifact(path)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if claim, ok := agentstate.WriteClaimFromContext(ctx); ok {
		// Stable framing makes the request digest independent of a fresh
		// random multipart boundary when the same operation is inspected.
		_ = writer.SetBoundary("multica-native-" + agentstate.WriteKey(claim.SessionID, llm.ToolCall{ID: claim.CallID}, false)[:32])
	}
	file, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if h.task.ChatSessionID != "" {
		_ = writer.WriteField("task_id", h.task.ID)
	} else if h.task.IssueID != "" {
		_ = writer.WriteField("issue_id", h.task.IssueID)
	} else {
		return "", errors.New("this run's result surface does not accept attachments")
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.control.baseURL+"/api/upload-file", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+h.task.AuthToken)
	req.Header.Set("X-Workspace-ID", h.task.WorkspaceID)
	if claim, ok := agentstate.WriteClaimFromContext(ctx); ok {
		raw, _ := json.Marshal(claim)
		req.Header.Set(agentstate.NativeWriteHeader, string(raw))
	}
	client := &http.Client{Transport: h.control.client.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return "", tools.ErrOutcomeUnknown
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 {
		return "", tools.ErrOutcomeUnknown
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("artifact upload rejected; check storage configuration and task permissions")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return "", tools.ErrOutcomeUnknown
	}
	return nativeArtifactOutput(raw)
}

func nativeArtifactOutput(raw []byte) (string, error) {
	output, _, err := (agentstate.WriteResult{Known: true, Status: http.StatusOK, Body: raw}).ToolOutput("multica_upload_artifact")
	if err != nil {
		return "", tools.ErrOutcomeUnknown
	}
	return output, nil
}
