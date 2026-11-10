package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	ErrQuotaExceeded        = errors.New("llm: shared model quota exceeded")
	ErrAdmissionUnavailable = errors.New("llm: model admission unavailable")
)

// CallScope is supplied by trusted application code, never upstream headers.
// Scope identifiers are audit fields, not metric labels.
type CallScope struct {
	WorkspaceID string
	TaskID      string
	SessionID   string
	Purpose     string
}

type scopeContextKey struct{}

func WithCallScope(ctx context.Context, scope CallScope) context.Context {
	return context.WithValue(ctx, scopeContextKey{}, scope)
}

type Call struct {
	Scope          CallScope
	Model          string
	RetryAttempt   int
	ReservedTokens int64
}

type CallResult struct {
	Outcome    string
	HTTPStatus int
	Duration   time.Duration
	Usage      *Usage
}

// Admission reserves one upstream HTTP attempt. SDK error retries and parameter
// negotiation acquire separate reservations. Finish must be idempotent.
type Admission interface {
	Acquire(context.Context, Call) (func(context.Context, CallResult) error, error)
}

type admittedClient struct {
	upstream interface {
		Do(*http.Request) (*http.Response, error)
	}
	admission     Admission
	model         string
	contextTokens int64
}

func (c *admittedClient) Do(req *http.Request) (*http.Response, error) {
	// Every admitted request has a hard upper bound below its database lease.
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Minute)
	req = req.Clone(ctx)
	if req.Body == nil {
		cancel()
		return nil, ErrAdmissionUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, (8<<20)+1))
	_ = req.Body.Close()
	if err != nil || len(raw) > 8<<20 {
		cancel()
		return nil, ErrAdmissionUnavailable
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	var params struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
		MaxTokens           int64 `json:"max_tokens"`
		MaxOutputTokens     int64 `json:"max_output_tokens"`
		Stream              bool  `json:"stream"`
	}
	if json.Unmarshal(raw, &params) != nil {
		cancel()
		return nil, ErrAdmissionUnavailable
	}
	output := max(params.MaxCompletionTokens, params.MaxTokens, params.MaxOutputTokens)
	if output <= 0 || output > 1_000_000 {
		cancel()
		return nil, ErrAdmissionUnavailable
	}
	scope, _ := ctx.Value(scopeContextKey{}).(CallScope)
	attempt, _ := strconv.Atoi(req.Header.Get("X-Stainless-Retry-Count"))
	call := Call{Scope: scope, Model: c.model, RetryAttempt: max(0, attempt), ReservedTokens: int64(len(raw)) + output}
	if bytes.Contains(raw, []byte(`"image_url"`)) {
		if c.contextTokens <= 0 {
			cancel()
			return nil, ErrAdmissionUnavailable
		}
		call.ReservedTokens = max(call.ReservedTokens, c.contextTokens)
	}
	finish, err := c.admission.Acquire(ctx, call)
	if err != nil {
		cancel()
		return nil, err
	}
	started := time.Now()
	resp, err := c.upstream.Do(req)
	if err != nil {
		outcome := "failed"
		if ctx.Err() != nil {
			outcome = "cancelled"
		}
		finishAdmission(ctx, finish, CallResult{Outcome: outcome, Duration: time.Since(started)})
		cancel()
		return nil, err
	}
	resp.Body = &admittedBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, finish: finish, started: started, status: resp.StatusCode, streaming: params.Stream, responses: params.MaxOutputTokens > 0}
	return resp, nil
}

// Usage observation is independent of executable response validation. A model
// step may fail validation after the upstream has already billed its tokens.
// Only bounded usage metadata is retained; response text is never logged/stored.
type admittedBody struct {
	io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	finish    func(context.Context, CallResult) error
	started   time.Time
	status    int
	streaming bool
	responses bool
	once      sync.Once
	mu        sync.Mutex
	buffer    []byte
	discard   bool
	eof, done bool
	usage     *Usage
}

func (b *admittedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status >= 200 && b.status < 300 {
		if b.streaming {
			for _, part := range bytes.SplitAfter(p[:n], []byte{'\n'}) {
				limit := 64 << 10
				if b.responses {
					limit = 4 << 20
				}
				if len(b.buffer)+len(part) > limit {
					b.buffer, b.discard = nil, true
				}
				if !b.discard {
					b.buffer = append(b.buffer, part...)
				}
				if bytes.HasSuffix(part, []byte{'\n'}) {
					line := bytes.TrimSpace(b.buffer)
					if bytes.HasPrefix(line, []byte("data:")) {
						payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
						b.done = b.done || bytes.Equal(payload, []byte("[DONE]"))
						b.observeUsage(payload)
					}
					b.buffer, b.discard = nil, false
				}
			}
		} else if !b.discard {
			if len(b.buffer)+n > 4<<20 {
				b.buffer, b.discard = nil, true
			} else {
				b.buffer = append(b.buffer, p[:n]...)
			}
		}
	}
	b.eof = b.eof || err == io.EOF
	return n, err
}

type responsesUsage struct {
	Input   *int64 `json:"input_tokens"`
	Output  *int64 `json:"output_tokens"`
	Details struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (b *admittedBody) observeUsage(raw []byte) {
	var response struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &response) == nil && response.Type == "response.completed" && response.Response != nil {
		b.done = true
		if u := response.Response.Usage; u != nil && u.Input != nil && u.Output != nil && *u.Input >= 0 && *u.Output >= 0 && u.Details.Cached >= 0 && u.Details.Cached <= *u.Input && *u.Input <= 1_000_000_000 && *u.Output <= 1_000_000_000 {
			b.usage = &Usage{InputTokens: *u.Input, OutputTokens: *u.Output, CachedTokens: u.Details.Cached}
		}
		return
	}
	var frame struct {
		Usage *struct {
			Input   *int64 `json:"prompt_tokens"`
			Output  *int64 `json:"completion_tokens"`
			Details struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Usage == nil {
		return
	}
	u := frame.Usage
	if u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || u.Details.Cached < 0 || u.Details.Cached > *u.Input || *u.Input > 1_000_000_000 || *u.Output > 1_000_000_000 {
		return
	}
	b.usage = &Usage{InputTokens: *u.Input, OutputTokens: *u.Output, CachedTokens: u.Details.Cached}
}

func (b *admittedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		defer b.cancel()
		if !b.streaming && !b.discard {
			b.observeUsage(b.buffer)
		}
		outcome := "failed"
		if b.status >= 200 && b.status < 300 && (b.eof || b.done || b.usage != nil) {
			outcome = "completed"
		} else if b.ctx.Err() != nil {
			outcome = "cancelled"
		}
		finishAdmission(b.ctx, b.finish, CallResult{Outcome: outcome, HTTPStatus: b.status, Duration: time.Since(b.started), Usage: b.usage})
	})
	return err
}

func finishAdmission(ctx context.Context, finish func(context.Context, CallResult) error, result CallResult) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := finish(ctx, result); err != nil {
		// Database/gateway errors can carry private URLs. Keep the reservation
		// conservative until lease expiry and emit only a stable diagnostic.
		slog.Error("model call accounting unavailable; reservation retained")
	}
}
