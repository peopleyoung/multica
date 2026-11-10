package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

func TestFailureCodeRedactsUpstreamDetails(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("private request: %w", context.Canceled), "model_step_cancelled"},
		{context.DeadlineExceeded, "model_step_cancelled"},
		{ErrQuotaExceeded, "model_quota_exceeded"},
		{ErrIncompleteResponse, "model_response_incomplete"},
		{io.ErrUnexpectedEOF, "model_stream_interrupted"},
		{&openai.Error{StatusCode: 502}, "model_upstream_error"},
		{&openai.Error{StatusCode: 400, Message: "private upstream request"}, "model_request_rejected"},
		{&openai.Error{StatusCode: 401, Message: "private credential"}, "model_authentication_failed"},
		{&openai.Error{StatusCode: 404}, "model_not_found"},
		{&openai.Error{StatusCode: 429}, "model_rate_limited"},
		{fmt.Errorf("wrapped: %w", &ssestream.StreamError{Message: "private upstream credentials and prompt"}), "model_upstream_error"},
		{&url.Error{Op: "POST", URL: "https://private.invalid/secret", Err: errors.New("private key")}, "model_transport_error"},
		{errors.New("private prompt and credentials"), "model_step_failed"},
	} {
		if got := FailureCode(tc.err); got != tc.code {
			t.Errorf("failure classification: got %q, want %q", got, tc.code)
		}
	}
}
