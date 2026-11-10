package nativemigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// This opt-in drill uses PostgreSQL's real archive tools against a disposable
// schema in the managed development database. No application tables are restored.
func TestNativeMigrationBackupRestoreDrill(t *testing.T) {
	if os.Getenv("MULTICA_RUN_NATIVE_MIGRATION_DRILL") != "1" {
		t.Skip("set MULTICA_RUN_NATIVE_MIGRATION_DRILL=1 for the local PostgreSQL archive drill")
	}
	admin := integrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema := "native_drill_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
		if err != nil {
			t.Error("cannot clean drill schema")
		}
	})
	rows, err := admin.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if _, err := admin.Exec(ctx, "CREATE TABLE "+quoted+"."+pgx.Identifier{table}.Sanitize()+" (LIKE "+pgx.Identifier{"public", table}.Sanitize()+" INCLUDING ALL)"); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = quoted + ", public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Drill owner", "drill-"+uuid.NewString()+"@example.test")
	ws := fx.Workspace(t, "Restore drill", "restore-"+uuid.NewString())
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	old := fx.Runtime(t, "Historical", testutil.Cols{"provider": "claude", "daemon_id": "drill-machine"})
	native := fx.Runtime(t, "Native", testutil.Cols{"provider": "native", "daemon_id": "drill-machine"})
	agent := fx.Agent(t, "Restore", old, testutil.Cols{"model": "old-model"})
	issue := fx.Issue(t, "Retained history")
	fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": old, "status": "completed", "session_id": "external-history", "work_dir": "/drill/workdir"})
	fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": old})
	chat := fx.ChatSession(t, agent, testutil.Cols{"runtime_id": old, "work_dir": "/drill/workdir"})
	project := fx.Project(t, "Directory")
	fx.Insert(t, "project_resource", testutil.Cols{"workspace_id": ws, "project_id": project, "resource_type": "local_directory", "created_by": user, "resource_ref": []byte(`{"daemon_id":"drill-machine","local_path":"/drill/workdir"}`)})
	before := fingerprint(t, pool, ws)
	artifact := filepath.Join(t.TempDir(), "result.txt")
	content := []byte("retained artifact\n")
	if err := os.WriteFile(artifact, content, 0600); err != nil {
		t.Fatal(err)
	}
	runPG := func(tool string, input []byte, args ...string) []byte {
		t.Helper()
		cc := cfg.ConnConfig
		cmd := exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "--interactive", "--network", "host", "--env", "PGPASSWORD", "postgres:16", tool, "--host", cc.Host, "--port", strconv.Itoa(int(cc.Port)), "--username", cc.User, "--dbname", cc.Database}, args...)...)
		cmd.Env = append(os.Environ(), "PGPASSWORD="+cc.Password)
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s failed (database credentials and archive output withheld)", tool)
		}
		return out
	}
	archive := runPG("pg_dump", nil, "--format=custom", "--no-owner", "--no-privileges", "--schema", schema)
	opts := Options{WorkspaceID: ws, OperationID: uuid.NewString(), Action: "drain", ClearCLIConfig: true}
	if _, err := Migrate(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	opts.Action = "apply"
	receipt, err := Migrate(ctx, pool, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Bindings) != 1 || receipt.Bindings[0].Target != native || len(receipt.Runs) != 1 {
		t.Fatalf("unexpected migration receipt: %+v", receipt)
	}
	// A new native checkpoint demonstrates why deployment downgrade cannot replay
	// the new history into an external session.
	fx.Insert(t, "native_session", testutil.Cols{"workspace_id": ws, "agent_id": agent, "scope_type": "chat", "scope_id": chat, "daemon_id": "drill-machine", "work_dir": "/drill/workdir", "checkpoint": []byte(`{"messages":[],"tools":[]}`)})
	if fingerprint(t, pool, ws) == before {
		t.Fatal("migration did not change fixture")
	}
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	runPG("pg_restore", archive, "--exit-on-error", "--no-owner", "--no-privileges")
	pool.Reset()
	if fingerprint(t, pool, ws) != before {
		t.Fatal("restored permissions, queue, sessions or historical bindings differ from backup")
	}
	got, err := os.ReadFile(artifact)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("database restore changed the local artifact")
	}
	report, err := Inventory(ctx, pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	safe, _ := json.Marshal(map[string]any{"archive_bytes": len(archive), "archive_sha256": fmt.Sprintf("%x", sha256.Sum256(archive)), "state": "restored_pre_migration", "runtimes": len(report.Runtimes), "agents": len(report.Agents), "runs": len(report.Runs), "artifact_preserved": true})
	t.Log(string(safe))
}
