package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

var ErrIncompleteResponse = errors.New("llm: model response is incomplete")
var ErrEmptyResponse = errors.New("llm: model response is empty")

// Complete streams display text and returns one complete, validated model step.
// A failed or cancelled step returns no Response, so partial tool calls cannot
// accidentally become executable. It never retries an already-opened stream.
func (c *Client) Complete(ctx context.Context, req Request, emit func(Event) error) (*Response, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	if err := req.Validate(c.capabilities); err != nil {
		return nil, err
	}
	if c.capabilities.API == "responses" {
		return c.completeResponses(ctx, req, emit)
	}
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(c.defaultModel),
		MaxCompletionTokens: openai.Int(req.MaxOutputTokens),
		StreamOptions:       openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if req.requiredTool != "" {
		params.ToolChoice.OfFunctionToolChoice = &openai.ChatCompletionNamedToolChoiceParam{
			Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: req.requiredTool},
		}
	}
	for _, m := range req.Messages {
		var msg openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case "system":
			msg = openai.SystemMessage(m.Content)
		case "user":
			msg = openai.UserMessage(m.Content)
			if len(m.Images) > 0 {
				if !c.capabilities.ImageInput {
					return nil, errors.New("llm: image input capability is not enabled")
				}
				parts := []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(m.Content)}
				for _, image := range m.Images {
					parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: "data:" + image.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)}))
				}
				msg = openai.UserMessage(parts)
			}
		case "tool":
			msg = openai.ToolMessage(m.Content, m.ToolCallID)
		case "assistant":
			msg = openai.AssistantMessage(m.Content)
			for _, call := range m.ToolCalls {
				msg.OfAssistant.ToolCalls = append(msg.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID:       call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Name, Arguments: string(call.Arguments)},
					},
				})
			}
		}
		params.Messages = append(params.Messages, msg)
	}
	names := make(map[string]bool)
	for _, tool := range req.Tools {
		var schema shared.FunctionParameters
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			return nil, fmt.Errorf("llm: invalid tool schema: %w", err)
		}
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{Name: tool.Name, Description: openai.String(tool.Description), Parameters: schema},
		}})
		names[tool.Name] = true
	}

	stream := c.sdk.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	result := &Response{Message: ModelMessage{Role: "assistant"}}
	var text strings.Builder
	type partialCall struct{ id, name, arguments strings.Builder }
	calls := make(map[int64]*partialCall)
	finish := ""
	for stream.Next() {
		chunk := stream.Current()
		if chunk.JSON.Usage.Valid() {
			if chunk.Usage.PromptTokens < 0 || chunk.Usage.CompletionTokens < 0 || chunk.Usage.PromptTokensDetails.CachedTokens < 0 {
				return nil, errors.New("llm: invalid upstream usage")
			}
			result.Usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, CachedTokens: chunk.Usage.PromptTokensDetails.CachedTokens}
		}
		if len(chunk.Choices) == 0 {
			continue // The optional trailing usage chunk has no choices.
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 || finish != "" {
			return nil, errors.New("llm: unexpected streaming choice")
		}
		choice := chunk.Choices[0]
		if choice.Delta.Refusal != "" || choice.Delta.FunctionCall.Name != "" || choice.Delta.FunctionCall.Arguments != "" {
			return nil, errors.New("llm: refused or unsupported model response")
		}
		if delta := choice.Delta.Content; delta != "" {
			// Bound buffering even if an upstream ignores max_completion_tokens.
			if int64(text.Len()+len(delta)) > req.MaxOutputTokens*32 {
				return nil, errors.New("llm: text output exceeds byte limit")
			}
			text.WriteString(delta)
			if emit != nil {
				if err := emit(Event{Text: delta}); err != nil {
					return nil, err
				}
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			if delta.Index < 0 || delta.Index >= int64(c.capabilities.MaxTools) || delta.Type != "" && delta.Type != "function" {
				return nil, errors.New("llm: invalid tool-call index or type")
			}
			call := calls[delta.Index]
			if call == nil {
				call = &partialCall{}
				calls[delta.Index] = call
			}
			if call.id.Len()+len(delta.ID) > 256 || call.name.Len()+len(delta.Function.Name) > 64 || call.arguments.Len()+len(delta.Function.Arguments) > c.capabilities.MaxToolArgumentBytes {
				return nil, errors.New("llm: tool-call fragments exceed byte limit")
			}
			call.id.WriteString(delta.ID)
			call.name.WriteString(delta.Function.Name)
			call.arguments.WriteString(delta.Function.Arguments)
		}
		finish = choice.FinishReason
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if finish != "stop" && finish != "tool_calls" || (finish == "tool_calls") != (len(calls) > 0) {
		return nil, ErrIncompleteResponse
	}
	result.Message.Content = text.String()
	if len(calls) == 0 && strings.TrimSpace(result.Message.Content) == "" {
		return nil, ErrEmptyResponse
	}
	for i := int64(0); i < int64(len(calls)); i++ {
		call := calls[i]
		if call == nil || !names[call.name.String()] {
			return nil, errors.New("llm: model requested an unknown tool")
		}
		result.Message.ToolCalls = append(result.Message.ToolCalls, ToolCall{ID: call.id.String(), Name: call.name.String(), Arguments: json.RawMessage(call.arguments.String())})
	}
	history := append(append([]ModelMessage(nil), req.Messages...), result.Message)
	if err := ValidateHistory(history, c.capabilities.MaxToolArgumentBytes, true); err != nil {
		return nil, err
	}
	return result, nil
}
