package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/audit"
)

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

func (r Role) valid() bool { return r == RoleAdmin || r == RoleMember }

type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type Membership struct {
	WorkspaceID   int64  `json:"id"`
	WorkspaceName string `json:"name"`
	Role          Role   `json:"role"`
	Locale        string `json:"locale"`
}

type Member struct {
	User
	Role       Role    `json:"role"`
	JoinedAt   string  `json:"joined_at"`
	Disabled   bool    `json:"disabled"`
	DeletionAt *string `json:"deletion_at"`
	TwoFactor  *bool   `json:"two_factor,omitempty"`
}

type Invite struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Role      Role   `json:"role"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

var (
	errSetupDone       = errors.New("the server is already set up")
	errNotFound        = errors.New("not found")
	errCreationRefused = errors.New("this server doesn't let you create a workspace")
	errWorkspaceLimit  = fmt.Errorf("an account can create at most %d workspaces", maxWorkspacesPerUser)
	errLastAdmin       = errors.New("a workspace needs at least one admin")
	errAlreadyMember   = errors.New("already a member of this workspace")
	errEmailTaken      = errors.New("an account with this email exists")
	errInviteInvalid   = errors.New("the invite is unknown, used or expired")
	errNotOperator     = errors.New("only the server's operator can do this")
	errAccountDisabled = errors.New("this account is disabled: ask a workspace admin to enable it")
)

const (
	maxWorkspacesPerUser = 10
	workspaceBurst       = 5
	workspaceWindow      = time.Hour
)

type workspaceRateError struct{ wait time.Duration }

func (e *workspaceRateError) Error() string {
	return "too many workspaces created lately: try again later"
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) setupNeeded(ctx context.Context) (bool, error) {
	return SetupNeeded(ctx, s.db)
}

func (s *Service) setup(ctx context.Context, email, name, passwordHash, workspace string) (User, string, error) {
	var u User
	var token string
	err := s.write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errSetupDone
		}
		var err error
		if u, err = insertUser(ctx, tx, email, name, passwordHash); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name, created_by, is_setup) VALUES (?, ?, 1)`, workspace, u.ID)
		if err != nil {
			return err
		}
		wsID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)`,
			wsID, u.ID, RoleAdmin); err != nil {
			return err
		}
		if token, err = s.insertSession(ctx, tx, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: wsID, Actor: u.ID, Action: audit.SetupCompleted,
			TargetType: audit.TargetWorkspace, Target: wsID})
	})
	return u, token, err
}

func (s *Service) creationMode() string {
	switch s.opts.WorkspaceCreation {
	case CreateByAnyone, CreateByNobody:
		return s.opts.WorkspaceCreation
	}
	return CreateByAdmins
}

func (s *Service) mayCreateWorkspace(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64) (bool, error) {
	switch s.creationMode() {
	case CreateByAnyone:
		return true, nil
	case CreateByNobody:
		return false, nil
	}
	var admin bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = ? AND role = ?)`,
		userID, RoleAdmin).Scan(&admin)
	return admin, err
}

func (s *Service) createWorkspace(ctx context.Context, userID int64, name string) (Membership, error) {
	m := Membership{WorkspaceName: name, Role: RoleAdmin}
	err := s.write(ctx, func(tx *sql.Tx) error {
		ok, err := s.mayCreateWorkspace(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !ok {
			return errCreationRefused
		}
		if err := s.checkWorkspaceLimits(ctx, tx, userID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name, created_by, created_at) VALUES (?, ?, ?)`,
			name, userID, stamp(s.now()))
		if err != nil {
			return err
		}
		if m.WorkspaceID, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)`,
			m.WorkspaceID, userID, RoleAdmin); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: m.WorkspaceID, Actor: userID, Action: audit.WorkspaceCreated,
			TargetType: audit.TargetWorkspace, Target: m.WorkspaceID, Details: audit.Details{"allowed_by": s.creationMode()}})
	})
	return m, err
}

func (s *Service) checkWorkspaceLimits(ctx context.Context, tx *sql.Tx, userID int64) error {
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE created_by = ?`, userID).Scan(&total); err != nil {
		return err
	}
	if total >= maxWorkspacesPerUser {
		return errWorkspaceLimit
	}
	now := s.now()
	rows, err := tx.QueryContext(ctx, `SELECT created_at FROM workspaces WHERE created_by = ? AND created_at > ?
		ORDER BY created_at DESC LIMIT ?`, userID, stamp(now.Add(-workspaceWindow)), workspaceBurst)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var recent []time.Time
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return err
		}
		t, err := time.Parse(timeFormat, at)
		if err != nil {
			return err
		}
		recent = append(recent, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(recent) >= workspaceBurst {
		wait := recent[len(recent)-1].Add(workspaceWindow).Sub(now)
		return &workspaceRateError{wait: max(wait, time.Second)}
	}
	return nil
}

func insertUser(ctx context.Context, tx *sql.Tx, email, name, passwordHash string) (User, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO users (id, email, name, password_hash)
		VALUES (max(coalesce((SELECT max(id) FROM users), 0), (SELECT high FROM user_ids WHERE id = 1)) + 1, ?, ?, ?)
		RETURNING id`, email, name, passwordHash).Scan(&id)
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Email: email, Name: name}, nil
}

func (s *Service) userByEmail(ctx context.Context, email string) (User, string, error) {
	u := User{Email: email}
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, password_hash FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Name, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", errNotFound
	}
	return u, hash, err
}

func (s *Service) insertSession(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	now := s.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, stamp(now)); err != nil {
		return "", err
	}
	var disabled bool
	if err := tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, userID).Scan(&disabled); err != nil {
		return "", err
	}
	if disabled {
		return "", errAccountDisabled
	}
	token, hash := newToken()
	browser, os := deviceOf(ctx)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (public_id, token_hash, user_id, created_at, expires_at, browser, os) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		newSessionPublicID(), hash, userID, stamp(now), stamp(now.Add(s.opts.SessionTTL)), browser, os)
	return token, err
}

const sessionTouchEvery = time.Minute

func (s *Service) sessionUser(ctx context.Context, token string) (User, error) {
	var u User
	var id int64
	var seen string
	now := s.now()
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name, s.id, COALESCE(s.last_seen_at, s.created_at)
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled_at IS NULL`, hashToken(token), stamp(now)).
		Scan(&u.ID, &u.Email, &u.Name, &id, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errNotFound
	}
	if err != nil {
		return u, err
	}
	if seen <= stamp(now.Add(-sessionTouchEvery)) {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET last_seen_at = ?
			WHERE id = ? AND COALESCE(last_seen_at, created_at) <= ?`,
			stamp(now), id, stamp(now.Add(-sessionTouchEvery))); err != nil {
			slog.WarnContext(ctx, "auth: recording a session's use", "session_id", id, "err", err)
		}
	}
	return u, nil
}

func (s *Service) deleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func (s *Service) endSession(ctx context.Context, token string) error {
	var ended int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var userID int64
		err := tx.QueryRowContext(ctx, `DELETE FROM sessions WHERE token_hash = ? AND expires_at > ? RETURNING user_id`,
			hashToken(token), stamp(s.now())).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
			return err
		}
		if err != nil {
			return err
		}
		ended = userID
		return audit.Record(ctx, tx, audit.Event{Actor: userID, Action: audit.Logout,
			TargetType: audit.TargetUser, Target: userID})
	})
	if err == nil && ended != 0 {
		s.endLive(ended, token)
	}
	return err
}

func (s *Service) workspacesOf(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id, w.name, m.role, w.locale
		FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ?
		ORDER BY w.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.WorkspaceID, &m.WorkspaceName, &m.Role, &m.Locale); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

func (s *Service) membership(ctx context.Context, workspaceID, userID int64) (Membership, error) {
	m := Membership{WorkspaceID: workspaceID}
	err := s.db.QueryRowContext(ctx, `
		SELECT w.name, m.role, w.locale
		FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.workspace_id = ? AND m.user_id = ?`, workspaceID, userID).
		Scan(&m.WorkspaceName, &m.Role, &m.Locale)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, errNotFound
	}
	return m, err
}

func (s *Service) members(ctx context.Context, workspaceID int64) ([]Member, error) {
	return Roster(ctx, s.db, workspaceID, RosterPage{})
}

type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type RosterPage struct {
	ByID    bool
	AfterID int64
	Limit   int
}

func Roster(ctx context.Context, q Queryer, workspaceID int64, page RosterPage) ([]Member, error) {
	order := "u.name, u.id"
	if page.ByID || page.AfterID > 0 {
		order = "u.id"
	}
	query := `
		SELECT u.id, u.email, u.name, m.role, m.created_at, u.disabled_at IS NOT NULL,
			(SELECT d.purge_at FROM account_deletions d WHERE d.user_id = u.id)
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND u.id > ?
		ORDER BY ` + order
	args := []any{workspaceID, max(page.AfterID, 0)}
	if page.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, page.Limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.JoinedAt, &m.Disabled, &m.DeletionAt); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

func IsMember(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, workspaceID, userID int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func MemberRole(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, workspaceID, userID int64) (Role, bool, error) {
	var role Role
	err := q.QueryRowContext(ctx, `SELECT m.role FROM memberships m JOIN users u ON u.id = m.user_id AND u.disabled_at IS NULL
		WHERE m.workspace_id = ? AND m.user_id = ?`, workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return role, err == nil, err
}

func roleIn(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) (Role, error) {
	var role Role
	err := tx.QueryRowContext(ctx, `SELECT role FROM memberships WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return role, err
}

func checkNotLastAdmin(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) error {
	var others int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND m.role = ? AND m.user_id <> ? AND u.disabled_at IS NULL`,
		workspaceID, RoleAdmin, userID).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return errLastAdmin
	}
	return nil
}

func (s *Service) setRole(ctx context.Context, workspaceID, userID int64, role Role) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := EnsureActorEnabled(ctx, tx); err != nil {
			return err
		}
		current, err := roleIn(ctx, tx, workspaceID, userID)
		if err != nil || current == role {
			return err
		}
		if current == RoleAdmin {
			if err := checkNotLastAdmin(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE memberships SET role = ? WHERE workspace_id = ? AND user_id = ?`,
			role, workspaceID, userID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.MemberRoleChanged,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"from": string(current), "to": string(role)}})
	})
}

func (s *Service) removeMember(ctx context.Context, workspaceID, userID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := EnsureActorEnabled(ctx, tx); err != nil {
			return err
		}
		current, err := roleIn(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if current == RoleAdmin {
			if err := checkNotLastAdmin(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
		}
		tokens, err := removedTokens(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		spaces, err := leftSpaces(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`,
			workspaceID, userID); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.MemberRemoved,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"role": string(current)}}); err != nil {
			return err
		}
		for _, t := range tokens {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.AccessTokenRevoked,
				TargetType: audit.TargetAccessToken, Target: t.id, TargetCreated: t.created, Details: audit.Details{"cascade": true}}); err != nil {
				return err
			}
		}
		for _, sp := range spaces {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.SpaceMemberRemoved,
				TargetType: audit.TargetSpace, Target: sp.id,
				Details: audit.Details{"user_id": userID, "role": sp.role, "cascade": true}}); err != nil {
				return err
			}
		}
		return nil
	})
}

type cascaded struct {
	id      int64
	created string
	role    string
}

func cascadedRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]cascaded, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []cascaded
	for rows.Next() {
		var c cascaded
		if err := rows.Scan(&c.id, &c.created, &c.role); err != nil {
			return nil, err
		}
		all = append(all, c)
	}
	return all, rows.Err()
}

func removedTokens(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) ([]cascaded, error) {
	return cascadedRows(ctx, tx, `SELECT id, created_at, '' FROM access_tokens WHERE workspace_id = ? AND user_id = ? ORDER BY id`,
		workspaceID, userID)
}

func leftSpaces(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) ([]cascaded, error) {
	return cascadedRows(ctx, tx, `
		SELECT sp.id, sp.created_at, sm.role FROM space_members sm JOIN spaces sp ON sp.id = sm.space_id
		WHERE sp.workspace_id = ? AND sm.user_id = ? ORDER BY sp.id`, workspaceID, userID)
}

func (s *Service) createInvite(ctx context.Context, workspaceID, by int64, email string, role Role) (Invite, string, error) {
	now := s.now()
	inv := Invite{Email: email, Role: role, CreatedAt: stamp(now), ExpiresAt: stamp(now.Add(s.opts.InviteTTL))}
	token, hash := newToken()
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := EnsureActorEnabled(ctx, tx); err != nil {
			return err
		}
		var member bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
			               WHERE m.workspace_id = ? AND u.email = ?)`, workspaceID, email).Scan(&member); err != nil {
			return err
		}
		if member {
			return errAlreadyMember
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO invites (workspace_id, token_hash, email, role, created_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, workspaceID, hash, email, role, by, inv.CreatedAt, inv.ExpiresAt)
		if err != nil {
			return err
		}
		if inv.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.InviteCreated,
			TargetType: audit.TargetInvite, Target: inv.ID, Details: audit.Details{"email": email, "role": string(role)}})
	})
	if err != nil {
		return Invite{}, "", err
	}
	return inv, token, nil
}

func (s *Service) pendingInvites(ctx context.Context, workspaceID int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, email, role, created_at, expires_at
		FROM invites
		WHERE workspace_id = ? AND accepted_at IS NULL AND expires_at > ?
		ORDER BY id`, workspaceID, stamp(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Invite{}
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		all = append(all, inv)
	}
	return all, rows.Err()
}

func (s *Service) deleteInvite(ctx context.Context, workspaceID, inviteID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := EnsureActorEnabled(ctx, tx); err != nil {
			return err
		}
		var email, created string
		var role Role
		err := tx.QueryRowContext(ctx, `DELETE FROM invites WHERE id = ? AND workspace_id = ? AND accepted_at IS NULL RETURNING email, role, created_at`,
			inviteID, workspaceID).Scan(&email, &role, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.InviteRevoked,
			TargetType: audit.TargetInvite, Target: inviteID, TargetCreated: created,
			Details: audit.Details{"email": email, "role": string(role)}})
	})
}

func mustAffect(res sql.Result, none error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return none
	}
	return nil
}

type pendingInvite struct {
	id          int64
	workspaceID int64
	email       string
	role        Role
}

func (s *Service) inviteByToken(ctx context.Context, token string) (pendingInvite, error) {
	var inv pendingInvite
	err := s.db.QueryRowContext(ctx, `
		SELECT id, workspace_id, email, role FROM invites
		WHERE token_hash = ? AND accepted_at IS NULL AND expires_at > ?`, hashToken(token), stamp(s.now())).
		Scan(&inv.id, &inv.workspaceID, &inv.email, &inv.role)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingInvite{}, errInviteInvalid
	}
	return inv, err
}

func (s *Service) useInvite(ctx context.Context, tx *sql.Tx, inv pendingInvite) error {
	now := stamp(s.now())
	res, err := tx.ExecContext(ctx, `
		UPDATE invites SET accepted_at = ?
		WHERE id = ? AND accepted_at IS NULL AND expires_at > ?`, now, inv.id, now)
	if err != nil {
		return err
	}
	return mustAffect(res, errInviteInvalid)
}

func (s *Service) join(ctx context.Context, tx *sql.Tx, inv pendingInvite, userID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET accepted_by = ? WHERE id = ?`, userID, inv.id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, inv.workspaceID, userID, inv.role)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Event{Workspace: inv.workspaceID, Actor: userID, Action: audit.InviteAccepted,
		TargetType: audit.TargetInvite, Target: inv.id, Details: audit.Details{"role": string(inv.role), "already_member": n == 0}}); err != nil {
		return err
	}
	if n > 0 {
		for _, h := range s.memberHooks {
			if err := h(ctx, tx, inv.workspaceID, userID); err != nil {
				return err
			}
		}
	}
	return mustAffect(res, errAlreadyMember)
}

func (s *Service) acceptAsUser(ctx context.Context, inv pendingInvite, userID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.useInvite(ctx, tx, inv); err != nil {
			return err
		}
		return s.join(ctx, tx, inv, userID)
	})
}

func (s *Service) acceptAsNew(ctx context.Context, inv pendingInvite, name, passwordHash string) (User, string, error) {
	var u User
	var token string
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.useInvite(ctx, tx, inv); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = ?)`, inv.email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errEmailTaken
		}
		var err error
		if u, err = insertUser(ctx, tx, inv.email, name, passwordHash); err != nil {
			return err
		}
		if err := s.join(ctx, tx, inv, u.ID); err != nil {
			return err
		}
		token, err = s.insertSession(ctx, tx, u.ID)
		return err
	})
	return u, token, err
}
