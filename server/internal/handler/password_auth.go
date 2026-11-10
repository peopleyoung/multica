package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type PasswordAccountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email,omitempty"`
}

func decodePasswordAccount(w http.ResponseWriter, r *http.Request) (PasswordAccountRequest, bool) {
	var input PasswordAccountRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid account request")
		return input, false
	}
	if len(input.Password) > 72 {
		writeError(w, http.StatusBadRequest, "password exceeds 72 bytes")
		return input, false
	}
	return input, true
}

func (h *Handler) RegisterPasswordAccount(w http.ResponseWriter, r *http.Request) {
	input, ok := decodePasswordAccount(w, r)
	if !ok {
		return
	}
	// Contact email cannot prove an allowlist entry or a pending invitation.
	if !h.cfg.AllowSignup {
		writeErrorCode(w, http.StatusForbidden, "signup_prohibited", ErrSignupProhibited.Error())
		return
	}
	username, err := auth.NormalizeUsername(input.Username)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email || len(email) > 254 {
			writeError(w, http.StatusBadRequest, "invalid contact email")
			return
		}
	}
	if len(h.cfg.AllowedEmails) > 0 || len(h.cfg.AllowedEmailDomains) > 0 {
		writeErrorCode(w, http.StatusForbidden, "signup_policy_requires_admin", "email allowlists cannot authorize unverified accounts; ask an administrator to provision your account")
		return
	}
	hash, err := auth.HashPassword(input.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := h.Queries.CreatePasswordUser(r.Context(), db.CreatePasswordUserParams{Username: ptrToText(&username), Name: username, Email: email, PasswordHash: hash})
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			writeErrorCode(w, http.StatusConflict, "account_conflict", "username or contact email is already in use")
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot create account")
		return
	}
	evt := analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r))
	evt.Properties["auth_method"] = "password"
	obsmetrics.RecordEvent(h.Analytics, h.Metrics, evt)
	h.completePasswordLogin(w, user, http.StatusCreated)
}

func (h *Handler) PasswordLogin(w http.ResponseWriter, r *http.Request) {
	input, ok := decodePasswordAccount(w, r)
	if !ok {
		return
	}
	username := strings.ToLower(strings.TrimSpace(input.Username))
	user, err := h.Queries.GetUserByUsername(r.Context(), username)
	if err != nil && !isNotFound(err) {
		writeError(w, http.StatusServiceUnavailable, "authentication unavailable")
		return
	}
	hash := user.PasswordHash
	if err != nil {
		hash = ""
	}
	if !auth.CheckPassword(hash, input.Password) || err != nil {
		writeErrorCode(w, http.StatusUnauthorized, "invalid_credentials", "incorrect username or password")
		return
	}
	h.completePasswordLogin(w, user, http.StatusOK)
}

func (h *Handler) completePasswordLogin(w http.ResponseWriter, user db.User, status int) {
	token, err := h.issueJWT(user)
	if err != nil {
		if errors.Is(err, auth.ErrTemporarilyDisabledUser) {
			writeError(w, http.StatusForbidden, auth.TemporarilyDisabledUserError)
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot create sign-in session")
		return
	}
	if err := auth.SetAuthCookies(w, token); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create sign-in session")
		return
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(auth.AuthTokenTTL())) {
			http.SetCookie(w, cookie)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, LoginResponse{Token: token, User: h.userToResponse(user)})
}

// Installed clients receive a clear error instead of silently requesting email codes.
func (*Handler) EmailCodeRetired(w http.ResponseWriter, r *http.Request) {
	writeErrorCode(w, http.StatusGone, "password_login_required", "email-code sign-in has been retired; update Multica and sign in with a username and password")
}
