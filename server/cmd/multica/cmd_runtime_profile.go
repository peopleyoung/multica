package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// ---------------------------------------------------------------------------
// `multica runtime profile ...` — custom runtime profiles (MUL-3284)
//
// A runtime profile lets a workspace declare a custom agent runtime built on
// top of a supported protocol family (the routing backend) but launched via a
// site-specific command_name (e.g. a wrapper that injects credentials). The
// profile lives server-side and is workspace-scoped; the daemon resolves the
// command_name on each host's PATH at registration time.
//
// `set-path` / `unset-path` are the per-machine escape hatch: they record a
// profile_id -> absolute executable path mapping in this machine's local CLI
// config so the daemon can launch a profile whose command isn't on PATH (or
// pick a specific install among several). That mapping never leaves the
// machine — it is not sent to the server.
// ---------------------------------------------------------------------------

var runtimeProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Inspect or remove historical runtime profiles",
}

var runtimeProfileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List custom runtime profiles in the workspace",
	RunE:  runRuntimeProfileList,
}

var runtimeProfileCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a custom runtime profile",
	RunE:  runRuntimeProfileCreate,
}

var runtimeProfileUpdateCmd = &cobra.Command{
	Use:   "update <profile-id>",
	Short: "Update a custom runtime profile (protocol family is immutable)",
	Args:  exactArgs(1),
	RunE:  runRuntimeProfileUpdate,
}

var runtimeProfileDeleteCmd = &cobra.Command{
	Use:   "delete <profile-id>",
	Short: "Delete a custom runtime profile",
	Args:  exactArgs(1),
	RunE:  runRuntimeProfileDelete,
}

var runtimeProfileSetPathCmd = &cobra.Command{
	Use:   "set-path <profile-id>",
	Short: "Pin a per-machine executable path for a runtime profile (local only)",
	Args:  exactArgs(1),
	RunE:  runRuntimeProfileSetPath,
}

var runtimeProfileUnsetPathCmd = &cobra.Command{
	Use:   "unset-path <profile-id>",
	Short: "Remove a per-machine executable path override for a runtime profile",
	Args:  exactArgs(1),
	RunE:  runRuntimeProfileUnsetPath,
}

func init() {
	runtimeCmd.AddCommand(runtimeProfileCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileListCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileCreateCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileUpdateCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileDeleteCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileSetPathCmd)
	runtimeProfileCmd.AddCommand(runtimeProfileUnsetPathCmd)

	// list
	runtimeProfileListCmd.Flags().String("output", "table", "Output format: table or json")

	// create
	runtimeProfileCreateCmd.Flags().String("runtime-type", "", "Base runtime compatibility target (e.g. pi or omp)")
	runtimeProfileCreateCmd.Flags().String("protocol-family", "", "Legacy base protocol family (use --runtime-type for Oh-My-Pi)")
	runtimeProfileCreateCmd.Flags().String("command-name", "", "Executable the daemon resolves on PATH (required)")
	runtimeProfileCreateCmd.Flags().String("display-name", "", "Human-readable profile name (required)")
	runtimeProfileCreateCmd.Flags().String("description", "", "Optional description")
	runtimeProfileCreateCmd.Flags().String("output", "json", "Output format: table or json")

	// update
	runtimeProfileUpdateCmd.Flags().String("display-name", "", "New display name")
	runtimeProfileUpdateCmd.Flags().String("command-name", "", "New command name")
	runtimeProfileUpdateCmd.Flags().String("description", "", "New description")
	// NOTE: --fixed-arg remains out of the CLI create/update surface for now:
	// the product path parses command + args in the UI and stores them as
	// command_name + fixed_args. Keep this CLI shape narrow until we add an
	// argv-aware command-line parser here too.
	runtimeProfileUpdateCmd.Flags().Bool("enabled", true, "Enable or disable the profile")
	runtimeProfileUpdateCmd.Flags().String("output", "json", "Output format: table or json")

	// set-path
	runtimeProfileSetPathCmd.Flags().String("path", "", "Absolute path to the executable on this machine (required)")
}

// runtimeProfilesPath builds the workspace-scoped collection path.
func runtimeProfilesPath(workspaceID string) string {
	return fmt.Sprintf("/api/workspaces/%s/runtime-profiles", workspaceID)
}

// validateProtocolFamily checks a protocol family against the canonical agent
// whitelist client-side so an obvious typo fails fast with a helpful list
// instead of an opaque server 400.
func validateProtocolFamily(family string) error {
	if !agent.IsSupportedType(family) {
		return fmt.Errorf("invalid --protocol-family %q: must be one of %s",
			family, strings.Join(agent.SupportedTypes, ", "))
	}
	return nil
}

// NOTE: a --visibility flag is intentionally NOT exposed in v1. The server
// forces every profile to 'workspace' because the read paths do not yet
// enforce 'private' (exposing it would leak "private" profiles). Re-add once
// creator-visibility filtering exists. Follow-up: MUL-3308.

func runRuntimeProfileList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var resp struct {
		RuntimeProfiles []map[string]any `json:"runtime_profiles"`
	}
	if err := client.GetJSON(ctx, runtimeProfilesPath(workspaceID), &resp); err != nil {
		return fmt.Errorf("list runtime profiles: %w", err)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, resp.RuntimeProfiles)
	}
	printRuntimeProfileTable(resp.RuntimeProfiles)
	return nil
}

func runRuntimeProfileDelete(cmd *cobra.Command, args []string) error {
	profileID := args[0]

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	path := runtimeProfilesPath(workspaceID) + "/" + profileID
	if err := client.DeleteJSON(ctx, path); err != nil {
		// 409 means the server refused — usually because active agents are
		// still bound to this profile, on machines it names. Surface that
		// sentence rather than the raw response body: it already reads as
		// guidance, and it is what tells the user the blockers may be sitting
		// on a machine other than the one they were cleaning up.
		if _, msg, isConflict := serverConflictMessage(err); isConflict {
			return errors.New(msg)
		}
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusConflict {
			return fmt.Errorf("cannot delete runtime profile %s: profile still has active agents bound to it", profileID)
		}
		return fmt.Errorf("delete runtime profile: %w", err)
	}
	fmt.Printf("Deleted runtime profile %s\n", profileID)
	return nil
}

// outputRuntimeProfile renders a single profile honoring --output.
func outputRuntimeProfile(cmd *cobra.Command, profile map[string]any) error {
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, profile)
	}
	printRuntimeProfileTable([]map[string]any{profile})
	return nil
}

// printRuntimeProfileTable renders profiles as a stable, sorted table.
func printRuntimeProfileTable(profiles []map[string]any) {
	headers := []string{"ID", "DISPLAY_NAME", "PROTOCOL_FAMILY", "COMMAND_NAME", "ENABLED"}
	rows := make([][]string, 0, len(profiles))
	for _, p := range profiles {
		rows = append(rows, []string{
			strVal(p, "id"),
			strVal(p, "display_name"),
			strVal(p, "protocol_family"),
			strVal(p, "command_name"),
			strVal(p, "enabled"),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][1] < rows[j][1] })
	cli.PrintTable(os.Stdout, headers, rows)
}

func runRuntimeProfileCreate(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("external AI CLI profiles have been retired; use Multica Native")
}

func runRuntimeProfileUpdate(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("external AI CLI profiles have been retired; use Multica Native")
}

func runRuntimeProfileSetPath(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("external AI CLI profiles have been retired; use Multica Native")
}

func runRuntimeProfileUnsetPath(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("external AI CLI profiles have been retired; use Multica Native")
}
