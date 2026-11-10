package nativemigration

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestMigrationDrainApplyAndRetryPreserveHistoryPermissionsAndDirectories(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Migration owner", "migration-"+uuid.NewString()+"@example.test")
	ws := fx.Workspace(t, "Migration", "migration-"+uuid.NewString())
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	old := fx.Runtime(t, "Legacy", testutil.Cols{"provider": "claude", "daemon_id": "machine"})
	native := fx.Runtime(t, "Native", testutil.Cols{"provider": "native", "daemon_id": "machine"})
	agent := fx.Agent(t, "Migration", old, testutil.Cols{"custom_args": []byte(`["secret-cli"]`), "model": "old-model", "custom_env": []byte(`{"TOKEN":"secret-env"}`), "mcp_config": []byte(`{"mcpServers":{}}`)})
	issue := fx.Issue(t, "Migration issue")
	history := fx.Task(t, agent, testutil.Cols{"runtime_id": old, "issue_id": issue, "status": "completed", "session_id": "secret-external-session", "work_dir": "/same/machine"})
	chat := fx.ChatSession(t, agent, testutil.Cols{"runtime_id": old, "session_id": "secret-chat-session", "work_dir": "/same/machine"})
	queued := fx.Task(t, agent, testutil.Cols{"runtime_id": old, "issue_id": issue, "runtime_mcp_overlay": []byte(`{"secret":"old-overlay"}`)})
	deferred := fx.Task(t, agent, testutil.Cols{"runtime_id": old, "chat_session_id": chat, "status": "deferred"})
	active := fx.Task(t, agent, testutil.Cols{"runtime_id": old, "status": "dispatched", "issue_id": fx.Issue(t, "Active")})
	project := fx.Project(t, "Migration directory")
	dir := fx.Insert(t, "project_resource", testutil.Cols{"workspace_id": ws, "project_id": project, "resource_type": "local_directory", "created_by": user, "resource_ref": []byte(`{"daemon_id":"machine","local_path":"/same/machine"}`)})
	otherWS := fx.Workspace(t, "Unrelated", "migration-other-"+uuid.NewString())
	otherBefore := fingerprint(t, pool, otherWS)
	opts := Options{WorkspaceID: ws, OperationID: uuid.NewString(), Action: "drain"}
	first, err := Migrate(ctx, pool, opts)
	if err != nil || first.State != "draining" {
		t.Fatalf("drain: %+v %v", first, err)
	}
	if _, err := Migrate(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual sqlc query used by both HTTP and WS claim paths.
	_, err = db.New(pool).ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: util.MustParseUUID(agent), RuntimeID: util.MustParseUUID(old), PrepareLeaseSecs: 60, RuntimeStaleSecs: 90})
	if !reflect.DeepEqual(err, pgx.ErrNoRows) {
		t.Fatalf("draining claim = %v", err)
	}
	// Owned dispatches may finish draining; new starts are not created.
	fx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, active)
	opts.Action = "apply"
	if _, err := Migrate(ctx, pool, opts); err == nil || !strings.Contains(err.Error(), "active executions") {
		t.Fatalf("active apply: %v", err)
	}
	fx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, active)
	if _, err := Migrate(ctx, pool, opts); err == nil || !strings.Contains(err.Error(), "CLI configuration") {
		t.Fatalf("implicit config deletion: %v", err)
	}
	opts.ClearCLIConfig = true
	receipt, err := Migrate(ctx, pool, opts)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != "native" || receipt.CompletedAt == nil || !reflect.DeepEqual(receipt.Bindings, []Binding{{Source: old, Target: native}}) || len(receipt.Runs) != 2 || len(receipt.Agents) != 1 {
		t.Fatalf("receipt: %+v", receipt)
	}
	again, err := Migrate(ctx, pool, opts)
	if err != nil || !reflect.DeepEqual(again, receipt) {
		t.Fatalf("retry: %+v %v", again, err)
	}
	var runtime, owner, env, mcp, args, model string
	fx.QueryRow(t, `SELECT runtime_id::text,owner_id::text,custom_env::text,mcp_config::text,custom_args::text,COALESCE(model,'') FROM agent WHERE id=$1`, agent).Scan(&runtime, &owner, &env, &mcp, &args, &model)
	if runtime != native || owner != user || !strings.Contains(env, "secret-env") || mcp != `{"mcpServers": {}}` || args != "[]" || model != "" {
		t.Fatalf("agent semantics changed: runtime=%s owner=%s args=%s model=%s", runtime, owner, args, model)
	}
	for _, id := range []string{queued, deferred} {
		var fresh, overlayGone bool
		fx.QueryRow(t, `SELECT runtime_id::text,force_fresh_session,runtime_mcp_overlay IS NULL FROM agent_task_queue WHERE id=$1`, id).Scan(&runtime, &fresh, &overlayGone)
		if runtime != native || fresh || !overlayGone {
			t.Fatal("queue was not rebound with fresh session and credentials")
		}
	}
	var session, path string
	fx.QueryRow(t, `SELECT runtime_id::text,session_id,work_dir FROM agent_task_queue WHERE id=$1`, history).Scan(&runtime, &session, &path)
	if runtime != old || session != "secret-external-session" || path != "/same/machine" {
		t.Fatal("history was changed")
	}
	fx.QueryRow(t, `SELECT session_id FROM chat_session WHERE id=$1`, chat).Scan(&session)
	if session != "secret-chat-session" {
		t.Fatal("historical chat pointer was deleted")
	}
	fx.QueryRow(t, `SELECT resource_ref->>'local_path' FROM project_resource WHERE id=$1`, dir).Scan(&path)
	if path != "/same/machine" || fingerprint(t, pool, otherWS) != otherBefore {
		t.Fatal("directory or another workspace changed")
	}
	encoded, _ := json.Marshal(receipt)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("receipt exposed configuration or session values")
	}
	// Even a stale API rebinding an agent cannot reopen legacy execution.
	fx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, agent, old)
	fx.Exec(t, `UPDATE agent_task_queue SET runtime_id=$2 WHERE id=$1`, queued, old)
	_, err = db.New(pool).ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: util.MustParseUUID(agent), RuntimeID: util.MustParseUUID(old), PrepareLeaseSecs: 60, RuntimeStaleSecs: 90})
	if err != pgx.ErrNoRows {
		t.Fatalf("legacy execution after apply: %v", err)
	}
	opts.Action = "resume"
	if _, err := Migrate(ctx, pool, opts); err == nil {
		t.Fatal("resume downgraded applied workspace")
	}
}

func TestMigrationGateSerializesWithInflightDispatch(t *testing.T) {
	pool := integrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fx := testutil.New(pool, "", "")
	u := fx.User(t, "Gate owner", "gate-"+uuid.NewString()+"@example.test")
	ws := fx.Workspace(t, "Gate", "gate-"+uuid.NewString())
	fx.WorkspaceID, fx.UserID = ws, u
	fx.Member(t, ws, u, "owner")
	rt := fx.Runtime(t, "Legacy", testutil.Cols{"provider": "claude", "daemon_id": "gate-machine"})
	a := fx.Agent(t, "Gate", rt)
	task := fx.Task(t, a, testutil.Cols{"runtime_id": rt})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, task); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	opts := Options{WorkspaceID: ws, OperationID: uuid.NewString(), Action: "drain"}
	go func() { _, err := Migrate(ctx, pool, opts); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("gate did not wait for dispatch commit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	opts.Action = "apply"
	if _, err := Migrate(ctx, pool, opts); err == nil || !strings.Contains(err.Error(), "active executions") {
		t.Fatalf("inflight dispatch escaped drain: %v", err)
	}
	opts.Action = "resume"
	if _, err := Migrate(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	opts.Action, opts.OperationID = "drain", uuid.NewString()
	if _, err := Migrate(ctx, pool, opts); err != nil {
		t.Fatalf("cannot start a new operation after resume: %v", err)
	}
}

func TestMigrationRejectsChangedTargetsQueuesAndLeasesAtomically(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *testutil.Fixture, string, string, string, string)
		want   string
	}{
		{"old registration", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Exec(t, `UPDATE agent_runtime SET metadata='{}' WHERE id=$1`, rt)
		}, "native-runtime-v1"},
		{"stale target", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Exec(t, `UPDATE agent_runtime SET last_seen_at=now()-interval '1 day' WHERE id=$1`, rt)
		}, "native-runtime-v1"},
		{"changed visibility", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Exec(t, `UPDATE agent_runtime SET visibility='public' WHERE id=$1`, rt)
		}, "same machine, owner and visibility"},
		{"queue drift", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Exec(t, `UPDATE agent_task_queue SET runtime_id=$2 WHERE id=$1`, task, rt)
		}, "queue"},
		{"model lease", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Insert(t, "model_call", testutil.Cols{"workspace_id": ws, "task_id": task, "purpose": "execution", "scope_key": "run:" + task, "model": "fake", "reserved_tokens": 1, "charged_tokens": 1})
		}, "active executions or leases"},
		{"session lease", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Insert(t, "native_session", testutil.Cols{"workspace_id": ws, "agent_id": a, "scope_type": "run", "scope_id": task, "daemon_id": "matrix-machine", "work_dir": "/same", "lease_until": time.Now().Add(time.Hour)})
		}, "active executions or leases"},
		{"native checkpoint", func(t *testing.T, fx *testutil.Fixture, ws, a, rt, task string) {
			fx.Insert(t, "native_session", testutil.Cols{"workspace_id": ws, "agent_id": a, "scope_type": "run", "scope_id": task, "daemon_id": "matrix-machine", "work_dir": "/same"})
		}, "existing native checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := integrationPool(t)
			fx := testutil.New(pool, "", "")
			u := fx.User(t, "Matrix owner", "matrix-"+uuid.NewString()+"@example.test")
			ws := fx.Workspace(t, "Matrix", "matrix-"+uuid.NewString())
			fx.WorkspaceID, fx.UserID = ws, u
			old := fx.Runtime(t, "Old", testutil.Cols{"provider": "claude", "daemon_id": "matrix-machine"})
			native := fx.Runtime(t, "Native", testutil.Cols{"provider": "native", "daemon_id": "matrix-machine"})
			a := fx.Agent(t, "Matrix", old)
			task := fx.Task(t, a, testutil.Cols{"runtime_id": old})
			opts := Options{WorkspaceID: ws, OperationID: uuid.NewString(), Action: "drain", ClearCLIConfig: true}
			if _, err := Migrate(context.Background(), pool, opts); err != nil {
				t.Fatal(err)
			}
			tc.change(t, fx, ws, a, native, task)
			before := fingerprint(t, pool, ws)
			opts.Action = "apply"
			if _, err := Migrate(context.Background(), pool, opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("apply: %v, want %s", err, tc.want)
			}
			if before != fingerprint(t, pool, ws) {
				t.Fatal("failed migration partially changed workspace data")
			}
		})
	}
}
