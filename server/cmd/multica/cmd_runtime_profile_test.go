package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// addCommonProfileFlags wires the persistent-style flags the run functions
// resolve (server-url, workspace-id, profile, token) onto a detached test
// command so the helpers can be invoked directly.
func addCommonProfileFlags(cmd *cobra.Command) {
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("token", "", "")
}

func newProfileListTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "list"}
	addCommonProfileFlags(cmd)
	cmd.Flags().String("output", "json", "")
	return cmd
}

func newProfileCreateTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	addCommonProfileFlags(cmd)
	cmd.Flags().String("protocol-family", "", "")
	cmd.Flags().String("runtime-type", "", "")
	cmd.Flags().String("command-name", "", "")
	cmd.Flags().String("display-name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func newProfileUpdateTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "update"}
	addCommonProfileFlags(cmd)
	cmd.Flags().String("display-name", "", "")
	cmd.Flags().String("command-name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().Bool("enabled", true, "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func newProfileDeleteTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "delete"}
	addCommonProfileFlags(cmd)
	return cmd
}

func newProfileSetPathTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "set-path"}
	addCommonProfileFlags(cmd)
	cmd.Flags().String("path", "", "")
	return cmd
}

func newProfileUnsetPathTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "unset-path"}
	addCommonProfileFlags(cmd)
	return cmd
}

func TestRunRuntimeProfileList(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-123")

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runtime_profiles": []map[string]any{
				{"id": "prof-1", "display_name": "Company Codex", "protocol_family": "codex", "command_name": "company-codex", "visibility": "workspace", "enabled": true},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)

	cmd := newProfileListTestCmd()
	_ = cmd.Flags().Set("output", "json")
	if err := runRuntimeProfileList(cmd, nil); err != nil {
		t.Fatalf("runRuntimeProfileList: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/api/workspaces/ws-123/runtime-profiles" {
		t.Errorf("path = %q, want /api/workspaces/ws-123/runtime-profiles", gotPath)
	}
}

func TestRunRuntimeProfileDeleteSuccess(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-123")

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)

	cmd := newProfileDeleteTestCmd()
	if err := runRuntimeProfileDelete(cmd, []string{"prof-1"}); err != nil {
		t.Fatalf("runRuntimeProfileDelete: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/api/workspaces/ws-123/runtime-profiles/prof-1" {
		t.Errorf("path = %q, want .../runtime-profiles/prof-1", gotPath)
	}
}

func TestRunRuntimeProfileDeleteConflictSurfacesServerMessage(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("2 active agents are bound to this profile"))
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)

	cmd := newProfileDeleteTestCmd()
	err := runRuntimeProfileDelete(cmd, []string{"prof-1"})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if got := err.Error(); !strings.Contains(got, "2 active agents are bound to this profile") {
		t.Errorf("error %q should surface the server message", got)
	}
}

func TestRetiredRuntimeProfileWritesFailLocally(t *testing.T) {
 for _, handler := range []func(*cobra.Command, []string) error{
  runRuntimeProfileCreate, runRuntimeProfileUpdate, runRuntimeProfileSetPath, runRuntimeProfileUnsetPath,
 } {
  if err := handler(&cobra.Command{}, nil); err == nil || !strings.Contains(err.Error(), "retired") {
   t.Fatalf("retired profile write = %v", err)
  }
 }
}
