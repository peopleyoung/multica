package daemon

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func (h *nativeToolHost) RecoverTool(ctx context.Context, call llm.ToolCall) (string, bool, error) {
	if call.Name == "persistent_job" && h.external != nil {
		return h.external.RecoverPersistentTool(ctx, call)
	}
	return h.Environment.RecoverTool(ctx, call)
}
