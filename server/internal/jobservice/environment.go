package jobservice

import (
	"encoding/json"
	"errors"
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
	"io"
	"net/http"
	"runtime"
)

func (s *Service) environment(w http.ResponseWriter, r *http.Request, namespace string) {
	if r.Method == "GET" && r.URL.Path == "/v1/environment" {
		mode := s.config.Limits.Sandbox.Mode
		if mode == "" {
			mode = "unconfined"
		}
		respond(w, 200, map[string]any{"work_dir": s.config.WorkDir, "platform": runtime.GOOS, "write_files": s.config.Limits.WriteFiles, "sandbox": mode, "network": s.config.Limits.Sandbox.Network || mode == "unconfined"})
		return
	}
	if r.Method != "POST" {
		respond(w, 405, map[string]string{"error": "POST required"})
		return
	}
	decode := func(v any) bool {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		dec.DisallowUnknownFields()
		return dec.Decode(v) == nil && dec.Decode(new(any)) == io.EOF
	}
	if r.URL.Path == "/v1/environment/artifact" {
		var a struct {
			Path string `json:"path"`
		}
		if !decode(&a) {
			respond(w, 400, map[string]string{"error": "invalid artifact path"})
			return
		}
		data, err := s.host.ReadArtifact(a.Path)
		if err != nil {
			respond(w, 400, map[string]string{"error": "artifact unavailable"})
			return
		}
		respond(w, 200, map[string]any{"data": data})
		return
	}
	if r.URL.Path != "/v1/environment/tools" {
		respond(w, 404, map[string]string{"error": "not found"})
		return
	}
	var a struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Scope     string          `json:"scope"`
		Key       string          `json:"key"`
	}
	if !decode(&a) {
		respond(w, 400, map[string]string{"error": "invalid environment request"})
		return
	}
	allowed := false
	for _, d := range s.host.Definitions() {
		if d.Name == a.Name {
			allowed = true
		}
	}
	if !allowed {
		respond(w, 403, map[string]string{"error": "tool unavailable"})
		return
	}
	ctx := s.ownerContext(namespace)
	if a.Name == "terminal" {
		if !identityPattern.MatchString(a.Scope) {
			respond(w, 400, map[string]string{"error": "engine session scope required"})
			return
		}
		ctx = agentstate.WithWriteClaim(s.ctx, agentstate.Session{ID: namespace + ":" + a.Scope}, "")
	}
	if a.Name == "shell" && !identityPattern.MatchString(a.Key) {
		respond(w, 400, map[string]string{"error": "admission identity required"})
		return
	}
	out, err := s.host.Execute(ctx, llm.ToolCall{ID: a.Key, Name: a.Name, Arguments: a.Arguments})
	if err != nil {
		status := 400
		if errors.Is(err, tools.ErrOutcomeUnknown) {
			status = 503
		}
		respond(w, status, map[string]string{"error": "environment tool failed; reconcile writes before retrying"})
		return
	}
	respond(w, 200, map[string]string{"output": out})
}
