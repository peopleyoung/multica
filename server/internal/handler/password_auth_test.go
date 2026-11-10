package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func passwordHandler() *Handler {
	h := *testHandler
	h.cfg.AllowSignup = true
	h.cfg.AllowedEmails = nil
	h.cfg.AllowedEmailDomains = nil
	return &h
}

func TestPasswordRegistrationAndLogin(t *testing.T) {
	h := passwordHandler()
	for _, name := range []string{"password.alice", "password.bob"} {
		dbfx.Cleanup(t, `DELETE FROM "user" WHERE username=$1`, name)
		body := map[string]string{"username": name, "password": "Strong password 2026"}
		var created LoginResponse
		response := testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(http.StatusCreated)
		response.JSON(&created)
		if created.Token == "" || created.User.Username == nil || *created.User.Username != name || created.User.Email != "" || len(response.Result().Cookies()) == 0 {
			t.Fatalf("invalid registration: %+v", created.User)
		}
		if strings.Contains(response.Body.String(), "password_hash") {
			t.Fatal("password hash exposed")
		}
		row, err := h.Queries.GetUserByUsername(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if row.EmailVerified || !auth.CheckPassword(row.PasswordHash, body["password"]) {
			t.Fatal("credentials or unverified email persisted incorrectly")
		}
		body["username"] = strings.ToUpper(name)
		var logged LoginResponse
		testutil.Call(t, h.PasswordLogin, testutil.JSONRequest("POST", "/auth/login", body)).Want(http.StatusOK).JSON(&logged)
		if logged.User.ID != created.User.ID {
			t.Fatal("login changed identity")
		}
		body["password"] = "incorrect password"
		testutil.Call(t, h.PasswordLogin, testutil.JSONRequest("POST", "/auth/login", body)).Want(http.StatusUnauthorized)
		body["username"] = "missing-user"
		testutil.Call(t, h.PasswordLogin, testutil.JSONRequest("POST", "/auth/login", body)).Want(http.StatusUnauthorized)
	}
}

func TestPasswordRegistrationBoundaries(t *testing.T) {
	h := passwordHandler()
	cases := []struct {
		name   string
		input  map[string]string
		status int
	}{
		{"username", map[string]string{"username": "xx", "password": "Strong password 2026"}, 400},
		{"short", map[string]string{"username": "password.invalid", "password": "short"}, 400},
		{"oversized", map[string]string{"username": "password.invalid", "password": strings.Repeat("a", 73)}, 400},
		{"email", map[string]string{"username": "password.invalid", "password": "Strong password 2026", "email": "invalid"}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", tc.input)).Want(tc.status)
		})
	}
	body := map[string]string{"username": "password.policy", "password": "Strong password 2026"}
	h.cfg.AllowSignup = false
	testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(403)
	h.cfg.AllowSignup = true
	h.cfg.AllowedEmails = []string{"allowed@example.com"}
	body["email"] = "allowed@example.com"
	testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(403)
}

func TestPasswordAccountsCannotClaimLegacyOrEmailInvitation(t *testing.T) {
	h := passwordHandler()
	const name = "password.contact"
	const email = "password-contact@example.com"
	dbfx.Cleanup(t, `DELETE FROM "user" WHERE username=$1`, name)
	body := map[string]string{"username": name, "password": "Strong password 2026", "email": email}
	var created LoginResponse
	testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(201).JSON(&created)
	testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(409)
	user, err := h.Queries.GetUserByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if verifiedInvitationEmail(user) != "" {
		t.Fatal("unverified email became invitation identity")
	}
	if _, err := h.Queries.GetUserByEmail(context.Background(), email); err == nil {
		t.Fatal("unverified contact email resolved as trusted identity")
	}
	// Registering with an existing legacy address cannot attach credentials to it.
	legacy := dbfx.Insert(t, "user", testutil.Cols{"name": "Legacy", "email": "password-legacy@example.com"})
	body["username"] = "password.takeover"
	body["email"] = "password-legacy@example.com"
	testutil.Call(t, h.RegisterPasswordAccount, testutil.JSONRequest("POST", "/auth/register", body)).Want(409)
	var hash string
	dbfx.QueryRow(t, `SELECT password_hash FROM "user" WHERE id=$1`, legacy).Scan(&hash)
	if hash != "" {
		t.Fatal("registration changed legacy credentials")
	}
	// A stale email code, even in development mode, cannot sign into the public endpoint.
	testutil.Call(t, h.EmailCodeRetired, testutil.JSONRequest("POST", "/auth/verify-code", map[string]string{"email": email, "code": "888888"})).Want(410)
}

func TestPasswordContactEmailCannotAccessInvitations(t *testing.T) {
	h := passwordHandler()
	const email = "unverified-invite-contact@example.com"
	userID := dbfx.Insert(t, "user", testutil.Cols{"name": "Password invitee", "email": email, "email_verified": false})
	invitationID := dbfx.Insert(t, "workspace_invitation", testutil.Cols{
		"workspace_id": testWorkspaceID, "inviter_id": testUserID,
		"invitee_email": email, "role": "member",
	})
	request := func(method, path string) *http.Request {
		req := newRequest(method, path, nil)
		req.Header.Set("X-User-ID", userID)
		return withURLParam(req, "id", invitationID)
	}
	for _, endpoint := range []struct {
		method string
		handle http.HandlerFunc
	}{
		{"GET", h.GetMyInvitation}, {"POST", h.AcceptInvitation}, {"POST", h.DeclineInvitation},
	} {
		testutil.Call(t, endpoint.handle, request(endpoint.method, "/api/invitations/"+invitationID)).Want(http.StatusForbidden)
	}
	var invitations []InvitationResponse
	testutil.Call(t, h.ListMyInvitations, request("GET", "/api/invitations")).Want(200).JSON(&invitations)
	if len(invitations) != 0 || dbfx.Count(t, `SELECT count(*) FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, userID) != 0 {
		t.Fatal("unverified email granted an invitation or membership")
	}
	// An invitation explicitly assigned to this immutable user ID still works.
	dbfx.Exec(t, `UPDATE workspace_invitation SET invitee_user_id=$1 WHERE id=$2`, userID, invitationID)
	testutil.Call(t, h.GetMyInvitation, request("GET", "/api/invitations/"+invitationID)).Want(200)
	testutil.Call(t, h.DeclineInvitation, request("POST", "/api/invitations/"+invitationID+"/decline")).Want(http.StatusNoContent)
}
