package daemon

import (
	"encoding/json"
	"errors"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func (h *nativeToolHost) runtimeInfo(call llm.ToolCall) (string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil || args == nil || len(args) != 0 {
		return "", errors.New("native_runtime_info accepts an empty object")
	}
	services := []map[string]any{}
	for _, s := range h.runtimeConfig.Services {
		services = append(services, map[string]any{"name": s.Name, "base_url": s.BaseURL, "allow_write": s.AllowWrite, "authentication_configured": s.TokenEnv != ""})
	}
	info := map[string]any{"file_paths": "relative to the working directory", "file_read_limit_bytes": 64 << 10, "file_pagination": "absolute byte offsets", "artifact_upload_limit_bytes": 1 << 20, "local_job_log_limit_bytes": 8 << 20, "local_job_log_search": true, "job_completion_notifications": "current execution; durable model history deduplicates delivery", "persistent_jobs": "available only when the named multica_jobs service is configured; independent service lifecycle", "tool_output_limit_bytes": 64 << 10, "write_files": h.permissions.WriteFiles, "shell": h.permissions.Shell, "shell_execution": "managed_jobs", "shell_wait_default_ms": tools.ShellWaitDefaultMS, "shell_wait_max_ms": tools.ShellWaitMaxMS, "shell_max_active_jobs": tools.ShellMaxActiveJobs, "shell_job_lifetime": "current task execution; interrupted records never restart commands", "services": services, "issue_id": h.task.IssueID, "reply_parent_id": h.task.TriggerCommentID, "coalesced_comment_ids": h.task.CoalescedCommentIDs}
	info["execution_environment"] = "local"
	info["interactive_terminal"] = "Linux PTY; per-run ownership; readiness unknown; ten-minute idle expiry"
	if remote, ok := h.Environment.(*tools.RemoteEnvironment); ok {
		info["execution_environment"] = "multica_jobs"
		info["remote_environment"] = remote.Info
		info["shell_job_lifetime"] = "independent service; survives Native task; service restart interrupts unsettled work without replay"
	}
	info["job_completion_notifications"] = "persistent submissions: durable observation after source run; idempotent follow-up with current invocation checks; issue sources only"
	raw, _ := json.Marshal(info)
	return string(raw), nil
}
