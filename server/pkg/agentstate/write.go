package agentstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/pkg/llm"
)

const NativeWriteHeader = "X-Multica-Native-Write"

type WriteClaim struct {
	SessionID  string `json:"session_id"`
	LeaseToken string `json:"lease_token"`
	Version    int64  `json:"version"`
	CallID     string `json:"call_id"`
}

type writeContextKey struct{}

// WithWriteClaim carries engine-owned identity; model arguments cannot choose
// the operation key, execution lease, or session version.
func WithWriteClaim(ctx context.Context, session Session, callID string) context.Context {
	return context.WithValue(ctx, writeContextKey{}, WriteClaim{session.ID, session.LeaseToken, session.Version, callID})
}

func WriteClaimFromContext(ctx context.Context) (WriteClaim, bool) {
	claim, ok := ctx.Value(writeContextKey{}).(WriteClaim)
	return claim, ok
}

func NativeWriteTool(name string) bool {
	switch name {
	case "multica_create_issue", "multica_create_comment", "multica_update_issue_status", "multica_record_squad_evaluation", "multica_upload_artifact", "multica_manage_project", "multica_manage_agent", "multica_manage_squad", "multica_manage_autopilot":
		return true
	}
	return false
}

func WriteKey(inputID string, call llm.ToolCall, quickCreate bool) string {
	identity := inputID + ":" + call.ID
	if quickCreate && call.Name == "multica_create_issue" {
		identity = inputID + ":quick_create"
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

type WriteOutcome struct {
	State  string          `json:"state"`
	Digest string          `json:"digest"`
	CallID string          `json:"call_id"`
	Status int             `json:"status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type WriteResultRequest struct {
	Claim   llm.NativeRequest `json:"claim"`
	Session Session           `json:"session"`
	CallID  string            `json:"call_id"`
}

type WriteResult struct {
	Known   bool            `json:"known"`
	Status  int             `json:"status,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Session *Session        `json:"session,omitempty"`
}

// ToolOutput is shared by ordinary upload delivery and receipt reconciliation,
// so a recovered result has the same model-visible shape as the first result.
func (r WriteResult) ToolOutput(name string) (string, bool, error) {
	if !r.Known || r.Status < 200 || r.Status >= 500 || !json.Valid(r.Body) {
		return "", false, errors.New("invalid confirmed native write result")
	}
	if r.Status >= 300 {
		message := fmt.Sprintf("business API returned HTTP %d; refresh the resource before another operation", r.Status)
		if name == "multica_upload_artifact" {
			message = "artifact upload rejected; check storage configuration and task permissions"
		}
		return "Tool error: " + message, true, nil
	}
	if name == "multica_upload_artifact" {
		var artifact struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
		}
		if json.Unmarshal(r.Body, &artifact) != nil || artifact.ID == "" || len(artifact.ID) > 200 {
			return "", false, errors.New("invalid confirmed artifact")
		}
		for _, c := range artifact.ID {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false, errors.New("invalid confirmed artifact identifier")
			}
		}
		output, err := json.Marshal(map[string]any{"type": "attachment", "id": artifact.ID, "filename": artifact.Filename, "download_path": "/api/attachments/" + artifact.ID + "/download", "delivery": "attached to the current issue or producing chat run; external channel delivery follows that channel's capabilities"})
		return string(output), false, err
	}
	// jsonb and HTTP normalize whitespace, key order and HTML escaping. Build
	// identical model-visible results without rounding integer revisions.
	decoder := json.NewDecoder(bytes.NewReader(r.Body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false, err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", false, err
	}
	if len(body) > 64<<10 {
		return strings.ToValidUTF8(string(body[:64<<10]), "�") + "\n[business response truncated]", false, nil
	}
	return string(body), false, nil
}
