// Native-migration inventories and explicitly drains/migrates a workspace.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/nativemigration"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("native-migration", flag.ContinueOnError)
	workspace := flags.String("workspace-id", "", "Workspace UUID to inventory (required)")
	dryRun := flags.Bool("dry-run", true, "Read-only inventory (disable explicitly for maintenance actions)")
	action := flags.String("action", "inventory", "inventory, status, drain, apply or resume")
	operation := flags.String("operation-id", "", "Maintenance operation UUID, required for non-inventory actions")
	clearCLI := flags.Bool("clear-cli-config", false, "Discard reviewed CLI arguments/profile/model overrides; retain tool environment and MCP")
	timeout := flags.Duration("timeout", time.Minute, "Maximum time for database inventory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected argument; this command only inventories a workspace")
	}
	if *action == "inventory" && !*dryRun {
		return errors.New("inventory requires --dry-run=true; select an explicit maintenance action to write")
	}
	if *action != "inventory" {
		if *action != "status" && *action != "drain" && *action != "apply" && *action != "resume" {
			return errors.New("unsupported migration action")
		}
		if *dryRun && *action != "status" {
			return errors.New("maintenance actions require --dry-run=false; inventory remains the default")
		}
		id, err := uuid.Parse(*operation)
		if err != nil || id == uuid.Nil {
			return errors.New("--operation-id must be a nonzero UUID")
		}
	}
	if _, err := uuid.Parse(*workspace); err != nil {
		return errors.New("--workspace-id must be a UUID")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("cannot configure database connection")
	}
	defer pool.Close()
	var report any
	if *action == "inventory" {
		report, err = nativemigration.Inventory(ctx, pool, *workspace)
	} else {
		report, err = nativemigration.Migrate(ctx, pool, nativemigration.Options{WorkspaceID: *workspace, OperationID: *operation, Action: *action, ClearCLIConfig: *clearCLI})
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return errors.New("cannot write inventory report")
	}
	return nil
}
