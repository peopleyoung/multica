package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func nativeWorkflowDefinitions() []llm.Tool {
	return []llm.Tool{
		{Name: "multica_read_context", Description: "Read workspace members, visible agents, skills, squads or automations, this run's automation, or its chat history/thread. Chat is pinned to the task credential, never an arbitrary session. Thread accepts a Slack thread id; before pages history. Other resources take no cursor. Management responses exclude runtime and webhook credentials.", Parameters: json.RawMessage(`{"type":"object","properties":{"resource":{"type":"string","enum":["members","agents","skills","squads","autopilots","autopilot","chat_history","chat_thread"]},"thread_id":{"type":"string"},"before":{"type":"string"}},"required":["resource"],"additionalProperties":false}`)},
		{Name: "multica_create_issue", Description: "Create an issue using existing permissions and triggers. Quick-create binds selected fields, attachments and origin, defaults ownership to the picker, and permits one creation attempt only. Never retry an uncertain creation. Assignee type and id must be supplied together.", Parameters: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"},"status":{"type":"string"},"priority":{"type":"string"},"assignee_type":{"type":"string","enum":["member","agent","squad"]},"assignee_id":{"type":"string"},"parent_issue_id":{"type":"string"},"project_id":{"type":"string"},"due_date":{"type":"string"},"stage":{"type":"integer","minimum":0},"attachment_ids":{"type":"array","items":{"type":"string"},"maxItems":4}},"required":["title"],"additionalProperties":false}`)},
	}
}

func decodeNativeToolArguments(raw json.RawMessage, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if len(raw) > 64<<10 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return errors.New("invalid native tool arguments")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("native tool arguments must not be null")
		}
	}
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected native tool field or value type")
	}
	return nil
}

func (h *nativeToolHost) workflow(ctx context.Context, call llm.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if call.Name == "multica_read_context" {
		var args struct {
			Resource string `json:"resource"`
			ThreadID string `json:"thread_id"`
			Before   string `json:"before"`
		}
		if err := decodeNativeToolArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		if args.Resource != "chat_history" && args.Resource != "chat_thread" && (args.ThreadID != "" || args.Before != "") {
			return "", errors.New("this resource does not accept a chat cursor")
		}
		path := ""
		switch args.Resource {
		case "members":
			path = "/api/workspaces/" + url.PathEscape(h.task.WorkspaceID) + "/members"
		case "agents":
			path = "/api/agents"
		case "squads":
			path = "/api/squads"
		case "skills":
			path = "/api/skills"
		case "autopilots":
			path = "/api/autopilots"
		case "autopilot":
			if !nativeResourceID(h.task.AutopilotID) {
				return "", errors.New("this run has no automation")
			}
			result, err := h.request(ctx, http.MethodGet, "/api/autopilots/"+url.PathEscape(h.task.AutopilotID), nil)
			if err != nil {
				return "", err
			}
			return nativeAutomationContext(result)
		case "chat_history", "chat_thread":
			if h.task.ChatSessionID == "" || args.Resource == "chat_thread" && h.task.ChatChannelType != "slack" {
				return "", errors.New("this conversation has no requested history capability")
			}
			if len(args.ThreadID) > 200 || len(args.Before) > 512 {
				return "", errors.New("chat cursor exceeds limit")
			}
			query := url.Values{"limit": {"30"}}
			path = "/api/chat/history"
			if args.Resource == "chat_thread" {
				path = "/api/chat/thread"
				if args.ThreadID != "" {
					query.Set("id", args.ThreadID)
				}
			} else if args.ThreadID != "" {
				return "", errors.New("thread_id requires chat_thread")
			}
			if args.Before != "" {
				query.Set("before", args.Before)
			}
			return h.request(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
		default:
			return "", errors.New("unknown context resource")
		}
		return h.request(ctx, http.MethodGet, path, nil)
	}
	var args struct {
		Title         string   `json:"title"`
		Description   string   `json:"description,omitempty"`
		Status        string   `json:"status,omitempty"`
		Priority      string   `json:"priority,omitempty"`
		AssigneeType  string   `json:"assignee_type,omitempty"`
		AssigneeID    string   `json:"assignee_id,omitempty"`
		ParentIssueID string   `json:"parent_issue_id,omitempty"`
		ProjectID     string   `json:"project_id,omitempty"`
		DueDate       string   `json:"due_date,omitempty"`
		Stage         *int32   `json:"stage,omitempty"`
		AttachmentIDs []string `json:"attachment_ids,omitempty"`
	}
	if err := decodeNativeToolArguments(call.Arguments, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Title) == "" || len(args.Title) > 500 || len(args.Description) > 32<<10 || args.Stage != nil && *args.Stage < 0 {
		return "", errors.New("issue requires a title up to 500 bytes, a description up to 32 KiB and a nonnegative stage")
	}
	if args.AssigneeID == "" != (args.AssigneeType == "") || args.AssigneeType != "" && args.AssigneeType != "member" && args.AssigneeType != "agent" && args.AssigneeType != "squad" {
		return "", errors.New("invalid issue assignee; specify its type and exact id together")
	}
	if h.task.QuickCreatePrompt != "" {
		if h.task.QuickCreatePriority != "" {
			args.Priority = h.task.QuickCreatePriority
		}
		if h.task.QuickCreateDueDate != "" {
			args.DueDate = h.task.QuickCreateDueDate
		}
		if h.task.ProjectID != "" {
			args.ProjectID = h.task.ProjectID
		}
		if h.task.ParentIssueID != "" {
			args.ParentIssueID = h.task.ParentIssueID
		}
		args.AttachmentIDs = h.task.QuickCreateAttachmentIDs
		if args.AssigneeID == "" {
			args.AssigneeType, args.AssigneeID = "agent", h.task.AgentID
			if h.task.SquadID != "" {
				args.AssigneeType, args.AssigneeID = "squad", h.task.SquadID
			}
		}
	}
	if args.DueDate != "" {
		if _, err := time.Parse("2006-01-02", args.DueDate); err != nil {
			return "", errors.New("due_date requires YYYY-MM-DD")
		}
	}
	for _, id := range append([]string{args.AssigneeID, args.ParentIssueID, args.ProjectID}, args.AttachmentIDs...) {
		if id != "" && !nativeResourceID(id) {
			return "", errors.New("invalid issue resource identifier")
		}
	}
	if len(args.AttachmentIDs) > 4 {
		return "", errors.New("native issue creation accepts at most four attachments")
	}
	raw, _ := json.Marshal(args)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if h.task.QuickCreatePrompt != "" {
		body["origin_type"], body["origin_id"] = "quick_create", h.task.ID
		h.createMu.Lock()
		if h.quickCreateAttempted {
			h.createMu.Unlock()
			return "", errors.New("quick-create already attempted issue creation; do not retry")
		}
		h.quickCreateAttempted = true
		h.createMu.Unlock()
	}
	return h.request(ctx, http.MethodPost, "/api/issues", body)
}

// The regular detail endpoint may include trigger credentials for a writer.
// Only this explicit context projection is permitted in model/tool history.
func nativeAutomationContext(result string) (string, error) {
	var payload struct {
		Autopilot map[string]json.RawMessage `json:"autopilot"`
	}
	if json.Unmarshal([]byte(result), &payload) != nil || payload.Autopilot == nil {
		return "", errors.New("invalid automation context response")
	}
	raw, err := agentstate.ProjectManagementResponse("multica_manage_autopilot", json.RawMessage(result))
	return string(raw), err
}
