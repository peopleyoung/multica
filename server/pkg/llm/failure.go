package llm

import (
	"context"
	"errors"
	"io"
	"net"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// FailureCode is safe to log or return to a daemon. Never expose err.Error():
// upstream errors can contain credentials, request bodies or private URLs.
func FailureCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "model_step_cancelled"
	case errors.Is(err, ErrQuotaExceeded):
		return "model_quota_exceeded"
	case errors.Is(err, ErrIncompleteResponse):
		return "model_response_incomplete"
	case errors.Is(err, ErrUpstreamResponse):
		return "model_upstream_error"
	case errors.Is(err, ErrEmptyResponse):
		return "model_response_empty"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "model_stream_interrupted"
	}
	var upstream *openai.Error
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case 400, 422:
			return "model_request_rejected"
		case 401, 403:
			return "model_authentication_failed"
		case 404:
			return "model_not_found"
		case 429:
			return "model_rate_limited"
		}
		return "model_upstream_error"
	}
	var stream *ssestream.StreamError
	if errors.As(err, &stream) {
		return "model_upstream_error"
	}
	var transport net.Error
	if errors.As(err, &transport) {
		return "model_transport_error"
	}
	return "model_step_failed"
}
