package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// CompleteNativeModel performs exactly one authorized model step. No task-level
// retry or upstream credentials are present on the daemon. A missing terminal
// frame is an error even if the display already received text.
func (c *Client) CompleteNativeModel(ctx context.Context, task Task, lease agentstate.Session, input llm.Request, emit func(llm.Event) error) (*llm.Response, error) {
	body, err := json.Marshal(llm.NativeRequest{Attempt: task.Attempt, DispatchedAt: task.DispatchedAt, SessionID: lease.ID, LeaseToken: lease.LeaseToken, SessionVersion: lease.Version, Input: input})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/daemon/tasks/"+url.PathEscape(task.ID)+"/native/inference", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	c.setIdentityHeaders(req)
	// The ordinary control-plane client has a 30-second total timeout, too
	// short for model streams. Reuse its transport, retaining cancellation.
	client := &http.Client{Transport: c.client.Transport, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("native model endpoint returned HTTP %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		return nil, errors.New("native model endpoint returned an unsupported protocol")
	}
	// Bound total input even if a compromised endpoint ignores its own limits.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	dec.DisallowUnknownFields()
	for {
		var frame llm.Frame
		if err := dec.Decode(&frame); err != nil {
			return nil, fmt.Errorf("native model stream ended before a result: %w", err)
		}
		variants := 0
		if frame.Event != nil {
			variants++
		}
		if frame.Response != nil {
			variants++
		}
		if frame.Error != "" {
			variants++
		}
		if variants != 1 {
			return nil, errors.New("native model stream has an invalid frame")
		}
		if frame.Error != "" {
			// Only recognized codes become user-visible diagnostics. Arbitrary
			// server strings still receive the credential-free generic message.
			switch frame.Error {
			case "model_step_cancelled":
				return nil, errors.New("native model step cancelled or timed out")
			case "model_quota_exceeded":
				return nil, errors.New("native model quota exceeded")
			case "model_response_incomplete":
				return nil, errors.New("native model response incomplete; no partial tool calls were executed")
			case "model_response_empty":
				return nil, errors.New("native model returned no usable content or tool calls")
			case "model_stream_interrupted":
				return nil, errors.New("native model response stream interrupted")
			case "model_transport_error":
				return nil, errors.New("native model connection failed")
			case "model_upstream_error":
				return nil, errors.New("native model upstream returned an error")
			case "model_request_rejected":
				return nil, errors.New("native model request rejected; check API protocol and model capabilities")
			case "model_authentication_failed":
				return nil, errors.New("native model authentication failed; check provider credentials and access")
			case "model_not_found":
				return nil, errors.New("native model not found or unavailable at the configured endpoint")
			case "model_rate_limited":
				return nil, errors.New("native model provider rate limit exceeded")
			}
			return nil, errors.New("native model step failed")
		}
		if frame.Event != nil && emit != nil {
			if err := emit(*frame.Event); err != nil {
				return nil, err
			}
		}
		if frame.Response != nil {
			if frame.Response.Message.Role != "assistant" {
				return nil, errors.New("native model response is not an assistant step")
			}
			history := append(append([]llm.ModelMessage(nil), input.Messages...), frame.Response.Message)
			if err := llm.ValidateHistory(history, 1<<20, true); err != nil {
				return nil, err
			}
			names := make(map[string]bool)
			for _, tool := range input.Tools {
				names[tool.Name] = true
			}
			for _, call := range frame.Response.Message.ToolCalls {
				if !names[call.Name] {
					return nil, errors.New("native model response requested an unknown tool")
				}
			}
			return frame.Response, nil
		}
	}
}
