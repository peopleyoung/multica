package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

// External exposes only configured services; the model never supplies a host,
// credential, arbitrary header or redirect target. There are no implicit retries.
type External struct {
	// ObserveSubmission durably registers engine-owned admission before dispatch.
	ObserveSubmission func(context.Context, string) error
	services          map[string]nativeruntime.Service
	tokens            map[string]string
}

func NewExternal(cfg nativeruntime.Config, limits nativeruntime.Limits, env map[string]string) (*External, error) {
	if err := cfg.Check(limits); err != nil {
		return nil, err
	}
	h := &External{services: map[string]nativeruntime.Service{}, tokens: map[string]string{}}
	for _, s := range cfg.Services {
		if s.Name == "multica_jobs" && (s.TokenEnv == "" || s.AuthScheme != "" && s.AuthScheme != "Bearer") {
			return nil, errors.New("multica_jobs requires a private Bearer token environment variable")
		}
		if s.TokenEnv != "" {
			token := env[s.TokenEnv]
			if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
				return nil, errors.New("external service credential is missing or invalid; configure the referenced agent environment variable")
			}
			h.tokens[s.Name] = token
		}
		h.services[s.Name] = s
	}
	return h, nil
}

func (h *External) Definitions() []llm.Tool {
	return append(h.httpDefinitions(), h.persistentDefinitions()...)
}

func (h *External) httpDefinitions() []llm.Tool {
	if h == nil || len(h.services) == 0 {
		return nil
	}
	return []llm.Tool{{Name: "http_request", Description: "Call a configured external service such as CVAT using a relative API path. Credentials are injected privately. GET is read-only; other methods require service write permission. No redirects or retries. Inspect the status and body; never retry an uncertain write.", Parameters: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"},"method":{"type":"string","enum":["GET","POST","PUT","PATCH","DELETE"]},"path":{"type":"string"},"body":{"type":"object"}},"required":["service","method","path"],"additionalProperties":false}`)}}
}

func (h *External) IsReadOnly(call llm.ToolCall) bool {
	if call.Name == "persistent_job" {
		var args struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(call.Arguments, &args) != nil {
			return false
		}
		switch args.Action {
		case "list", "output", "search", "lookup":
			return true
		}
		return false
	}
	var args struct {
		Method string `json:"method"`
	}
	return call.Name == "http_request" && json.Unmarshal(call.Arguments, &args) == nil && args.Method == http.MethodGet
}

func (h *External) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if call.Name == "persistent_job" {
		return h.persistentExecute(ctx, call)
	}
	return h.executeHTTP(ctx, call, false)
}

func (h *External) executeHTTP(ctx context.Context, call llm.ToolCall, jobService bool) (string, error) {
	var args struct {
		Service string         `json:"service"`
		Method  string         `json:"method"`
		Path    string         `json:"path"`
		Body    map[string]any `json:"body"`
	}
	dec := json.NewDecoder(bytes.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if len(call.Arguments) > 64<<10 || dec.Decode(&args) != nil || dec.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid external request arguments")
	}
	if args.Service == "multica_jobs" && !jobService {
		return "", errors.New("use persistent_job for multica_jobs; admission identity is owned by the runtime")
	}
	s, ok := h.services[args.Service]
	if !ok {
		return "", errors.New("external service is not configured")
	}
	switch args.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return "", errors.New("unsupported external request method")
	}
	write := args.Method != http.MethodGet
	if write && !s.AllowWrite {
		return "", errors.New("external service is read-only")
	}
	relative, err := url.Parse(args.Path)
	if err != nil {
		return "", errors.New("invalid external request path")
	}
	cleanPath := strings.TrimSuffix(relative.Path, "/")
	if relative.IsAbs() || relative.Host != "" || relative.User != nil || relative.Fragment != "" || relative.RawPath != "" || strings.HasPrefix(args.Path, "/") || strings.ContainsAny(args.Path, "\\\r\n") || cleanPath == "" || path.Clean(cleanPath) != cleanPath || strings.HasPrefix(relative.Path, "..") {
		return "", errors.New("external request path must be relative and stay beneath the configured service base")
	}
	if !write && args.Body != nil {
		return "", errors.New("GET does not accept a request body")
	}
	base, _ := url.Parse(strings.TrimRight(s.BaseURL, "/") + "/")
	target := base.ResolveReference(relative)
	// Reject dot segments including encoded forms before resolving.
	if nativeruntime.Origin(target.String()) != nativeruntime.Origin(s.BaseURL) || !strings.HasPrefix(target.Path, base.Path) {
		return "", errors.New("external request escaped the configured base")
	}
	var body io.Reader
	if args.Body != nil {
		raw, _ := json.Marshal(args.Body)
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, args.Method, target.String(), body)
	if err != nil {
		return "", errors.New("invalid external request")
	}
	req.Header.Set("Accept", "application/json")
	if args.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := h.tokens[s.Name]; token != "" {
		scheme := s.AuthScheme
		if scheme == "" {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	failure := func() (string, error) {
		if write {
			return "", ErrOutcomeUnknown
		}
		return "", errors.New("external service request failed; check connectivity or response size")
	}
	response, err := client.Do(req)
	if err != nil {
		return failure()
	}
	defer response.Body.Close()
	if write && response.StatusCode >= 500 {
		return failure()
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if write {
			return failure()
		}
		return "", errors.New("external service redirect rejected; configure the final service base URL")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, outputLimit+1))
	if err != nil || len(raw) > outputLimit {
		return failure()
	}
	text := strings.ToValidUTF8(string(raw), "�")
	for _, token := range h.tokens {
		if token != "" {
			text = strings.ReplaceAll(text, token, "[REDACTED]")
		}
	}
	result, _ := json.Marshal(map[string]any{"status": response.StatusCode, "body": text})
	if response.StatusCode >= 400 {
		return string(result), fmt.Errorf("external service returned HTTP %d; inspect the response before another operation", response.StatusCode)
	}
	return string(result), nil
}
