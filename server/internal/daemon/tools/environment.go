package tools

import (
	"context"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Environment keeps file and subprocess operations in the same execution world.
// Local and remote implementations share this contract; platform APIs stay out.
type Environment interface {
	Definitions() []llm.Tool
	Execute(context.Context, llm.ToolCall) (string, error)
	IsReadOnly(llm.ToolCall) bool
	Rules(context.Context, string) (string, error)
	ReadArtifact(string) ([]byte, error)
	RecoverTool(context.Context, llm.ToolCall) (string, bool, error)
	PendingJobs(context.Context) ([]string, error)
	JobNotifications(context.Context, []llm.ModelMessage) ([]string, error)
	Close() error
}
