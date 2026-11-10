package llm

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ConnectionTest describes only protocol health, never upstream content or credentials.
type ConnectionTest struct {
	OK         bool   `json:"ok"`
	Stage      string `json:"stage"`
	Code       string `json:"code"`
	DurationMs int64  `json:"duration_ms"`
}

// TestConnection uses the Native transport with synthetic input and a simulated
// tool result. No executor or task is involved. Callers own admission and retries.
func (c *Client) TestConnection(ctx context.Context) (result ConnectionTest) {
	started := time.Now()
	defer func() { result.DurationMs = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	result.Stage = "text"
	req := Request{Messages: []ModelMessage{{Role: "user", Content: "This is a connection test. Reply briefly with OK."}}, MaxOutputTokens: min(c.capabilities.MaxOutputTokens, 2048)}
	text, err := c.Complete(ctx, req, nil)
	if err != nil {
		result.Code = FailureCode(err)
		return
	}
	if strings.TrimSpace(text.Message.Content) == "" || len(text.Message.ToolCalls) != 0 {
		result.Code = "model_response_empty"
		return
	}
	result.Stage = "tool_call"
	const tool = "multica_connection_probe"
	req.Messages = []ModelMessage{{Role: "user", Content: "Test function calling: call multica_connection_probe with no arguments. After its result, reply briefly with OK and do not call any more tools."}}
	req.Tools = []Tool{{Name: tool, Description: "A connection test with no side effects; returns a simulated success.", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
	req.requiredTool = tool
	call, err := c.Complete(ctx, req, nil)
	if err != nil {
		result.Code = FailureCode(err)
		return
	}
	if len(call.Message.ToolCalls) != 1 || call.Message.ToolCalls[0].Name != tool {
		result.Code = "model_tool_call_failed"
		return
	}
	var arguments map[string]json.RawMessage
	if json.Unmarshal(call.Message.ToolCalls[0].Arguments, &arguments) != nil || len(arguments) != 0 {
		result.Code = "model_tool_call_failed"
		return
	}
	result.Stage = "tool_result"
	req.requiredTool = ""
	req.Tools = nil
	req.Messages = append(req.Messages, call.Message, ModelMessage{Role: "tool", ToolCallID: call.Message.ToolCalls[0].ID, Content: `{"ok":true}`})
	final, err := c.Complete(ctx, req, nil)
	if err != nil {
		result.Code = FailureCode(err)
		return
	}
	if strings.TrimSpace(final.Message.Content) == "" || len(final.Message.ToolCalls) != 0 {
		result.Code = "model_tool_result_failed"
		return
	}
	result.OK, result.Stage, result.Code = true, "complete", "ok"
	return
}
