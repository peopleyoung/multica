package nativemigration

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type Binding struct {
	Source string `json:"source_runtime_id"`
	Target string `json:"target_runtime_id"`
}

// Receipt contains identities and counts only, never configuration values or
// external session identifiers. The committed receipt is the retry result.
type Receipt struct {
	OperationID string     `json:"operation_id"`
	WorkspaceID string     `json:"workspace_id"`
	State       string     `json:"state"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Bindings    []Binding  `json:"bindings"`
	Agents      []string   `json:"agent_ids"`
	Runs        []string   `json:"run_ids"`
	Profiles    []string   `json:"profile_ids"`
}

type Options struct {
	WorkspaceID    string
	OperationID    string
	Action         string
	ClearCLIConfig bool
}

// Migrate is an explicitly invoked maintenance operation. Drain commits before
// apply so existing executions can finish without holding a database lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool, opts Options) (Receipt, error) {
	workspace, err := uuid.Parse(opts.WorkspaceID)
	if err != nil {
		return Receipt{}, errors.New("workspace_id must be a UUID")
	}
	operation, err := uuid.Parse(opts.OperationID)
	if err != nil || operation == uuid.Nil {
		return Receipt{}, errors.New("operation_id must be a nonzero UUID")
	}
	if opts.Action != "drain" && opts.Action != "apply" && opts.Action != "resume" && opts.Action != "status" {
		return Receipt{}, errors.New("unsupported migration action")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Receipt{}, errors.New("cannot begin migration")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
		return Receipt{}, errors.New("cannot configure migration lock timeout")
	}
	// Apply is a short maintenance transaction. Freeze writers before taking
	// the workspace lock to avoid agent->workspace claim lock inversion. This
	// also fences enqueues and runtime/permission changes during validation.
	if opts.Action == "apply" {
		if _, err := tx.Exec(ctx, `LOCK TABLE agent_task_queue, agent, agent_runtime, chat_session, native_session, model_call, runtime_profile IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return Receipt{}, errors.New("migration writers are busy; retry during the maintenance window")
		}
	}
	var state string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT native_migration_state,native_migration_receipt FROM workspace WHERE id=$1 FOR UPDATE`, workspace).Scan(&state, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Receipt{}, errors.New("workspace not found")
		}
		return Receipt{}, errors.New("cannot lock migration state; verify database migrations")
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Receipt{}, errors.New("invalid stored migration receipt")
	}
	if opts.Action == "status" {
		receipt.State, receipt.WorkspaceID = state, workspace.String()
		return receipt, nil
	}
	if receipt.OperationID != "" && receipt.OperationID != operation.String() && !(opts.Action == "drain" && state == "none") {
		return Receipt{}, errors.New("operation_id does not match the current migration")
	}
	if state == "native" {
		if opts.Action == "apply" || opts.Action == "drain" {
			return receipt, nil
		}
		return Receipt{}, errors.New("migration already applied; resume cannot downgrade native execution")
	}
	switch opts.Action {
	case "drain":
		if state == "draining" {
			return receipt, nil
		}
		receipt = Receipt{OperationID: operation.String(), WorkspaceID: workspace.String(), State: "draining", Bindings: []Binding{}, Agents: []string{}, Runs: []string{}, Profiles: []string{}}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&receipt.StartedAt); err != nil {
			return Receipt{}, errors.New("cannot read migration timestamp")
		}
	case "resume":
		if state != "draining" {
			return Receipt{}, errors.New("workspace is not draining")
		}
		receipt.State = "none"
	case "apply":
		if state != "draining" {
			return Receipt{}, errors.New("drain the workspace before applying migration")
		}
		if err := apply(ctx, tx, workspace, opts.ClearCLIConfig, &receipt); err != nil {
			return Receipt{}, err
		}
		receipt.State = "native"
		var completed time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&completed); err != nil {
			return Receipt{}, errors.New("cannot read migration timestamp")
		}
		receipt.CompletedAt = &completed
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return Receipt{}, errors.New("cannot encode migration receipt")
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace SET native_migration_state=$2,native_migration_receipt=$3,updated_at=now() WHERE id=$1`, workspace, receipt.State, encoded); err != nil {
		return Receipt{}, errors.New("cannot persist migration receipt")
	}
	if err := tx.Commit(ctx); err != nil {
		return Receipt{}, errors.New("cannot commit migration; retry with the same operation_id")
	}
	return receipt, nil
}

func apply(ctx context.Context, tx pgx.Tx, workspace uuid.UUID, clear bool, receipt *Receipt) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE a.workspace_id=$1 AND t.status IN ('running','dispatched','waiting_local_directory'))
 OR EXISTS(SELECT 1 FROM native_session WHERE workspace_id=$1 AND lease_until>now())
 OR EXISTS(SELECT 1 FROM model_call WHERE workspace_id=$1 AND outcome='active' AND lease_until>now())`, workspace).Scan(&active); err != nil {
		return errors.New("cannot verify drained executions")
	}
	if active {
		return errors.New("workspace has active executions or leases; finish draining before apply")
	}
	var needsReview bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_profile WHERE workspace_id=$1 AND enabled)`, workspace).Scan(&needsReview); err != nil {
		return errors.New("cannot validate CLI profiles")
	}
	if needsReview && !clear {
		return errors.New("CLI configuration requires review; use --clear-cli-config only after translating required tools")
	}
	runtimes, err := query[Runtime](ctx, tx, runtimeQuery, workspace)
	if err != nil {
		return err
	}
	agents, err := query[Agent](ctx, tx, agentQuery, workspace)
	if err != nil {
		return err
	}
	r := Report{Runtimes: runtimes}
	assess(&r)
	byID := map[string]Runtime{}
	for _, rt := range runtimes {
		byID[rt.ID] = rt
	}
	bindings := map[string]string{}
	for _, a := range agents {
		rt, ok := byID[a.RuntimeID]
		if !ok {
			return errors.New("an agent has no workspace runtime binding; configure it before migration")
		}
		if a.OwnerID == "" || rt.OwnerID == "" || (rt.Visibility == "private" && a.OwnerID != rt.OwnerID) {
			return errors.New("agent runtime ownership requires review")
		}
		if (a.HasCLIConfig || a.HasModelOverride || rt.ProfileID != "") && !clear {
			return errors.New("CLI configuration requires review; use --clear-cli-config only after translating required tools")
		}
		target := rt
		if rt.Provider != "native" && len(rt.TargetIDs) != 1 {
			return errors.New("legacy runtime needs exactly one native target with the same machine, owner and visibility")
		}
		if rt.Provider != "native" {
			target = byID[rt.TargetIDs[0]]
		}
		if target.ProfileID != "" || target.DaemonID == "" {
			return errors.New("native target cannot use a CLI profile or omit machine identity")
		}
		var compatible bool
		if err := tx.QueryRow(ctx, `SELECT status='online' AND last_seen_at>=now()-interval '90 seconds'
 AND jsonb_typeof(metadata->'capabilities')='array' AND COALESCE(metadata->'capabilities','[]'::jsonb) ? $2
 FROM agent_runtime WHERE id=$1::uuid AND workspace_id=$3`, target.ID, protocol.DaemonCapabilityNativeRuntimeV1, workspace).Scan(&compatible); err != nil || !compatible {
			return errors.New("native target must be online, recently registered and support native-runtime-v1")
		}
		if rt.Provider != "native" {
			var hasCheckpoint bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM native_session WHERE workspace_id=$1 AND agent_id=$2::uuid)`, workspace, a.ID).Scan(&hasCheckpoint); err != nil || hasCheckpoint {
				return errors.New("legacy agent has an existing native checkpoint; reconcile its session history before migration")
			}
			bindings[rt.ID] = target.ID
		}
	}
	// Reject queued work whose saved binding already disagrees with its agent;
	// guessing a destination could change its machine or execution authority.
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE a.workspace_id=$1 AND t.status IN ('queued','deferred') AND t.runtime_id IS DISTINCT FROM a.runtime_id)`, workspace).Scan(&stale); err != nil {
		return errors.New("cannot validate queue bindings")
	}
	if stale {
		return errors.New("queued runtime bindings disagree with agents; reconcile the queue before migration")
	}
	// Sorted runtime projection makes the receipt deterministic.
	for _, rt := range runtimes {
		target, ok := bindings[rt.ID]
		if !ok {
			continue
		}
		receipt.Bindings = append(receipt.Bindings, Binding{Source: rt.ID, Target: target})
		rows, err := tx.Query(ctx, `UPDATE agent SET runtime_id=$3::uuid,runtime_config='{}',custom_args='[]',model=NULL,thinking_level=NULL,service_tier=NULL,updated_at=now() WHERE workspace_id=$1 AND runtime_id=$2::uuid RETURNING id::text`, workspace, rt.ID, target)
		if err != nil {
			return errors.New("cannot migrate agent bindings")
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return errors.New("cannot read migrated agent identities")
		}
		receipt.Agents = append(receipt.Agents, ids...)
		// No native checkpoint exists for these agents. The first run creates
		// one; later queued turns must continue it rather than each resetting it.
		rows, err = tx.Query(ctx, `UPDATE agent_task_queue t SET runtime_id=$3::uuid,
 runtime_mcp_overlay=NULL,runtime_connected_apps=NULL,delivered_comment_ids='{}',prepare_lease_expires_at=NULL
 FROM agent a WHERE a.id=t.agent_id AND a.workspace_id=$1 AND t.runtime_id=$2::uuid AND t.status IN ('queued','deferred') RETURNING t.id::text`, workspace, rt.ID, target)
		if err != nil {
			return errors.New("cannot migrate queued executions")
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return errors.New("cannot read migrated execution identities")
		}
		receipt.Runs = append(receipt.Runs, ids...)
	}
	if clear {
		rows, err := tx.Query(ctx, `UPDATE agent SET runtime_config='{}',custom_args='[]',model=NULL,thinking_level=NULL,service_tier=NULL,updated_at=now()
WHERE workspace_id=$1 AND (runtime_config <> '{}'::jsonb OR custom_args <> '[]'::jsonb OR model IS NOT NULL OR thinking_level IS NOT NULL OR service_tier IS NOT NULL)
RETURNING id::text`, workspace)
		if err != nil {
			return errors.New("cannot clear reviewed agent configuration")
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return errors.New("cannot read cleared agent identities")
		}
		receipt.Agents = append(receipt.Agents, ids...)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM task_token WHERE task_id=ANY($1::uuid[]) AND workspace_id=$2`, receipt.Runs, workspace); err != nil {
		return errors.New("cannot revoke old queue credentials")
	}
	rows, err := tx.Query(ctx, `UPDATE runtime_profile SET enabled=false WHERE workspace_id=$1 AND enabled RETURNING id::text`, workspace)
	if err != nil {
		return errors.New("cannot retire CLI profiles")
	}
	receipt.Profiles, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return errors.New("cannot read retired profile identities")
	}
	sort.Strings(receipt.Agents)
	sort.Strings(receipt.Runs)
	sort.Strings(receipt.Profiles)
	return nil
}
