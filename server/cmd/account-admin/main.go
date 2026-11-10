// Account-admin creates password accounts or updates existing users by UUID.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func run(args []string, input io.Reader) error {
	flags := flag.NewFlagSet("account-admin", flag.ContinueOnError)
	id := flags.String("user-id", "", "Existing user UUID; preserves memberships and history")
	create := flags.Bool("create", false, "Create a new account instead of updating an existing user")
	email := flags.String("email", "", "Optional unverified contact email for a new account")
	username := flags.String("username", "", "Unique username to assign")
	passwordStdin := flags.Bool("password-stdin", false, "Read the new password from stdin; never pass passwords as arguments")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*passwordStdin || flags.NArg() != 0 || *create == (*id != "") || (!*create && *email != "") {
		return errors.New("specify either --create [--email] or --user-id, plus --username and --password-stdin")
	}
	var userID pgtype.UUID
	if !*create {
		var err error
		userID, err = util.ParseUUID(*id)
		if err != nil {
			return errors.New("invalid --user-id")
		}
	}
	normalized, err := auth.NormalizeUsername(*username)
	if err != nil {
		return err
	}
	contact := strings.ToLower(strings.TrimSpace(*email))
	if contact != "" {
		address, err := mail.ParseAddress(contact)
		if err != nil || address.Address != contact || len(contact) > 254 {
			return errors.New("invalid --email")
		}
	}
	raw, err := io.ReadAll(io.LimitReader(input, 75))
	if err != nil || len(raw) > 74 {
		return errors.New("cannot read password")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("cannot configure database")
	}
	defer pool.Close()
	queries := db.New(pool)
	if *create {
		_, err = queries.CreatePasswordUser(ctx, db.CreatePasswordUserParams{Username: pgtype.Text{String: normalized, Valid: true}, Name: normalized, Email: contact, PasswordHash: hash})
	} else {
		_, err = queries.SetUserPasswordAccount(ctx, db.SetUserPasswordAccountParams{ID: userID, Username: pgtype.Text{String: normalized, Valid: true}, PasswordHash: hash})
	}
	if err != nil {
		return errors.New("cannot provision account; verify user UUID and username uniqueness")
	}
	if *create {
		fmt.Fprintln(os.Stdout, "Password account created; contact email is unverified.")
	} else {
		fmt.Fprintln(os.Stdout, "Account password updated; user ID, memberships and history preserved.")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
