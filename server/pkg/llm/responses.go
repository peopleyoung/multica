package llm

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

var ErrUpstreamResponse = errors.New("llm: upstream rejected the model response")

func validateReasoning(m ModelMessage) error {
	state := m.Reasoning
	if m.Role != "assistant" || len(state.Source) != 64 || len(state.Items) == 0 || len(state.Items) > 128 {
		return errors.New("llm: invalid reasoning replay state")
	}
	if _, err := hex.DecodeString(state.Source); err != nil {
		return errors.New("llm: invalid reasoning source")
	}
	raw, _ := json.Marshal(state)
	if len(raw) > 1<<20 {
		return errors.New("llm: reasoning replay exceeds byte limit")
	}
	seen := map[string]bool{}
	for _, item := range state.Items {
		if item.ID == "" || len(item.ID) > 256 || seen[item.ID] || len(item.Summary) > 128 {
			return errors.New("llm: invalid reasoning item")
		}
		seen[item.ID] = true
	}
	return nil
}

// Responses runs statelessly: Native owns history and side effects. Only a
// completed response may produce executable calls; deltas are display-only.
func (c *Client) completeResponses(ctx context.Context, req Request, emit func(Event) error) (*Response, error) {
	params := responses.ResponseNewParams{Model: shared.ResponsesModel(c.defaultModel), MaxOutputTokens: openai.Int(req.MaxOutputTokens), Store: openai.Bool(false)}
	if req.requiredTool != "" {
		params.ToolChoice.OfFunctionTool = &responses.ToolChoiceFunctionParam{Name: req.requiredTool}
	}
	if req.jsonObject {
		params.Text.Format.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user":
			item := responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRole(m.Role))
			if len(m.Images) > 0 {
				if !c.capabilities.ImageInput {
					return nil, errors.New("llm: image input capability is not enabled")
				}
				parts := responses.ResponseInputMessageContentListParam{responses.ResponseInputContentParamOfInputText(m.Content)}
				for _, image := range m.Images {
					part := responses.ResponseInputContentParamOfInputImage("auto")
					part.OfInputImage.ImageURL = openai.String("data:" + image.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image.Data))
					parts = append(parts, part)
				}
				item = responses.ResponseInputItemParamOfInputMessage(parts, m.Role)
			}
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, item)
		case "assistant":
			if m.Reasoning != nil && m.Reasoning.Source == c.responseSource {
				for _, reason := range m.Reasoning.Items {
					summaries := make([]responses.ResponseReasoningItemSummaryParam, 0, len(reason.Summary))
					for _, summary := range reason.Summary {
						summaries = append(summaries, responses.ResponseReasoningItemSummaryParam{Text: summary.Text})
					}
					item := responses.ResponseInputItemParamOfReasoning(reason.ID, summaries)
					if reason.EncryptedContent != "" {
						item.OfReasoning.EncryptedContent = openai.String(reason.EncryptedContent)
					}
					params.Input.OfInputItemList = append(params.Input.OfInputItemList, item)
				}
			}
			if m.Content != "" {
				params.Input.OfInputItemList = append(params.Input.OfInputItemList, responses.ResponseInputItemParamOfMessage(m.Content, "assistant"))
			}
			for _, call := range m.ToolCalls {
				params.Input.OfInputItemList = append(params.Input.OfInputItemList, responses.ResponseInputItemParamOfFunctionCall(string(call.Arguments), call.ID, call.Name))
			}
		case "tool":
			item := responses.ResponseInputItemParamOfFunctionCallOutput(m.Content)
			item.OfFunctionCallOutput.CallID = openai.String(m.ToolCallID)
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, item)
		}
	}
	names := map[string]bool{}
	for _, tool := range req.Tools {
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			return nil, fmt.Errorf("llm: invalid tool schema: %w", err)
		}
		// Native validates arguments itself; its schemas intentionally contain
		// optional fields and unions and must not be normalized to strict mode.
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: tool.Name, Description: openai.String(tool.Description), Parameters: schema, Strict: openai.Bool(false)}})
		names[tool.Name] = true
	}
	stream := c.sdk.Responses.NewStreaming(ctx, params)
	defer stream.Close()
	var completed *responses.Response
	textBytes := 0
	argumentBytes := map[int64]int{}
	for stream.Next() {
		event := stream.Current()
		if completed != nil {
			return nil, errors.New("llm: event after completed response")
		}
		switch event.Type {
		case "response.output_text.delta":
			textBytes += len(event.Delta)
			if int64(textBytes) > req.MaxOutputTokens*32 {
				return nil, errors.New("llm: text output exceeds byte limit")
			}
			if emit != nil && event.Delta != "" {
				if err := emit(Event{Text: event.Delta}); err != nil {
					return nil, err
				}
			}
		case "response.function_call_arguments.delta":
			if event.OutputIndex < 0 || event.OutputIndex >= 256 {
				return nil, errors.New("llm: invalid output item index")
			}
			argumentBytes[event.OutputIndex] += len(event.Delta)
			if len(argumentBytes) > c.capabilities.MaxTools || argumentBytes[event.OutputIndex] > c.capabilities.MaxToolArgumentBytes {
				return nil, errors.New("llm: tool-call fragments exceed byte limit")
			}
		case "response.incomplete":
			return nil, ErrIncompleteResponse
		case "response.failed", "error":
			return nil, ErrUpstreamResponse
		case "response.refusal.delta", "response.refusal.done":
			return nil, ErrUpstreamResponse
		case "response.completed":
			value := event.Response
			if value.Status != "completed" || value.Error.Code != "" {
				return nil, ErrIncompleteResponse
			}
			completed = &value
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, ErrIncompleteResponse
	}
	result := &Response{Message: ModelMessage{Role: "assistant"}}
	var content strings.Builder
	state := &ReasoningState{Source: c.responseSource}
	if len(completed.Output) > 256 {
		return nil, errors.New("llm: too many output items")
	}
	for _, item := range completed.Output {
		switch item.Type {
		case "message":
			if item.Role != "assistant" || item.Status != "completed" {
				return nil, ErrIncompleteResponse
			}
			for _, part := range item.Content {
				if part.Type != "output_text" {
					return nil, ErrUpstreamResponse
				}
				if int64(content.Len()+len(part.Text)) > req.MaxOutputTokens*32 {
					return nil, errors.New("llm: text output exceeds byte limit")
				}
				content.WriteString(part.Text)
			}
		case "function_call":
			if item.Status != "" && item.Status != "completed" {
				return nil, ErrIncompleteResponse
			}
			if !names[item.Name] || len(result.Message.ToolCalls) >= c.capabilities.MaxTools {
				return nil, errors.New("llm: model requested an unknown or excessive tool")
			}
			result.Message.ToolCalls = append(result.Message.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments.OfString)})
		case "reasoning":
			reason := ReasoningItem{ID: item.ID, EncryptedContent: item.EncryptedContent, Summary: []ReasoningSummary{}}
			for _, summary := range item.Summary {
				reason.Summary = append(reason.Summary, ReasoningSummary{Text: summary.Text})
			}
			// ID-only items cannot be replayed when store=false. A gateway that omits
			// encrypted content still retains its visible, provider-neutral history.
			if reason.EncryptedContent != "" {
				state.Items = append(state.Items, reason)
			}
		default:
			return nil, errors.New("llm: unsupported response output item")
		}
	}
	result.Message.Content = content.String()
	if len(state.Items) > 0 {
		result.Message.Reasoning = state
	}
	if strings.TrimSpace(result.Message.Content) == "" && len(result.Message.ToolCalls) == 0 {
		return nil, ErrEmptyResponse
	}
	if completed.JSON.Usage.Valid() {
		u := completed.Usage
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.InputTokensDetails.CachedTokens < 0 || u.InputTokensDetails.CachedTokens > u.InputTokens {
			return nil, errors.New("llm: invalid upstream usage")
		}
		result.Usage = &Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.InputTokensDetails.CachedTokens}
	}
	history := append(append([]ModelMessage(nil), req.Messages...), result.Message)
	if err := ValidateHistory(history, c.capabilities.MaxToolArgumentBytes, true); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) generateResponseText(ctx context.Context, model, systemPrompt, userPrompt string, limit int64, jsonObject bool) (string, error) {
	if strings.TrimSpace(model) != "" && strings.TrimSpace(model) != c.defaultModel {
		return "", errors.New("llm: request model differs from deployment model")
	}
	if limit <= 0 {
		limit = 2048
	}
	request := Request{MaxOutputTokens: min(limit, c.capabilities.MaxOutputTokens), jsonObject: jsonObject}
	if systemPrompt != "" {
		request.Messages = append(request.Messages, ModelMessage{Role: "system", Content: systemPrompt})
	}
	request.Messages = append(request.Messages, ModelMessage{Role: "user", Content: userPrompt})
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	result, err := c.Complete(ctx, request, nil)
	if err != nil {
		return "", err
	}
	return result.Message.Content, nil
}
