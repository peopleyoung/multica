package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Capabilities describe an operator-verified model deployment, never a guess
// based on its name. Zero values mean unknown or unsupported. Native execution
// requires streaming function calls and explicit resource limits.
type Capabilities struct {
	API                  string `json:"api,omitempty"`
	StreamingToolCalls   bool   `json:"streaming_tool_calls"`
	JSONObject           bool   `json:"json_object"`
	ContextTokens        int64  `json:"context_tokens"`
	MaxOutputTokens      int64  `json:"max_output_tokens"`
	MaxToolArgumentBytes int    `json:"max_tool_argument_bytes"`
	MaxTools             int    `json:"max_tools"`
	ImageInput           bool   `json:"image_input,omitempty"`
}

func (c Capabilities) Validate() error {
	if c.API != "" && c.API != "chat_completions" && c.API != "responses" {
		return errors.New("llm: API must be chat_completions or responses")
	}
	if !c.StreamingToolCalls {
		return errors.New("llm: streaming tool-call capability has not been verified")
	}
	if c.ContextTokens <= 0 || c.MaxOutputTokens <= 0 || c.MaxOutputTokens >= c.ContextTokens || c.MaxToolArgumentBytes <= 0 || c.MaxTools <= 0 {
		return errors.New("llm: explicit context, output, tool argument and tool count limits are required")
	}
	if c.ContextTokens > 10_000_000 || c.MaxToolArgumentBytes > 1_048_576 || c.MaxTools > 128 {
		return errors.New("llm: capabilities exceed native protocol resource limits")
	}
	return nil
}

// Request contains only model input. Model identity, credentials, endpoint and
// transport options belong exclusively to the API server's deployment config.
type Request struct {
	Messages        []ModelMessage `json:"messages"`
	Tools           []Tool         `json:"tools,omitempty"`
	MaxOutputTokens int64          `json:"max_output_tokens"`
	jsonObject      bool
	requiredTool    string
}

type ModelMessage struct {
	Role        string          `json:"role"`
	Content     string          `json:"content,omitempty"`
	ToolCalls   []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	// Images are hydrated from authorized attachment objects by the API only.
	Images []ImageInput `json:"-"`
	// Reasoning carries only bounded stateless Responses replay data. Source
	// binds it to the configured endpoint, model and credential, never a task.
	Reasoning *ReasoningState `json:"reasoning,omitempty"`
}

type ReasoningState struct {
	Source string          `json:"source"`
	Items  []ReasoningItem `json:"items"`
}

type ReasoningItem struct {
	ID               string             `json:"id"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
	Summary          []ReasoningSummary `json:"summary"`
}

type ReasoningSummary struct {
	Text string `json:"text"`
}

type AttachmentRef struct {
	ID string `json:"id"`
}
type ImageInput struct {
	ContentType string
	Data        []byte
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Usage is absent when the upstream did not report it; absence is not zero
// billable usage. Counts are upstream-reported, not local token estimates.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
}

type Response struct {
	Message ModelMessage `json:"message"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// Event is display-only. Tools may run only after Complete succeeds and
// returns a fully validated Response, never from partial argument deltas.
type Event struct {
	Text string `json:"text"`
}

// ValidateHistory checks complete model steps. Tool results immediately follow
// their assistant call, exactly once per ID. A pending tool group is allowed
// only in a persisted checkpoint, not in a new inference request.
func ValidateHistory(messages []ModelMessage, maxArgumentBytes int, allowPending bool) error {
	if len(messages) == 0 {
		return errors.New("llm: messages must not be empty")
	}
	seen := make(map[string]bool)
	pending := make(map[string]bool)
	for i, m := range messages {
		if m.Reasoning != nil {
			if err := validateReasoning(m); err != nil {
				return err
			}
		}
		if len(m.Attachments) > 0 && m.Role != "user" {
			return errors.New("llm: attachments require a user message")
		}
		if len(m.Attachments) > 4 {
			return errors.New("llm: at most four attachments per message")
		}
		if len(pending) != 0 && m.Role != "tool" {
			return fmt.Errorf("llm: message %d interrupts pending tool results", i)
		}
		if m.Role != "assistant" && len(m.ToolCalls) != 0 {
			return fmt.Errorf("llm: message %d has tool calls outside assistant role", i)
		}
		if m.Role != "tool" && m.ToolCallID != "" {
			return fmt.Errorf("llm: message %d has unexpected tool result ID", i)
		}
		switch m.Role {
		case "system", "user", "assistant":
			for _, call := range m.ToolCalls {
				if call.ID == "" || seen[call.ID] || !validToolName(call.Name) || !jsonObject(call.Arguments) || len(call.Arguments) > maxArgumentBytes {
					return fmt.Errorf("llm: message %d has an invalid or duplicate tool call", i)
				}
				seen[call.ID], pending[call.ID] = true, true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return fmt.Errorf("llm: message %d has an unmatched tool result", i)
			}
			delete(pending, m.ToolCallID)
		default:
			return fmt.Errorf("llm: message %d has an unsupported role", i)
		}
	}
	if !allowPending && len(pending) != 0 {
		return errors.New("llm: tool results are missing")
	}
	return nil
}

func (r Request) Validate(c Capabilities) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if r.MaxOutputTokens <= 0 || r.MaxOutputTokens > c.MaxOutputTokens {
		return errors.New("llm: output token budget exceeds configured capability")
	}
	if len(r.Tools) > c.MaxTools {
		return errors.New("llm: tool count exceeds configured capability")
	}
	names := make(map[string]bool)
	for _, tool := range r.Tools {
		var schema struct {
			Type string `json:"type"`
		}
		if !validToolName(tool.Name) || names[tool.Name] || !jsonObject(tool.Parameters) || json.Unmarshal(tool.Parameters, &schema) != nil || schema.Type != "object" {
			return errors.New("llm: tools require unique names and object parameter schemas")
		}
		names[tool.Name] = true
	}
	if r.requiredTool != "" && !names[r.requiredTool] {
		return errors.New("llm: required tool is not available")
	}
	return ValidateHistory(r.Messages, c.MaxToolArgumentBytes, false)
}

func validToolName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func jsonObject(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return strings.HasPrefix(s, "{") && json.Valid(raw)
}
