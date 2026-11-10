package nativemigration

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestMigrationCandidatesPreserveMachineOwnerVisibilityAndDrain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Report)
		want   string
		reason string
	}{
		{"same identity", func(*Report) {}, "can_migrate", ""},
		{"other machine", func(r *Report) { r.Runtimes[1].DaemonID = "elsewhere" }, "requires_reconfiguration", "matching_native_runtime_missing"},
		{"other owner", func(r *Report) { r.Runtimes[1].OwnerID = "other" }, "requires_reconfiguration", "matching_native_runtime_missing"},
		{"other visibility", func(r *Report) { r.Runtimes[1].Visibility = "public" }, "requires_reconfiguration", "matching_native_runtime_missing"},
		{"target offline", func(r *Report) { r.Runtimes[1].Status = "offline" }, "requires_reconfiguration", "native_target_offline"},
		{"owner missing", func(r *Report) { r.Runtimes[0].OwnerID = "" }, "requires_reconfiguration", "runtime_owner_missing"},
		{"machine missing", func(r *Report) { r.Runtimes[0].DaemonID = "" }, "requires_reconfiguration", "machine_identity_missing"},
		{"custom profile", func(r *Report) { r.Runtimes[0].ProfileID = "custom" }, "requires_reconfiguration", "custom_profile_requires_translation"},
		{"ambiguous target", func(r *Report) {
			other := r.Runtimes[1]
			other.ID = "duplicate"
			r.Runtimes = append(r.Runtimes, other)
		}, "requires_reconfiguration", "native_target_ambiguous"},
		{"active execution", func(r *Report) { r.Runs = []Run{{AgentID: "agent", EffectiveRuntimeID: "old", Status: "running"}} }, "requires_drain", "active_runs_must_drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Report{Runtimes: []Runtime{
				{ID: "old", DaemonID: "machine", OwnerID: "owner", Visibility: "private", Provider: "claude"},
				{ID: "native", DaemonID: "machine", OwnerID: "owner", Visibility: "private", Provider: "native", Status: "online"},
			}}
			tc.change(&r)
			assess(&r)
			got := r.Runtimes[0].Assessment
			if got.Disposition != tc.want || tc.reason != "" && !contains(got.Reasons, tc.reason) {
				t.Fatalf("assessment = %+v, want %s / %s", got, tc.want, tc.reason)
			}
		})
	}
}

func TestInventoryIncludesQueueHistoryIntegrationsAndNoSecrets(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Inventory owner", "inventory-"+uuid.NewString()+"@example.test")
	workspace := fx.Workspace(t, "Inventory", "inventory-"+uuid.NewString())
	fx.WorkspaceID, fx.UserID = workspace, user
	fx.Member(t, workspace, user, "owner")
	otherWS := fx.Workspace(t, "Other inventory", "inventory-other-"+uuid.NewString())
	fx.Runtime(t, "Other workspace", testutil.Cols{"workspace_id": otherWS, "provider": "native", "daemon_id": "machine"})
	old := fx.Runtime(t, "Old", testutil.Cols{"provider": "claude", "daemon_id": "machine"})
	native := fx.Runtime(t, "Native", testutil.Cols{"provider": "native", "daemon_id": "machine"})
	agent := fx.Agent(t, "Migratable", old)
	configured := fx.Agent(t, "Needs review", old, testutil.Cols{
		"custom_env":                 []byte(`{"TOKEN":"fixture-secret-env"}`),
		"custom_args":                []byte(`["fixture-secret-argument"]`),
		"mcp_config":                 []byte(`{"mcpServers":{"fixture":{"url":"https://fixture.invalid/fixture-secret-mcp"}}}`),
		"model":                      "fixture-secret-model",
		"composio_toolkit_allowlist": []string{"github"},
	})
	unbound := fx.Agent(t, "Unbound", "")
	profile := fx.Insert(t, "runtime_profile", testutil.Cols{
		"workspace_id": workspace, "display_name": "Custom", "protocol_family": "claude", "command_name": "fixture-secret-command",
		"fixed_args": []byte(`["fixture-secret-profile-arg"]`), "created_by": user,
	})
	chat := fx.ChatSession(t, agent, testutil.Cols{"session_id": "fixture-secret-external-session", "runtime_id": old, "work_dir": "/fixture/old"})
	issue := fx.Issue(t, "Inventory issue")
	history := fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": old, "status": "completed", "session_id": "fixture-secret-history-session", "work_dir": "/fixture/history"})
	for _, status := range []string{"queued", "deferred", "running", "dispatched", "waiting_local_directory"} {
		fx.Task(t, agent, testutil.Cols{"status": status, "runtime_id": old, "issue_id": fx.Issue(t, "Pending "+status)})
	}
	nativeAgent := fx.Agent(t, "Native", native)
	retired := fx.Task(t, nativeAgent, testutil.Cols{"runtime_id": native, "status": "failed", "retired_session_id": "fixture-secret-retired-session", "work_dir": "/fixture/retired"})
	nativeChat := fx.ChatSession(t, nativeAgent, testutil.Cols{"runtime_id": native})
	nativeSession := fx.Insert(t, "native_session", testutil.Cols{
		"workspace_id": workspace, "agent_id": nativeAgent, "scope_type": "chat", "scope_id": nativeChat, "daemon_id": "machine", "work_dir": "/fixture/native",
		"checkpoint":     []byte(`{"messages":[{"role":"user","content":"fixture-secret-private-body"}],"tools":[]}`),
		"write_outcomes": []byte(`{"fixture":{"body":{"token":"fixture-secret-receipt"}}}`),
	})
	fx.Exec(t, "UPDATE chat_session SET session_id=$1 WHERE id=$2", nativeSession, nativeChat)
	mcp := fx.Insert(t, "workspace_mcp_server", testutil.Cols{"workspace_id": workspace, "name": "Fixture", "created_by": user, "config": []byte(`{"headers":{"Authorization":"fixture-secret-header"}}`)})
	fx.InsertNoID(t, "agent_mcp_server", testutil.Cols{"agent_id": agent, "server_id": mcp, "enabled": true}, "agent_id=$1 AND server_id=$2", agent, mcp)
	fx.Insert(t, "user_composio_connection", testutil.Cols{"user_id": user, "toolkit_slug": "github", "auth_config_id": "fixture-secret-auth", "connected_account_id": "fixture-secret-account", "composio_user_id": user, "status": "active"})
	pkg := fx.Insert(t, "plugin_package", testutil.Cols{"workspace_id": workspace, "plugin_key": "fixture.plugin", "name": "Fixture", "created_by": user})
	pkgVersion := fx.Insert(t, "plugin_package_version", testutil.Cols{"workspace_id": workspace, "package_id": pkg, "version": "1.0.0", "manifest": []byte(`{}`), "digest": strings.Repeat("a", 64), "size_bytes": 0})
	fx.Insert(t, "plugin_installation", testutil.Cols{"workspace_id": workspace, "plugin_key": "fixture.plugin", "package_version_id": pkgVersion, "version": "1.0.0", "manifest": []byte(`{"fixture":"fixture-secret-manifest"}`), "config": []byte(`{"TOKEN":"fixture-secret-plugin"}`), "installed_by": user})
	fx.Insert(t, "channel_installation", testutil.Cols{"workspace_id": workspace, "agent_id": agent, "channel_type": "telegram", "installer_user_id": user, "config": []byte(`{"token":"fixture-secret-channel"}`)})
	project := fx.Project(t, "Inventory directory")
	fx.Insert(t, "project_resource", testutil.Cols{"workspace_id": workspace, "project_id": project, "resource_type": "local_directory", "created_by": user, "resource_ref": []byte(`{"daemon_id":"machine","local_path":"/fixture/project","extra":"fixture-secret-resource"}`)})

	before := fingerprint(t, pool, workspace)
	report, err := Inventory(ctx, pool, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if after := fingerprint(t, pool, workspace); before != after {
		t.Fatal("read-only inventory modified stored data")
	}
	if !report.ReadOnly || report.SchemaVersion != 1 || report.WorkspaceID != workspace || report.SnapshotAt.IsZero() {
		t.Fatal("inventory lacks snapshot identity")
	}
	if len(report.Runtimes) != 2 || len(report.Agents) != 4 || len(report.Profiles) != 1 || report.Profiles[0].ID != profile {
		t.Fatal("inventory missed resources or crossed workspace boundary")
	}
	if report.Pending != (Counts{Active: 3, Queued: 1, Deferred: 1}) || len(report.Runs) != 5 {
		t.Fatalf("pending runs = %+v", report.Pending)
	}
	for _, run := range report.Runs {
		if run.AgentID == unbound && run.EffectiveRuntimeID != "" {
			t.Fatal("unbound queue acquired a runtime")
		}
	}
	for _, rt := range report.Runtimes {
		if rt.ID == old && (rt.Assessment.Disposition != "requires_drain" || !reflect.DeepEqual(rt.TargetIDs, []string{native}) || rt.Runs.Active != 3) {
			t.Fatalf("legacy runtime assessment: %+v", rt)
		}
	}
	for _, a := range report.Agents {
		if a.ID == configured && (!a.HasCLIConfig || !a.HasInlineMCP || !a.HasModelOverride || a.ConnectedAppToolkits != 1 || a.Assessment.Disposition != "requires_reconfiguration") {
			t.Fatalf("provider config was not flagged: %+v", a)
		}
		if a.ID == unbound && a.Assessment.Disposition != "requires_reconfiguration" {
			t.Fatal("unbound agent is migratable")
		}
	}
	wantSessions := map[string]string{chat: "cannot_resume", history: "cannot_resume", retired: "cannot_resume", nativeSession: "verify_on_machine"}
	if len(report.Sessions) != len(wantSessions) {
		t.Fatalf("session references = %d, want %d", len(report.Sessions), len(wantSessions))
	}
	for _, s := range report.Sessions {
		if s.Assessment.Disposition != wantSessions[s.ID] {
			t.Fatalf("session assessment: %+v", s)
		}
	}
	if len(report.Integrations) != 5 || len(report.Directories) != 1 || report.Directories[0].LocalPath != "/fixture/project" {
		t.Fatal("integration or machine directory inventory incomplete")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-secret-") {
		t.Fatal("report exposed raw credentials, commands, external sessions, or model history")
	}
	second, err := Inventory(ctx, pool, workspace)
	if err != nil {
		t.Fatal(err)
	}
	second.SnapshotAt = report.SnapshotAt
	if !reflect.DeepEqual(report, second) {
		t.Fatal("unchanged snapshot produced unstable inventory")
	}
	if _, err := Inventory(ctx, pool, uuid.NewString()); err == nil {
		t.Fatal("missing workspace produced a successful report")
	}
}

// Mutate a binding after the runtime query but before the agent query. The
// report must keep the old binding from its repeatable-read snapshot.
func TestInventoryUsesOnePostgresEnforcedReadOnlySnapshot(t *testing.T) {
	pool := integrationPool(t)
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Snapshot owner", "snapshot-"+uuid.NewString()+"@example.test")
	workspace := fx.Workspace(t, "Snapshot", "snapshot-"+uuid.NewString())
	fx.UserID, fx.WorkspaceID = user, workspace
	runtime := fx.Runtime(t, "Snapshot", testutil.Cols{"provider": "native", "daemon_id": "machine"})
	agent := fx.Agent(t, "Snapshot", runtime)
	var mutationErr error
	tracer := &inventoryTracer{afterRuntime: func() {
		_, mutationErr = pool.Exec(context.Background(), "UPDATE agent SET model='changed-after-snapshot' WHERE id=$1", agent)
	}}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	r, err := Inventory(context.Background(), traced, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !tracer.readOnly || len(r.Agents) != 1 || r.Agents[0].HasModelOverride {
		t.Fatal("report lost read-only repeatable-read snapshot")
	}
	var model string
	fx.QueryRow(t, "SELECT model FROM agent WHERE id=$1", agent).Scan(&model)
	if model != "changed-after-snapshot" {
		t.Fatal("concurrent writer did not run")
	}
}

type inventorySQLKey struct{}
type inventoryTracer struct {
	once         sync.Once
	afterRuntime func()
	readOnly     bool
}

func (tr *inventoryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, inventorySQLKey{}, data.SQL)
}
func (tr *inventoryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(inventorySQLKey{}).(string)
	if sql == "begin isolation level repeatable read read only" && data.Err == nil {
		tr.readOnly = true
	}
	if sql == runtimeQuery && data.Err == nil {
		tr.once.Do(tr.afterRuntime)
	}
}

func contains(items []string, item string) bool {
	for _, value := range items {
		if value == item {
			return true
		}
	}
	return false
}

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for inventory integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("cannot configure test database")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cannot reach managed test database")
	}
	return pool
}

func fingerprint(t *testing.T, pool *pgxpool.Pool, workspace string) string {
	t.Helper()
	var result string
	err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
	 'runtimes',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM agent_runtime r WHERE workspace_id=$1),
	 'agents',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM agent a WHERE workspace_id=$1),
	 'profiles',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM runtime_profile p WHERE workspace_id=$1),
	 'chats',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chat_session c WHERE workspace_id=$1),
	 'sessions',(SELECT jsonb_agg(to_jsonb(n) ORDER BY id) FROM native_session n WHERE workspace_id=$1),
	 'runs',(SELECT jsonb_agg(to_jsonb(t) ORDER BY t.id) FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE a.workspace_id=$1)
	)::text`, workspace).Scan(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
