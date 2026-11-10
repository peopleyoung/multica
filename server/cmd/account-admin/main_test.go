package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRejectInvalidProvisioningBeforeDatabaseAccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		password string
	}{
		{"no mode", []string{"--username=alice", "--password-stdin"}, "valid password"},
		{"both modes", []string{"--create", "--user-id=bad", "--username=alice", "--password-stdin"}, "valid password"},
		{"invalid uuid", []string{"--user-id=bad", "--username=alice", "--password-stdin"}, "valid password"},
		{"invalid email", []string{"--create", "--username=alice", "--email=invalid", "--password-stdin"}, "valid password"},
		{"short password", []string{"--create", "--username=alice", "--password-stdin"}, "short"},
		{"truncated input", []string{"--create", "--username=alice", "--password-stdin"}, strings.Repeat("a", 72) + "\r\nextra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.args, strings.NewReader(tc.password)); err == nil {
				t.Fatal("accepted invalid provisioning input")
			}
		})
	}
}

func TestProvisionAndInitializeAccounts(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("requires DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fixture := testutil.New(pool, "", "")
	queries := db.New(pool)
	username := "admin_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	fixture.Cleanup(t, `DELETE FROM "user" WHERE username=$1`, username)
	if err := run([]string{"--create", "--username=" + username, "--password-stdin"}, strings.NewReader("New account password\n")); err != nil {
		t.Fatal(err)
	}
	created, err := queries.GetUserByUsername(ctx, username)
	if err != nil || created.Email != "" || created.EmailVerified || !auth.CheckPassword(created.PasswordHash, "New account password") {
		t.Fatalf("new account credentials or contact email invalid: %v", err)
	}
	legacyID := fixture.User(t, "Legacy user", username+"@example.com")
	workspaceID := fixture.Insert(t, "workspace", testutil.Cols{"name": "Account migration", "slug": username, "issue_prefix": "ADM"})
	fixture.Member(t, workspaceID, legacyID, "owner")
	legacyUsername := username + "_old"
	if err := run([]string{"--user-id=" + legacyID, "--username=" + legacyUsername, "--password-stdin"}, strings.NewReader("Updated legacy password\n")); err != nil {
		t.Fatal(err)
	}
	legacy, err := queries.GetUserByUsername(ctx, legacyUsername)
	expectedID, parseErr := util.ParseUUID(legacyID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if err != nil || legacy.ID != expectedID || legacy.Name != "Legacy user" || !legacy.EmailVerified || !auth.CheckPassword(legacy.PasswordHash, "Updated legacy password") {
		t.Fatalf("legacy account identity was not preserved: %v", err)
	}
	if fixture.Count(t, `SELECT count(*) FROM member WHERE workspace_id=$1 AND user_id=$2 AND role='owner'`, workspaceID, legacyID) != 1 {
		t.Fatal("initializing credentials changed membership")
	}
}
