package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func (h *External) persistentDefinitions() []llm.Tool {
	if h == nil {
		return nil
	}
	if _, ok := h.services["multica_jobs"]; !ok {
		return nil
	}
	return []llm.Tool{{Name: "persistent_job", Description: "Manage independently hosted jobs that outlive Native tasks. Actions: submit(command), list(after?,limit?), output(job_id,offset?,wait_ms?), search(job_id,text,offset?), cancel(job_id), lookup(call_id). Submit/cancel require service write permission. Commands run with the service account's authority in its configured directory. Submission identity is engine-owned. After an uncertain submit use lookup with the ORIGINAL call_id or list, never submit again. Copy job_id exactly; preserve it in the issue. Continue logs/search with next_offset. Service restart marks unsettled work interrupted without replay. Cancelling SSH does not prove remote work stopped.", Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["submit","list","output","search","cancel","lookup"]},"command":{"type":"string","minLength":1,"maxLength":32768},"job_id":{"type":"string"},"call_id":{"type":"string"},"text":{"type":"string","minLength":1,"maxLength":512},"offset":{"type":"integer","minimum":0},"wait_ms":{"type":"integer","minimum":0,"maximum":20000},"after":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["action"],"additionalProperties":false}`)}}
}

func submissionKey(ctx context.Context, callID string) (string, error) {
	claim, ok := agentstate.WriteClaimFromContext(ctx)
	if !ok || claim.SessionID == "" || callID == "" || len(callID) > 200 {
		return "", errors.New("persistent jobs require an engine-owned session and call identity")
	}
	return jobHash(claim.SessionID + "\x00" + callID), nil
}

func (h *External) persistentExecute(ctx context.Context, call llm.ToolCall) (string, error) {
	service, ok := h.services["multica_jobs"]
	if !ok {
		return "", errors.New("independent job service is not configured")
	}
	var args struct {
		Action  string `json:"action"`
		Command string `json:"command"`
		ID      string `json:"job_id"`
		CallID  string `json:"call_id"`
		Text    string `json:"text"`
		Offset  *int64 `json:"offset"`
		WaitMS  *int   `json:"wait_ms"`
		After   string `json:"after"`
		Limit   *int   `json:"limit"`
	}
	var fields map[string]json.RawMessage
	if len(call.Arguments) > 40<<10 || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil || json.Unmarshal(call.Arguments, &args) != nil {
		return "", errors.New("invalid persistent job arguments")
	}
	allowed := map[string]bool{"action": true}
	var keys []string
	switch args.Action {
	case "submit":
		keys = []string{"command"}
	case "list":
		keys = []string{"after", "limit"}
	case "lookup":
		keys = []string{"call_id"}
	case "output":
		keys = []string{"job_id", "offset", "wait_ms"}
	case "search":
		keys = []string{"job_id", "text", "offset"}
	case "cancel":
		keys = []string{"job_id"}
	default:
		return "", errors.New("unsupported persistent job action")
	}
	for _, key := range keys {
		allowed[key] = true
	}
	for key, value := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", errors.New("unexpected or null persistent job argument for this action")
		}
	}
	method, path := "GET", "v1/jobs"
	var body any
	query := url.Values{}
	switch args.Action {
	case "submit":
		if !service.AllowWrite {
			return "", errors.New("independent job service is read-only")
		}
		if strings.TrimSpace(args.Command) == "" || len(args.Command) > 32768 {
			return "", errors.New("command must contain 1 to 32768 bytes")
		}
		key, err := submissionKey(ctx, call.ID)
		if err != nil {
			return "", err
		}
		if h.ObserveSubmission != nil {
			if err := h.ObserveSubmission(ctx, key); err != nil {
				return "", err
			}
		}
		method, body = "POST", map[string]any{"key": key, "command": args.Command}
	case "lookup":
		key, err := submissionKey(ctx, args.CallID)
		if err != nil {
			return "", err
		}
		path = "v1/submissions/" + key
	case "list":
		if args.After != "" {
			query.Set("after", args.After)
		}
		if args.Limit != nil {
			if *args.Limit < 1 || *args.Limit > 100 {
				return "", errors.New("limit must be 1 to 100")
			}
			query.Set("limit", strconv.Itoa(*args.Limit))
		}
	default:
		if !validJobResource(args.ID) {
			return "", errors.New("copy a complete job_id from persistent_job action=list")
		}
		path += "/" + args.ID
		if args.Action == "cancel" {
			method = "POST"
			path += "/cancel"
		}
		if args.Action == "search" {
			if args.Text == "" || len(args.Text) > 512 {
				return "", errors.New("text must contain 1 to 512 bytes")
			}
			path += "/search"
			query.Set("text", args.Text)
		}
		if args.Offset != nil {
			if *args.Offset < 0 {
				return "", errors.New("offset must be nonnegative")
			}
			query.Set("offset", strconv.FormatInt(*args.Offset, 10))
		}
		if args.WaitMS != nil {
			if *args.WaitMS < 0 || *args.WaitMS > 20000 {
				return "", errors.New("wait_ms must be 0 to 20000")
			}
			query.Set("wait_ms", strconv.Itoa(*args.WaitMS))
		}
	}
	if method == "POST" && !service.AllowWrite {
		return "", errors.New("independent job service is read-only")
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	request := map[string]any{"service": "multica_jobs", "method": method, "path": path}
	if body != nil {
		request["body"] = body
	}
	raw, _ := json.Marshal(request)
	out, err := h.executeHTTP(ctx, llm.ToolCall{Name: "http_request", ID: call.ID, Arguments: raw}, true)
	if err != nil {
		return out, err
	}
	var envelope struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if json.Unmarshal([]byte(out), &envelope) != nil || !validPersistentResponse(args.Action, []byte(envelope.Body)) {
		if method == "POST" {
			return "", ErrOutcomeUnknown
		}
		return "", errors.New("invalid independent job service response")
	}
	return envelope.Body, nil
}

func validJobResource(id string) bool {
	if len(id) > 100 || id == "" {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// RecoverPersistentTool reads an admission receipt; it never submits a command.
func (h *External) RecoverPersistentTool(ctx context.Context, call llm.ToolCall) (string, bool, error) {
	if h == nil || call.Name != "persistent_job" {
		return "", false, nil
	}
	if _, ok := h.services["multica_jobs"]; !ok {
		return "", false, nil
	}
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(call.Arguments, &args) != nil || args.Action != "submit" {
		return "", false, nil
	}
	key, err := submissionKey(ctx, call.ID)
	if err != nil {
		return "", false, err
	}
	raw, _ := json.Marshal(map[string]any{"service": "multica_jobs", "method": "GET", "path": "v1/submissions/" + key})
	out, err := h.executeHTTP(ctx, llm.ToolCall{Name: "http_request", Arguments: raw}, true)
	var envelope struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if json.Unmarshal([]byte(out), &envelope) == nil && envelope.Status == 404 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !validPersistentResponse("lookup", []byte(envelope.Body)) {
		return "", false, errors.New("invalid independent job receipt")
	}
	return envelope.Body, true, nil
}

func validPersistentResponse(action string, raw []byte) bool {
	type observation struct {
		ID     string `json:"job_id"`
		Status string `json:"status"`
	}
	valid := func(v observation) bool {
		if !validJobResource(v.ID) {
			return false
		}
		switch v.Status {
		case "starting", "running", "completed", "failed", "cancelled", "interrupted":
			return true
		}
		return false
	}
	switch action {
	case "list":
		var page struct {
			Jobs []observation `json:"jobs"`
			Next string        `json:"next_cursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Jobs == nil || len(page.Jobs) > 100 || page.Next != "" && !validJobResource(page.Next) {
			return false
		}
		for _, job := range page.Jobs {
			if !valid(job) {
				return false
			}
		}
		return true
	case "search":
		var page struct {
			ID      string            `json:"job_id"`
			Matches []json.RawMessage `json:"matches"`
			Next    *int64            `json:"next_offset"`
		}
		return json.Unmarshal(raw, &page) == nil && validJobResource(page.ID) && page.Matches != nil && len(page.Matches) <= 20 && page.Next != nil && *page.Next >= 0
	default:
		var job observation
		return json.Unmarshal(raw, &job) == nil && valid(job)
	}
}
