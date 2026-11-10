package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/nativemigration"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestInventoryCommandRejectsWritesAndInvalidScopeBeforeConnecting(t *testing.T) {
	t.Setenv("DATABASE_URL", "invalid-database-url-with-fixture-secret")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run=false"}, "inventory requires --dry-run=true"},
		{[]string{"--action=drain"}, "maintenance actions require --dry-run=false"},
		{[]string{"--action=apply", "--dry-run=false"}, "--operation-id must be a nonzero UUID"},
		{[]string{}, "--workspace-id must be a UUID"},
		{[]string{"--workspace-id=bad"}, "--workspace-id must be a UUID"},
		{[]string{"--workspace-id=00000000-0000-0000-0000-000000000001", "--timeout=0"}, "--timeout must be positive"},
		{[]string{"apply"}, "unexpected argument"},
		{[]string{"--workspace-id=00000000-0000-0000-0000-000000000001"}, "cannot configure database connection"},
	} {
		err := run(context.Background(), tc.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("args=%v error=%v", tc.args, err)
		}
	}
	t.Setenv("DATABASE_URL", "")
	if err := run(context.Background(), []string{"--workspace-id=00000000-0000-0000-0000-000000000001"}, io.Discard); err == nil || err.Error() != "DATABASE_URL is required" {
		t.Fatalf("missing database: %v", err)
	}
}

func TestInventoryCommandWritesScopedJSON(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL is required for inventory command integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot configure test database")
	}
	defer pool.Close()
	fx := testutil.New(pool, "", "")
	workspace := fx.Workspace(t, "CLI inventory", "cli-inventory-"+uuid.NewString())
	var out bytes.Buffer
	if err := run(ctx, []string{"--workspace-id=" + workspace, "--dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	var report nativemigration.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.ReadOnly || report.WorkspaceID != workspace || len(report.Runtimes) != 0 || len(report.Runs) != 0 {
		t.Fatal("command did not produce a scoped read-only report")
	}
}
