// Package jobservice owns independently supervised, authenticated command jobs.
// Client disconnects and Native task teardown do not own these processes.
package jobservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type Credential struct {
	Namespace string `json:"namespace"`
	Token     string `json:"token"`
}
type Config struct {
	Listen  string              `json:"listen"`
	WorkDir string              `json:"work_dir"`
	DataDir string              `json:"data_dir"`
	Keys    []Credential        `json:"keys"`
	Limits  tools.ServiceLimits `json:"limits"`
}

type Service struct {
	config Config
	host   *tools.Host
	keys   map[[32]byte]string
	ctx    context.Context
	cancel context.CancelFunc
	unlock func() error
	once   sync.Once
}

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func New(cfg Config, env []string) (*Service, error) {
	if !filepath.IsAbs(cfg.WorkDir) || !filepath.IsAbs(cfg.DataDir) || len(cfg.Keys) == 0 || len(cfg.Keys) > 32 {
		return nil, errors.New("job service requires absolute directories and 1 to 32 credentials")
	}
	keys := map[[32]byte]string{}
	names := map[string]bool{}
	for _, key := range cfg.Keys {
		digest := sha256.Sum256([]byte(key.Token))
		if !identityPattern.MatchString(key.Namespace) || len(key.Token) < 32 || len(key.Token) > 512 || strings.ContainsAny(key.Token, " \r\n\t") || keys[digest] != "" || names[key.Namespace] {
			return nil, errors.New("job credentials require unique namespaces and unique tokens of at least 32 characters")
		}
		keys[digest] = key.Namespace
		names[key.Namespace] = true
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(cfg.DataDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("job service data directory must be private (0700)")
	}
	unlock, err := lockDirectory(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	host, err := tools.NewServiceHost(cfg.WorkDir, env, cfg.DataDir, cfg.Limits)
	if err != nil {
		cancel()
		_ = unlock()
		return nil, err
	}
	s := &Service{config: cfg, host: host, keys: keys, ctx: ctx, cancel: cancel, unlock: unlock}
	// Reconcile records before accepting requests. A second service cannot
	// acquire the directory lock and misclassify live admissions as interrupted.
	for namespace := range names {
		if _, err := host.Execute(s.ownerContext(namespace), llm.ToolCall{Name: "job_list", Arguments: json.RawMessage(`{}`)}); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Service) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = errors.Join(s.host.Close(), s.unlock()) })
	return err
}

func (s *Service) ownerContext(namespace string) context.Context {
	return agentstate.WithWriteClaim(s.ctx, agentstate.Session{ID: namespace}, "")
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		respond(w, 200, map[string]bool{"ready": s.ctx.Err() == nil})
		return
	}
	authorization := r.Header.Get("Authorization")
	namespace := s.keys[sha256.Sum256([]byte(strings.TrimPrefix(authorization, "Bearer ")))]
	if !strings.HasPrefix(authorization, "Bearer ") || namespace == "" {
		respond(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	if s.ctx.Err() != nil {
		respond(w, 503, map[string]string{"error": "service shutting down"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/environment") {
		s.environment(w, r, namespace)
		return
	}
	ctx := s.ownerContext(namespace)
	var call llm.ToolCall
	var args any
	path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(path) < 2 || path[0] != "v1" {
		respond(w, 404, map[string]string{"error": "not found"})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
		var request struct {
			Key     string `json:"key"`
			Command string `json:"command"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 40<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !identityPattern.MatchString(request.Key) || strings.TrimSpace(request.Command) == "" || len(request.Command) > 32768 {
			respond(w, 400, map[string]string{"error": "key and command are required; unknown fields rejected"})
			return
		}
		call = llm.ToolCall{ID: request.Key, Name: "shell"}
		args = map[string]any{"command": request.Command, "run_in_background": true}
	case r.Method == http.MethodGet && len(path) == 3 && path[1] == "submissions" && identityPattern.MatchString(path[2]):
		out, found, err := s.host.FindSubmission(ctx, path[2])
		if err != nil {
			respond(w, 500, map[string]string{"error": "cannot read submission journal"})
			return
		}
		if !found {
			respond(w, 404, map[string]string{"error": "submission not admitted"})
			return
		}
		respond(w, 200, json.RawMessage(out))
		return
	case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
		out, err := s.host.Execute(ctx, llm.ToolCall{Name: "job_list", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			respond(w, 500, map[string]string{"error": "cannot read job journal"})
			return
		}
		var jobs []map[string]any
		if json.Unmarshal([]byte(out), &jobs) != nil {
			respond(w, 500, map[string]string{"error": "invalid job journal"})
			return
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i]["job_id"].(string) < jobs[j]["job_id"].(string) })
		limit := 20
		if raw := r.URL.Query().Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				respond(w, 400, map[string]string{"error": "limit must be 1 to 100"})
				return
			}
			limit = value
		}
		cursor := r.URL.Query().Get("after")
		selected := []map[string]any{}
		more := false
		for _, job := range jobs {
			if job["job_id"].(string) <= cursor {
				continue
			}
			if len(selected) == limit {
				more = true
				break
			}
			selected = append(selected, job)
		}
		next := ""
		if more {
			next = selected[len(selected)-1]["job_id"].(string)
		}
		respond(w, 200, map[string]any{"jobs": selected, "next_cursor": next})
		return
	case len(path) >= 3 && path[1] == "jobs" && identityPattern.MatchString(path[2]):
		values := map[string]any{"job_id": path[2]}
		if len(path) == 3 && r.Method == http.MethodGet {
			call.Name = "job_output"
			values["wait_ms"] = 0
			if raw := r.URL.Query().Get("wait_ms"); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil || value < 0 || value > 30000 {
					respond(w, 400, map[string]string{"error": "wait_ms must be 0 to 30000"})
					return
				}
				values["wait_ms"] = value
			}
		} else if len(path) == 4 && path[3] == "cancel" && r.Method == http.MethodPost {
			call.Name = "job_kill"
		} else if len(path) == 4 && path[3] == "search" && r.Method == http.MethodGet {
			call.Name = "job_search"
			values["text"] = r.URL.Query().Get("text")
		} else {
			respond(w, 404, map[string]string{"error": "not found"})
			return
		}
		if raw := r.URL.Query().Get("offset"); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				respond(w, 400, map[string]string{"error": "invalid offset"})
				return
			}
			values["offset"] = value
		}
		args = values
	default:
		respond(w, 404, map[string]string{"error": "not found"})
		return
	}
	call.Arguments, _ = json.Marshal(args)
	// The lifetime context is deliberately independent of the HTTP request.
	// A lost response is recovered by GET submissions/<key>, never a new key.
	out, err := s.host.Execute(ctx, call)
	if err != nil {
		status := 400
		if strings.Contains(err.Error(), "not found") {
			status = 404
		}
		if strings.Contains(err.Error(), "limit reached") {
			status = 429
		}
		if errors.Is(err, tools.ErrOutcomeUnknown) {
			status = 503
		}
		respond(w, status, map[string]string{"error": fmt.Sprint(err)})
		return
	}
	var result any
	decoder := json.NewDecoder(bytes.NewBufferString(out))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		respond(w, 500, map[string]string{"error": "invalid job observation"})
		return
	}
	respond(w, 200, result)
}
