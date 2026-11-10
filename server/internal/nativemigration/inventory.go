// Package nativemigration inventories a workspace without changing execution
// bindings or contacting model, daemon, or integration endpoints.
package nativemigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Assessment describes a proposal, never permission to execute a migration.
type Assessment struct {
	Disposition string   `json:"disposition"`
	Reasons     []string `json:"reasons"`
}

type Counts struct {
	Active   int `json:"active"`
	Queued   int `json:"queued"`
	Deferred int `json:"deferred"`
}

type Runtime struct {
	ID         string     `json:"id"`
	DaemonID   string     `json:"daemon_id"`
	OwnerID    string     `json:"owner_id"`
	Visibility string     `json:"visibility"`
	Provider   string     `json:"provider"`
	Status     string     `json:"status"`
	ProfileID  string     `json:"profile_id"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	TargetIDs  []string   `json:"target_runtime_ids" db:"-"`
	Runs       Counts     `json:"runs" db:"-"`
	Assessment Assessment `json:"assessment" db:"-"`
}

type Agent struct {
	ID                   string     `json:"id"`
	RuntimeID            string     `json:"runtime_id"`
	OwnerID              string     `json:"owner_id"`
	PermissionMode       string     `json:"permission_mode"`
	Kind                 string     `json:"kind"`
	Archived             bool       `json:"archived"`
	HasCLIConfig         bool       `json:"has_cli_config"`
	HasModelOverride     bool       `json:"has_model_override"`
	HasInlineMCP         bool       `json:"has_inline_mcp"`
	ConnectedAppToolkits int        `json:"connected_app_toolkits"`
	Runs                 Counts     `json:"runs" db:"-"`
	Assessment           Assessment `json:"assessment" db:"-"`
}

type Profile struct {
	ID             string     `json:"id"`
	ProtocolFamily string     `json:"protocol_family"`
	Visibility     string     `json:"visibility"`
	OwnerID        string     `json:"owner_id"`
	Enabled        bool       `json:"enabled"`
	Assessment     Assessment `json:"assessment" db:"-"`
}

// Session reports references and directory identity, not transcripts, external
// session tokens, checkpoints, or credentials embedded in execution config.
type Session struct {
	Source      string     `json:"source"`
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	ScopeType   string     `json:"scope_type"`
	ScopeID     string     `json:"scope_id"`
	RuntimeID   string     `json:"runtime_id"`
	DaemonID    string     `json:"daemon_id"`
	WorkDir     string     `json:"work_dir"`
	LeaseActive bool       `json:"lease_active"`
	Assessment  Assessment `json:"assessment" db:"-"`
}

type Run struct {
	ID                 string     `json:"id"`
	AgentID            string     `json:"agent_id"`
	RuntimeID          string     `json:"runtime_id"`
	EffectiveRuntimeID string     `json:"effective_runtime_id"`
	Status             string     `json:"status"`
	WorkDir            string     `json:"work_dir"`
	Assessment         Assessment `json:"assessment" db:"-"`
}

type Integration struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	OwnerID    string     `json:"owner_id"`
	Status     string     `json:"status"`
	Assessment Assessment `json:"assessment" db:"-"`
}

type Directory struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	DaemonID  string `json:"daemon_id"`
	LocalPath string `json:"local_path"`
}

type Report struct {
	SchemaVersion int           `json:"schema_version"`
	ReadOnly      bool          `json:"read_only"`
	WorkspaceID   string        `json:"workspace_id"`
	SnapshotAt    time.Time     `json:"snapshot_at"`
	Runtimes      []Runtime     `json:"runtimes"`
	Agents        []Agent       `json:"agents"`
	Profiles      []Profile     `json:"profiles"`
	Sessions      []Session     `json:"sessions"`
	Runs          []Run         `json:"runs"`
	Integrations  []Integration `json:"integrations"`
	Directories   []Directory   `json:"directories"`
	Pending       Counts        `json:"pending"`
	Limitations   []string      `json:"limitations"`
}

// Inventory uses a single PostgreSQL-enforced read-only snapshot. It does not
// load raw configuration JSON, so redaction is not dependent on secret names.
func Inventory(ctx context.Context, pool *pgxpool.Pool, workspaceID string) (Report, error) {
	parsed, err := uuid.Parse(workspaceID)
	if err != nil {
		return Report{}, errors.New("workspace_id must be a UUID")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, errors.New("cannot open inventory snapshot")
	}
	defer tx.Rollback(context.Background())
	r := Report{SchemaVersion: 1, ReadOnly: true, WorkspaceID: parsed.String(), Limitations: []string{
		"snapshot_only_recheck_before_migration",
		"model_configuration_and_daemon_capabilities_not_probed",
		"local_files_cli_profiles_and_directory_access_require_machine_verification",
		"integration_credentials_and_remote_services_not_probed",
		"legacy_sessions_are_history_only_start_a_new_native_session",
	}}
	if err := tx.QueryRow(ctx, "SELECT transaction_timestamp() FROM workspace WHERE id=$1", parsed).Scan(&r.SnapshotAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Report{}, errors.New("workspace not found")
		}
		return Report{}, errors.New("cannot read workspace snapshot")
	}
	r.Runtimes, err = query[Runtime](ctx, tx, runtimeQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Agents, err = query[Agent](ctx, tx, agentQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Profiles, err = query[Profile](ctx, tx, profileQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Sessions, err = query[Session](ctx, tx, sessionQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Runs, err = query[Run](ctx, tx, runQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Integrations, err = query[Integration](ctx, tx, integrationQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	r.Directories, err = query[Directory](ctx, tx, directoryQuery, parsed)
	if err != nil {
		return Report{}, err
	}
	assess(&r)
	if err := tx.Commit(ctx); err != nil {
		return Report{}, errors.New("cannot finish inventory snapshot")
	}
	return r, nil
}

func query[T any](ctx context.Context, tx pgx.Tx, sql string, workspaceID uuid.UUID) ([]T, error) {
	rows, err := tx.Query(ctx, sql, workspaceID)
	if err != nil {
		return nil, errors.New("cannot query migration inventory; verify database migrations")
	}
	values, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return nil, fmt.Errorf("cannot decode migration inventory projection: %w", err)
	}
	if values == nil {
		values = []T{}
	}
	return values, nil
}

func assessment(disposition string, reasons ...string) Assessment {
	if reasons == nil {
		reasons = []string{}
	}
	return Assessment{Disposition: disposition, Reasons: append([]string{}, reasons...)}
}

func active(status string) bool {
	return status == "running" || status == "dispatched" || status == "waiting_local_directory"
}

func count(c *Counts, status string) {
	switch {
	case active(status):
		c.Active++
	case status == "queued":
		c.Queued++
	case status == "deferred":
		c.Deferred++
	}
}

func assess(r *Report) {
	runtimes := map[string]*Runtime{}
	agents := map[string]*Agent{}
	for i := range r.Runtimes {
		runtimes[r.Runtimes[i].ID] = &r.Runtimes[i]
	}
	for i := range r.Agents {
		agents[r.Agents[i].ID] = &r.Agents[i]
	}
	for _, run := range r.Runs {
		count(&r.Pending, run.Status)
		if rt := runtimes[run.EffectiveRuntimeID]; rt != nil {
			count(&rt.Runs, run.Status)
		}
		if a := agents[run.AgentID]; a != nil {
			count(&a.Runs, run.Status)
		}
	}
	for i := range r.Runtimes {
		rt := &r.Runtimes[i]
		rt.TargetIDs = []string{}
		reasons := []string{}
		if rt.DaemonID == "" {
			reasons = append(reasons, "machine_identity_missing")
		}
		if rt.OwnerID == "" {
			reasons = append(reasons, "runtime_owner_missing")
		}
		if rt.Visibility != "public" && rt.Visibility != "private" {
			reasons = append(reasons, "unknown_runtime_visibility")
		}
		if rt.ProfileID != "" {
			reasons = append(reasons, "custom_profile_requires_translation")
		}
		if rt.Provider == "native" && rt.ProfileID == "" {
			rt.Assessment = assessment("already_native", reasons...)
			if len(reasons) > 0 {
				rt.Assessment.Disposition = "requires_reconfiguration"
			}
			continue
		}
		for _, candidate := range r.Runtimes {
			if candidate.Provider == "native" && candidate.ProfileID == "" && rt.DaemonID != "" && rt.OwnerID != "" && candidate.DaemonID == rt.DaemonID && candidate.OwnerID == rt.OwnerID && candidate.Visibility == rt.Visibility {
				rt.TargetIDs = append(rt.TargetIDs, candidate.ID)
			}
		}
		switch len(rt.TargetIDs) {
		case 0:
			reasons = append(reasons, "matching_native_runtime_missing")
		case 1:
			if runtimes[rt.TargetIDs[0]].Status != "online" {
				reasons = append(reasons, "native_target_offline")
			}
		default:
			reasons = append(reasons, "native_target_ambiguous")
		}
		rt.Assessment = assessment("can_migrate", reasons...)
		if len(reasons) > 0 {
			rt.Assessment.Disposition = "requires_reconfiguration"
		}
		if rt.Runs.Active > 0 {
			rt.Assessment.Disposition = "requires_drain"
			rt.Assessment.Reasons = append(rt.Assessment.Reasons, "active_runs_must_drain")
		}
	}
	for i := range r.Agents {
		a := &r.Agents[i]
		rt := runtimes[a.RuntimeID]
		if rt == nil {
			a.Assessment = assessment("requires_reconfiguration", "runtime_binding_missing")
			continue
		}
		a.Assessment = assessment(rt.Assessment.Disposition, rt.Assessment.Reasons...)
		if a.HasCLIConfig || a.HasModelOverride {
			a.Assessment.Disposition = "requires_reconfiguration"
			a.Assessment.Reasons = append(a.Assessment.Reasons, "provider_configuration_requires_review")
		}
		if a.OwnerID == "" || (rt.Visibility == "private" && a.OwnerID != rt.OwnerID) {
			a.Assessment.Disposition = "requires_reconfiguration"
			a.Assessment.Reasons = append(a.Assessment.Reasons, "runtime_owner_access_requires_review")
		}
		if a.Runs.Active > 0 && rt.Provider != "native" {
			a.Assessment.Disposition = "requires_drain"
		}
	}
	for i := range r.Profiles {
		r.Profiles[i].Assessment = assessment("requires_reconfiguration", "translate_cli_profile_to_native_tool_configuration")
	}
	for i := range r.Sessions {
		s := &r.Sessions[i]
		s.Assessment = assessment("cannot_resume", "legacy_session_history_only_new_native_session_required")
		if s.Source == "native" {
			s.Assessment = assessment("verify_on_machine", "verify_native_daemon_and_directory")
			if s.LeaseActive {
				s.Assessment = assessment("requires_drain", "native_session_lease_active")
			}
		}
	}
	for i := range r.Runs {
		run := &r.Runs[i]
		run.Assessment = assessment("review_queue", "revalidate_binding_authorization_and_connected_app_overlay_before_requeue")
		if active(run.Status) {
			run.Assessment = assessment("requires_drain", "do_not_rebind_an_active_execution")
		}
	}
	for i := range r.Integrations {
		r.Integrations[i].Assessment = assessment("verify_on_target", "retain_authorization_and_verify_native_tool_compatibility")
	}
}

const runtimeQuery = `SELECT id::text, COALESCE(daemon_id,'') AS daemon_id, COALESCE(owner_id::text,'') AS owner_id, visibility, provider, status,
COALESCE(profile_id::text,'') AS profile_id, last_seen_at FROM agent_runtime WHERE workspace_id=$1 ORDER BY id`

const agentQuery = `SELECT id::text, COALESCE(runtime_id::text,'') AS runtime_id, COALESCE(owner_id::text,'') AS owner_id, permission_mode, kind,
archived_at IS NOT NULL AS archived,
(COALESCE(runtime_config,'{}'::jsonb) NOT IN ('{}'::jsonb,'null'::jsonb)
 OR COALESCE(custom_env,'{}'::jsonb) NOT IN ('{}'::jsonb,'null'::jsonb)
 OR COALESCE(custom_args,'[]'::jsonb) NOT IN ('[]'::jsonb,'null'::jsonb)) AS has_cli_config,
(NULLIF(model,'') IS NOT NULL OR NULLIF(thinking_level,'') IS NOT NULL OR NULLIF(service_tier,'') IS NOT NULL) AS has_model_override,
COALESCE(mcp_config,'{}'::jsonb) NOT IN ('{}'::jsonb,'null'::jsonb) AS has_inline_mcp,
COALESCE(cardinality(composio_toolkit_allowlist),0) AS connected_app_toolkits
FROM agent WHERE workspace_id=$1 ORDER BY id`

const profileQuery = `SELECT id::text, protocol_family, visibility, COALESCE(created_by::text,'') AS owner_id, enabled
FROM runtime_profile WHERE workspace_id=$1 ORDER BY id`

const sessionQuery = `SELECT 'native' AS source, n.id::text, n.agent_id::text, n.scope_type, n.scope_id::text,
'' AS runtime_id, n.daemon_id, n.work_dir, COALESCE(n.lease_until>now(),false) AS lease_active
FROM native_session n WHERE n.workspace_id=$1
UNION ALL
SELECT 'chat', c.id::text, c.agent_id::text, 'chat', c.id::text, COALESCE(c.runtime_id::text,''),
COALESCE(rt.daemon_id,''), COALESCE(c.work_dir,''), false
FROM chat_session c LEFT JOIN agent_runtime rt ON rt.id=c.runtime_id AND rt.workspace_id=$1
WHERE c.workspace_id=$1 AND NULLIF(c.session_id,'') IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM native_session n WHERE n.workspace_id=$1 AND n.agent_id=c.agent_id
 AND n.scope_type='chat' AND n.scope_id=c.id AND n.id::text=c.session_id)
UNION ALL
SELECT 'run_history', t.id::text, t.agent_id::text,
CASE WHEN t.issue_id IS NOT NULL THEN 'issue' WHEN t.chat_session_id IS NOT NULL THEN 'chat' ELSE 'run' END,
COALESCE(t.issue_id,t.chat_session_id,t.id)::text, COALESCE(t.runtime_id::text,''), COALESCE(rt.daemon_id,''), COALESCE(t.work_dir,''), false
FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$1
LEFT JOIN agent_runtime rt ON rt.id=t.runtime_id AND rt.workspace_id=$1
WHERE NULLIF(t.session_id,'') IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM native_session n WHERE n.workspace_id=$1 AND n.agent_id=t.agent_id
 AND n.scope_type=CASE WHEN t.issue_id IS NOT NULL THEN 'issue' WHEN t.chat_session_id IS NOT NULL THEN 'chat' ELSE 'run' END
 AND n.scope_id=COALESCE(t.issue_id,t.chat_session_id,t.id) AND n.id::text=t.session_id)
UNION ALL
SELECT 'retired_run_history', t.id::text, t.agent_id::text,
CASE WHEN t.issue_id IS NOT NULL THEN 'issue' WHEN t.chat_session_id IS NOT NULL THEN 'chat' ELSE 'run' END,
COALESCE(t.issue_id,t.chat_session_id,t.id)::text, COALESCE(t.runtime_id::text,''), COALESCE(rt.daemon_id,''), COALESCE(t.work_dir,''), false
FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$1
LEFT JOIN agent_runtime rt ON rt.id=t.runtime_id AND rt.workspace_id=$1
WHERE NULLIF(t.retired_session_id,'') IS NOT NULL AND t.retired_session_id IS DISTINCT FROM t.session_id
ORDER BY source,id`

const runQuery = `SELECT t.id::text, t.agent_id::text, COALESCE(t.runtime_id::text,'') AS runtime_id,
COALESCE(t.runtime_id::text,a.runtime_id::text,'') AS effective_runtime_id, t.status, COALESCE(t.work_dir,'') AS work_dir
FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$1
WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred') ORDER BY t.id`

const integrationQuery = `SELECT 'workspace_mcp' AS kind, id::text, '' AS agent_id, COALESCE(created_by::text,'') AS owner_id, 'configured' AS status
FROM workspace_mcp_server WHERE workspace_id=$1
UNION ALL
SELECT 'agent_mcp_binding', s.server_id::text, s.agent_id::text, COALESCE(a.owner_id::text,''), CASE WHEN s.enabled THEN 'enabled' ELSE 'disabled' END
FROM agent_mcp_server s JOIN agent a ON a.id=s.agent_id AND a.workspace_id=$1
JOIN workspace_mcp_server m ON m.id=s.server_id AND m.workspace_id=$1
UNION ALL
SELECT 'plugin', id::text, '', COALESCE(installed_by::text,''), CASE WHEN enabled THEN 'enabled' ELSE 'disabled' END
FROM plugin_installation WHERE workspace_id=$1
UNION ALL
SELECT 'connected_app', c.id::text, '', c.user_id::text, c.status FROM user_composio_connection c
WHERE EXISTS (SELECT 1 FROM member m WHERE m.workspace_id=$1 AND m.user_id=c.user_id)
UNION ALL
SELECT 'channel:'||channel_type, id::text, COALESCE(agent_id::text,''), COALESCE(installer_user_id::text,''), status
FROM channel_installation WHERE workspace_id=$1
ORDER BY kind,id,agent_id`

const directoryQuery = `SELECT id::text, project_id::text, COALESCE(resource_ref->>'daemon_id','') AS daemon_id,
COALESCE(resource_ref->>'local_path','') AS local_path FROM project_resource
WHERE workspace_id=$1 AND resource_type='local_directory' ORDER BY id`
