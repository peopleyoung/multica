package daemon

import "context"

// agents returns the configured native runtime descriptor.
func (d *Daemon) agents() map[string]AgentEntry { return d.cfg.Agents }

// skippedAgentsSnapshot retains the health response field for installed clients.
func (d *Daemon) skippedAgentsSnapshot() map[string]string { return nil }

// deregisterDroppedRuntimes runs under the workspace registration lock.
func (d *Daemon) deregisterDroppedRuntimes(ctx context.Context, workspaceID string, runtimeIDs []string, reason string, offlineReasons map[string]RuntimeOfflineReason) {
	runtimeIDs = d.untrackedRuntimeIDs(runtimeIDs)
	if len(runtimeIDs) == 0 {
		return
	}
	if err := d.client.Deregister(ctx, runtimeIDs, offlineReasons); err != nil {
		d.logger.Warn("deregister of dropped runtimes failed",
			"workspace_id", workspaceID, "runtime_ids", runtimeIDs, "reason", reason, "error", err)
	}
}
