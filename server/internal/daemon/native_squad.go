package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/llm"
)

// The server still checks current leadership and task provenance. The model
// cannot select another task, issue, squad or actor for its evaluation.
func (h *nativeToolHost) squadEvaluation(ctx context.Context, call llm.ToolCall) (string, error) {
	if !h.task.IsLeaderTask || !nativeResourceID(h.task.IssueID) || !nativeResourceID(h.task.SquadID) {
		return "", errors.New("squad evaluation requires an issue-bound leader run")
	}
	var args struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := decodeNativeToolArguments(call.Arguments, &args); err != nil {
		return "", err
	}
	if args.Outcome != "action" && args.Outcome != "no_action" && args.Outcome != "failed" || strings.TrimSpace(args.Reason) == "" || len(args.Reason) > 2000 {
		return "", errors.New("evaluation requires action, no_action or failed and a reason up to 2000 bytes")
	}
	return h.request(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(h.task.IssueID)+"/squad-evaluated", args)
}
