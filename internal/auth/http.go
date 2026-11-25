package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

// Routes mounts the account endpoints on mux. docs/api.md describes them.
func (s *Service) Routes(mux *http.ServeMux) {
	// Logins and accounts are the browser's: these refuse access tokens outright.
	mux.Handle("GET /api/setup", refuseTokens(s.handleSetupStatus))
	mux.Handle("POST /api/setup", refuseTokens(s.handleSetup))
	mux.Handle("POST /api/login", refuseTokens(s.handleLogin))
	mux.Handle("POST /api/logout", refuseTokens(s.handleLogout))
	mux.Handle("GET /api/me", s.RequireUser(http.HandlerFunc(s.handleMe)))
	mux.Handle("PATCH /api/me", s.RequireUser(http.HandlerFunc(s.handleUpdateMe)))
	mux.Handle("POST /api/workspaces", s.RequireUser(http.HandlerFunc(s.handleCreateWorkspace)))
	mux.Handle("PATCH /api/workspaces/{workspace}", s.RequireAdmin(http.HandlerFunc(s.handleUpdateWorkspace)))

	// An access token may read who the workspace's members are, to assign cards; the rest
	// takes a session.
	mux.Handle("GET /api/workspaces/{workspace}/members", AcceptTokens(s.RequireMember(http.HandlerFunc(s.handleMembers))))
	mux.Handle("PATCH /api/workspaces/{workspace}/members/{user}", s.RequireAdmin(http.HandlerFunc(s.handleSetRole)))
	mux.Handle("DELETE /api/workspaces/{workspace}/members/{user}", s.RequireAdmin(http.HandlerFunc(s.handleRemoveMember)))
	mux.Handle("GET /api/workspaces/{workspace}/invites", s.RequireAdmin(http.HandlerFunc(s.handleInvites)))
	mux.Handle("POST /api/workspaces/{workspace}/invites", s.RequireAdmin(http.HandlerFunc(s.handleCreateInvite)))
	mux.Handle("DELETE /api/workspaces/{workspace}/invites/{invite}", s.RequireAdmin(http.HandlerFunc(s.handleDeleteInvite)))
	mux.Handle("POST /api/invites/accept", refuseTokens(s.handleAcceptInvite))

	mux.Handle("GET /api/workspaces/{workspace}/tokens", s.RequireMember(http.HandlerFunc(s.handleTokens)))
	mux.Handle("POST /api/workspaces/{workspace}/tokens", s.RequireMember(http.HandlerFunc(s.handleCreateToken)))
	mux.Handle("DELETE /api/workspaces/{workspace}/tokens/{token}", s.RequireMember(http.HandlerFunc(s.handleDeleteToken)))

	s.passwordRoutes(mux)
	s.disableRoutes(mux)
	s.sessionRoutes(mux)
	s.twoFactorRoutes(mux)
	s.ssoRoutes(mux)
}

const (
	maxBody    = 64 << 10
	maxNameLen = 100
	maxEmail   = 254
)

// Me is the reply of GET /api/me, and of the calls that log someone in.
type Me struct {
	User       MeUser       `json:"user"`
	Workspaces []Membership `json:"workspaces" apitypes:"nonnull"`
	// CanCreateWorkspace is whether POST /api/workspaces would let this user in, under the
	// operator's setting: the web app offers "New workspace" only then.
	CanCreateWorkspace bool `json:"can_create_workspace"`
	// IsOperator is whether the user is an admin of the workspace the server's setup made: only
	// then does the web app show what belongs to the whole server (backups, email).
	IsOperator bool `json:"is_operator"`
}

func (s *Service) writeMe(w http.ResponseWriter, r *http.Request, status int, u User) {
	reply, err := s.meOf(r, u)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, status, reply)
}

func (s *Service) meOf(r *http.Request, u User) (Me, error) {
	ws, err := s.workspacesOf(r.Context(), u.ID)
	if err != nil {
		return Me{}, err
	}
	locale, err := s.UserLocale(r.Context(), u.ID)
	if err != nil {
		return Me{}, err
	}
	can, err := s.mayCreateWorkspace(r.Context(), s.db, u.ID)
	if err != nil {
		return Me{}, err
	}
	operator, err := s.IsOperator(r.Context(), u.ID)
	if err != nil {
		return Me{}, err
	}
	return Me{User: MeUser{User: u, Locale: locale}, Workspaces: ws, CanCreateWorkspace: can, IsOperator: operator}, nil
}

func (s *Service) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needed, err := s.setupNeeded(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needed": needed})
}

func (s *Service) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email         string `json:"email"`
		Name          string `json:"name"`
		Password      string `json:"password"`
		WorkspaceName string `json:"workspace_name"`
		SetupToken    string `json:"setup_token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Checked before hashing, so a set-up server doesn't spend a hash on every request.
	needed, err := s.setupNeeded(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !needed {
		s.fail(w, r, errSetupDone)
		return
	}
	if !s.validSetupToken(req.SetupToken) {
		// Another request may have finished the setup since the check above, which also
		// retires the token: then that is the answer, not a wrong token.
		if s.setupDone.Load() {
			s.fail(w, r, errSetupDone)
			return
		}
		writeError(w, http.StatusForbidden, apierr.CodeSetupToken, "the setup token is missing or wrong: the server prints it to its log at startup")
		return
	}
	email, ok := normalizeEmail(req.Email)
	name, workspace := strings.TrimSpace(req.Name), strings.TrimSpace(req.WorkspaceName)
	switch {
	case !ok:
		writeError(w, http.StatusBadRequest, apierr.CodeEmailInvalid, "a valid email address is required")
		return
	case !validName(name):
		nameError(w)
		return
	case !validName(workspace):
		writeError(w, http.StatusBadRequest, apierr.CodeWorkspaceNameLength,
			fmt.Sprintf("a workspace name of 1 to %d characters is required", maxNameLen), apierr.Params{"max": maxNameLen})
		return
	}
	if e := checkPassword(req.Password); e != nil {
		apierr.WriteError(w, e)
		return
	}
	hash, err := s.hasher.hash(r.Context(), req.Password)
	if err != nil {
		serverError(w, r, err)
		return
	}
	u, token, err := s.setup(r.Context(), email, name, hash, workspace)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setupDone.Store(true)
	slog.Info("first-run setup done", "user_id", u.ID)
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusCreated, u)
}

// handleCreateWorkspace creates a workspace with the caller as its first admin, if the
// operator's setting lets them, and answers like GET /api/me with the new one added, and
// its membership as "workspace".
func (s *Service) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		writeError(w, http.StatusBadRequest, apierr.CodeWorkspaceNameLength,
			fmt.Sprintf("a workspace name of 1 to %d characters is required", maxNameLen), apierr.Params{"max": maxNameLen})
		return
	}
	m, err := s.createWorkspace(r.Context(), u.ID, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	slog.Info("workspace created", "workspace_id", m.WorkspaceID, "user_id", u.ID)
	reply, err := s.meOf(r, u)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Workspace Membership `json:"workspace"`
		Me
	}{m, reply})
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	email, _ := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, apierr.CodeCredentialsRequired, "email and password are required")
		return
	}

	u, hash, err := s.userByEmail(r.Context(), email)
	known := err == nil
	if err != nil && !errors.Is(err, errNotFound) {
		serverError(w, r, err)
		return
	}
	// The per-address limit comes first, so one client adds at most ipAttempts keys to the
	// other two per window, whatever addresses it tries.
	ip := s.clientIP(r)
	limits := []struct {
		l   *limiter
		key string
	}{
		{s.perIP, ip},
		{s.perAccountIP, email + "|" + ip},
		{s.perAccount, email},
	}
	for i, lim := range limits {
		if ok, wait := lim.l.take(lim.key); !ok {
			for _, taken := range limits[:i] {
				taken.l.give(taken.key)
			}
			tooMany(w, wait)
			return
		}
	}

	// An account made by signing in with a provider has no password: it fails like a wrong one.
	if !known || hash == "" {
		s.hasher.burn(r.Context(), req.Password)
		// The address typed is never recorded: it may be a password typed in the wrong box.
		reason := "unknown_account"
		if known {
			reason = "no_password"
		}
		s.loginFailed(r, u.ID, reason)
		writeError(w, http.StatusUnauthorized, apierr.CodeWrongCredentials, "wrong email or password")
		return
	}
	// A server-side failure isn't a failed attempt: hand the attempts back.
	refund := func() {
		for _, lim := range limits {
			lim.l.give(lim.key)
		}
	}
	match, err := s.hasher.verify(r.Context(), req.Password, hash)
	if err != nil {
		refund()
		serverError(w, r, err)
		return
	}
	if !match {
		s.loginFailed(r, u.ID, "wrong_password")
		writeError(w, http.StatusUnauthorized, apierr.CodeWrongCredentials, "wrong email or password")
		return
	}
	refund()
	s.at("password-verified")

	// Only after the password matched, so the answer doesn't tell anyone else that an account
	// exists, or is off.
	disabled, err := s.isDisabled(r.Context(), s.db, u.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if disabled {
		s.loginFailed(r, u.ID, "account_disabled")
		accountDisabled(w)
		return
	}

	// What the password earns is decided in one transaction that checks the hash is still the
	// one that matched (a change or reset meanwhile ended the sessions and challenges the old
	// password could make) and reads the two-factor state: a user with it on gets no session
	// yet, but a challenge, which only the second step (twofactor.go) accepts, and the
	// browser's own session is left as it is.
	out, err := s.passwordSignIn(r.Context(), u.ID, hash)
	if errors.Is(err, errPasswordChanged) {
		writeError(w, http.StatusUnauthorized, apierr.CodeWrongCredentials, "wrong email or password")
		return
	}
	if errors.Is(err, errAccountDisabled) {
		// Disabled since the check above.
		s.loginFailed(r, u.ID, "account_disabled")
		accountDisabled(w)
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if out.challenge != "" {
		writeChallenge(w, out)
		return
	}

	// The new session first, then drop the one the browser had: a failure in between
	// leaves the user signed in rather than signed out.
	token := out.session
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			// The new session works; the old one just lives until it expires.
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusOK, u)
}

// loginFailed records a refused password login: of the user, or of no one when the account
// doesn't exist. Attempts the limiters turn away aren't recorded: the log would fill at the rate
// an attacker chooses.
func (s *Service) loginFailed(r *http.Request, userID int64, reason string) {
	// No actor: the person at the keyboard isn't known to be the account's owner, who is the target.
	e := audit.Event{Action: audit.LoginFailed, Details: audit.Details{"method": "password", "reason": reason}}
	if userID != 0 {
		e.TargetType, e.Target = audit.TargetUser, userID
	}
	audit.Log(r.Context(), s.db, e)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		if err := s.endSession(r.Context(), token); err != nil {
			serverError(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) handleMembers(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	members, err := s.members(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	// Who has two-factor login on is for the admins, who can clear it, to see.
	if m.Role == RoleAdmin {
		if err := s.markTwoFactor(r.Context(), m.WorkspaceID, members); err != nil {
			serverError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string][]Member{"members": members})
}

func (s *Service) handleSetRole(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
	if !ok {
		return
	}
	var req struct {
		Role Role `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Role.valid() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `role must be "admin" or "member"`)
		return
	}
	if err := s.setRole(r.Context(), m.WorkspaceID, userID, req.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
	if !ok {
		return
	}
	if err := s.removeMember(r.Context(), m.WorkspaceID, userID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleInvites(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	invites, err := s.pendingInvites(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Invite{"invites": invites})
}

func (s *Service) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	u, _ := UserFrom(r.Context())
	var req struct {
		Email string `json:"email"`
		Role  Role   `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, apierr.CodeEmailInvalid, "a valid email address is required")
		return
	}
	if req.Role == "" {
		req.Role = RoleMember
	}
	if !req.Role.valid() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `role must be "admin" or "member"`)
		return
	}
	inv, token, err := s.createInvite(r.Context(), m.WorkspaceID, u.ID, email, req.Role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The only time the token is shown: the database keeps its hash.
	writeJSON(w, http.StatusCreated, InviteCreated{Invite: inv, Token: token})
}

// InviteCreated is the reply that makes an invite: the invite and its token, shown only now.
type InviteCreated struct {
	Invite
	Token string `json:"token"`
}

func (s *Service) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	id, ok := pathID(w, r, "invite", apierr.CodeInviteNotFound, "invite not found")
	if !ok {
		return
	}
	if err := s.deleteInvite(r.Context(), m.WorkspaceID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAcceptInvite joins the invite's workspace. A logged-in user whose email the
// invite is for joins with their account; anyone else creates the account the invite
// names, with a name and a password, and is logged in.
func (s *Service) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Token == "" || len(req.Token) > 128 {
		s.fail(w, r, errInviteInvalid)
		return
	}
	inv, err := s.inviteByToken(r.Context(), req.Token)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	u, loggedIn, err := s.currentUser(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if loggedIn {
		if !strings.EqualFold(u.Email, inv.email) {
			writeError(w, http.StatusForbidden, apierr.CodeInviteOtherEmail, "this invite is for another email address")
			return
		}
		if err := s.acceptAsUser(r.Context(), inv, u.ID); err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeMe(w, r, http.StatusOK, u)
		return
	}

	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		nameError(w)
		return
	}
	if e := checkPassword(req.Password); e != nil {
		apierr.WriteError(w, e)
		return
	}
	// Checked before hashing, which is slow; acceptAsNew checks again in its transaction.
	switch _, _, err := s.userByEmail(r.Context(), inv.email); {
	case err == nil:
		s.fail(w, r, errEmailTaken)
		return
	case !errors.Is(err, errNotFound):
		serverError(w, r, err)
		return
	}
	hash, err := s.hasher.hash(r.Context(), req.Password)
	if err != nil {
		serverError(w, r, err)
		return
	}
	u, token, err := s.acceptAsNew(r.Context(), inv, name, hash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusCreated, u)
}

// fail answers with the status for one of the package's errors, or 500.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var status int
	var code apierr.Code
	var msg string
	var rate *workspaceRateError
	switch {
	case errors.Is(err, errSetupDone):
		status, code, msg = http.StatusConflict, apierr.CodeSetupDone, errSetupDone.Error()
	case errors.Is(err, errCreationRefused):
		status, code, msg = http.StatusForbidden, apierr.CodeWorkspaceCreation, errCreationRefused.Error()
	case errors.Is(err, errWorkspaceLimit):
		writeError(w, http.StatusForbidden, apierr.CodeWorkspaceLimit, err.Error(), apierr.Params{"max": maxWorkspacesPerUser})
		return
	case errors.As(err, &rate):
		limited(w, rate.wait, apierr.CodeTooManyWorkspaces, rate.Error())
		return
	case errors.Is(err, errNotFound):
		status, code, msg = http.StatusNotFound, apierr.CodeNotFound, "not found"
	case errors.Is(err, errLastAdmin):
		status, code, msg = http.StatusConflict, apierr.CodeLastAdmin, errLastAdmin.Error()
	case errors.Is(err, errAlreadyMember):
		status, code, msg = http.StatusConflict, apierr.CodeAlreadyMember, errAlreadyMember.Error()
	case errors.Is(err, errEmailTaken):
		status, code, msg = http.StatusConflict, apierr.CodeInviteAccountExists,
			"an account with this email exists: log in, then accept the invite"
	case errors.Is(err, errInviteInvalid):
		status, code, msg = http.StatusNotFound, apierr.CodeInviteInvalid, errInviteInvalid.Error()
	case errors.Is(err, errAccountDisabled):
		// The admin was disabled after the request was authenticated.
		accountDisabled(w)
		return
	default:
		serverError(w, r, err)
		return
	}
	writeError(w, status, code, msg)
}

func accountDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, apierr.CodeAccountDisabled, errAccountDisabled.Error())
}

func passwordLoginDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, apierr.CodePasswordLoginDisabled, "passwords are turned off: sign in with a provider")
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	limited(w, wait, apierr.CodeTooManyLogins, "too many login attempts: try again later")
}

func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
}

// pathID reads a numeric path value, answering 404 with code and notFound if it isn't one.
func pathID(w http.ResponseWriter, r *http.Request, name string, code apierr.Code, notFound string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, code, notFound)
		return 0, false
	}
	return id, true
}

// normalizeEmail trims and lower-cases an address, and reports whether it is a plain
// address (no display name) of sensible length.
func normalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxEmail {
		return s, false
	}
	addr, err := mail.ParseAddress(s)
	return s, err == nil && addr.Address == s
}

// validName reports whether s is 1 to maxNameLen characters with none that could hide or
// reorder what a reader sees: no control character (NUL, a newline), no line or paragraph
// separator and no bidirectional override or isolate.
func validName(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > maxNameLen {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' ||
			(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
	})
}

// readJSON decodes the request body into v, strictly: unknown fields and trailing data
// are errors. It answers 400 and returns false on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200
// with a broken body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("auth: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string, params ...apierr.Params) {
	apierr.Write(w, status, code, msg, params...)
}

func nameError(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, apierr.CodeNameLength,
		fmt.Sprintf("a name of 1 to %d characters is required", maxNameLen), apierr.Params{"max": maxNameLen})
}
