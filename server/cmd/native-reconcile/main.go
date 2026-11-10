// native-reconcile records a verified legacy shell outcome without executing it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("native-reconcile", flag.ContinueOnError)
	id := flags.String("session-id", "", "Native session UUID")
	operator := flags.String("operator-id", "", "Workspace owner/admin UUID recording the verification")
	call := flags.String("call-id", "", "Unknown shell call ID")
	version := flags.Int64("expected-version", -1, "Exact version inspected before reconciliation")
	outcome := flags.String("outcome", "", "Verified outcome: failed or succeeded")
	evidenceFile := flags.String("evidence-file", "", "UTF-8 file describing independently verified evidence")
	backupFile := flags.String("backup-file", "", "Exclusive 0600 checkpoint backup; required for --apply")
	apply := flags.Bool("apply", false, "Commit the verified result; omission is read-only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *version < 0 || *call == "" || *evidenceFile == "" || (*outcome != "failed" && *outcome != "succeeded") || (*apply && *backupFile == "") {
		return errors.New("specify session, operator, call, expected version, verified outcome and evidence; --apply also requires --backup-file")
	}
	sessionID, err := util.ParseUUID(*id)
	if err != nil {
		return errors.New("invalid session UUID")
	}
	operatorID, err := util.ParseUUID(*operator)
	if err != nil {
		return errors.New("invalid operator UUID")
	}
	f, err := os.Open(*evidenceFile)
	if err != nil {
		return errors.New("cannot read evidence file")
	}
	evidence, readErr := io.ReadAll(io.LimitReader(f, 8193))
	_ = f.Close()
	if readErr != nil || len(evidence) > 8192 || !utf8.Valid(evidence) {
		return errors.New("evidence must be UTF-8, at most 8192 bytes")
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("cannot configure database")
	}
	defer pool.Close()
	queries := db.New(pool)
	row, err := queries.GetNativeSessionForOperator(ctx, db.GetNativeSessionForOperatorParams{ID: sessionID, OperatorID: operatorID})
	if err != nil {
		return errors.New("session unavailable or operator is not a workspace manager")
	}
	if row.Version != *version {
		return errors.New("checkpoint version changed; inspect the current session before retrying")
	}
	if row.LeaseUntil.Valid && row.LeaseUntil.Time.After(time.Now()) {
		return errors.New("session has an active execution lease")
	}
	var checkpoint agentstate.Checkpoint
	if json.Unmarshal(row.Checkpoint, &checkpoint) != nil {
		return errors.New("invalid stored checkpoint")
	}
	next, err := checkpoint.ReconcileShell(*call, *outcome, *operator, string(evidence), time.Now().UTC())
	if err != nil {
		return err
	}
	if !*apply {
		fmt.Fprintln(out, "Verified input: would record the shell outcome; no checkpoint changed and no command executed.")
		return nil
	}
	backup, err := os.OpenFile(*backupFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create exclusive checkpoint backup")
	}
	raw, encodeErr := json.Marshal(row)
	if encodeErr != nil {
		_ = backup.Close()
		return errors.New("cannot encode backup")
	}
	_, writeErr := backup.Write(raw)
	syncErr := backup.Sync()
	closeErr := backup.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		return errors.New("checkpoint backup was not confirmed; no update attempted")
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(*backupFile))
		if err != nil {
			return errors.New("cannot confirm backup directory; no update attempted")
		}
		err = dir.Sync()
		_ = dir.Close()
		if err != nil {
			return errors.New("cannot sync backup directory; no update attempted")
		}
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return err
	}
	updated, err := queries.ReconcileInactiveNativeCheckpoint(ctx, db.ReconcileInactiveNativeCheckpointParams{ID: sessionID, OperatorID: operatorID, ExpectedVersion: *version, Checkpoint: raw})
	if err != nil {
		return errors.New("reconciliation not confirmed: version, membership or execution state changed; inspect before retrying")
	}
	fmt.Fprintf(out, "Recorded verified shell outcome at checkpoint version %d. No command executed; no task requeued.\n", updated.Version)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
