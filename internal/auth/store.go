package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Role is what a member may do in a workspace.
type Role string

// The roles. An admin manages the workspace's members and invites; a member uses it.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

func (r Role) valid() bool { return r == RoleAdmin || r == RoleMember }

// User is a person with a login.
type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Membership is a user's place in one workspace.
type Membership struct {
	WorkspaceID   int64  `json:"id"`
	WorkspaceName string `json:"name"`
	Role          Role   `json:"role"`
	// Locale is the workspace's default language, a BCP 47 tag, or "" for English.
	Locale string `json:"locale"`
}

// Member is a workspace's view of one of its users.
type Member struct {
	User
	Role     Role   `json:"role"`
	JoinedAt string `json:"joined_at"`
	// TwoFactor says whether they have two-factor login on. Only an admin's view of the
	// list has it.
	TwoFactor *bool `json:"two_factor,omitempty"`
}

// Invite is a pending invitation, without its token.
type Invite struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Role      Role   `json:"role"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

var (
	errSetupDone     = errors.New("the server is already set up")
	errNotFound      = errors.New("not found")
	errLastAdmin     = errors.New("a workspace needs at least one admin")
	errAlreadyMember = errors.New("already a member of this workspace")
	errEmailTaken    = errors.New("an account with this email exists")
	errInviteInvalid = errors.New("the invite is unknown, used or expired")
)

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the format of the schema's
// defaults, so stored times compare correctly as text.
const timeFormat = "2006-01-02T15:04:05.000Z"

func stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

// write runs fn in a transaction. Transactions take the write lock when they begin (see
// package db), so a check and the writes that depend on it can't interleave with another
// writer's.
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

// setup creates the first user, as the admin of a new workspace, and logs them in. It
// fails with errSetupDone once any user exists, also when two setups race.
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
		res, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name) VALUES (?)`, workspace)
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
		token, err = s.insertSession(ctx, tx, u.ID)
		return err
	})
	return u, token, err
}

func insertUser(ctx context.Context, tx *sql.Tx, email, name, passwordHash string) (User, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO users (email, name, password_hash) VALUES (?, ?, ?)`,
		email, name, passwordHash)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	return User{ID: id, Email: email, Name: name}, err
}

// userByEmail returns the user with email and their password hash, or errNotFound.
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

// newSession logs userID in and returns the session's token.
func (s *Service) newSession(ctx context.Context, userID int64) (string, error) {
	var token string
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		token, err = s.insertSession(ctx, tx, userID)
		return err
	})
	return token, err
}

// insertSession adds a session for userID, and drops the user's expired ones so they
// don't pile up.
func (s *Service) insertSession(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	now := s.now()
	// Sweep every expired session, not just this user's, so sessions of users who never
	// come back don't pile up.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, stamp(now)); err != nil {
		return "", err
	}
	token, hash := newToken()
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hash, userID, stamp(now.Add(s.opts.SessionTTL)))
	return token, err
}

// sessionUser returns the user a session token belongs to, or errNotFound if the token
// is unknown or expired.
func (s *Service) sessionUser(ctx context.Context, token string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashToken(token), stamp(s.now())).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errNotFound
	}
	return u, err
}

func (s *Service) deleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// workspacesOf lists the workspaces userID belongs to, oldest first.
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

// membership returns userID's membership of workspaceID, or errNotFound.
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.name, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ?
		ORDER BY u.name, u.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// roleIn returns userID's role in workspaceID within tx, or errNotFound.
func roleIn(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) (Role, error) {
	var role Role
	err := tx.QueryRowContext(ctx, `SELECT role FROM memberships WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return role, err
}

// checkNotLastAdmin fails with errLastAdmin if the workspace has only one admin. Call it
// before demoting or removing an admin.
func checkNotLastAdmin(ctx context.Context, tx *sql.Tx, workspaceID int64) error {
	var admins int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memberships WHERE workspace_id = ? AND role = ?`,
		workspaceID, RoleAdmin).Scan(&admins); err != nil {
		return err
	}
	if admins <= 1 {
		return errLastAdmin
	}
	return nil
}

// setRole changes a member's role. The last admin can't be demoted.
func (s *Service) setRole(ctx context.Context, workspaceID, userID int64, role Role) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		current, err := roleIn(ctx, tx, workspaceID, userID)
		if err != nil || current == role {
			return err
		}
		if current == RoleAdmin {
			if err := checkNotLastAdmin(ctx, tx, workspaceID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE memberships SET role = ? WHERE workspace_id = ? AND user_id = ?`,
			role, workspaceID, userID)
		return err
	})
}

// removeMember takes a user out of a workspace. The last admin can't be removed.
func (s *Service) removeMember(ctx context.Context, workspaceID, userID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		current, err := roleIn(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if current == RoleAdmin {
			if err := checkNotLastAdmin(ctx, tx, workspaceID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`,
			workspaceID, userID)
		return err
	})
}

// createInvite stores an invite to workspaceID for email and returns it with its token,
// which isn't kept. It fails with errAlreadyMember if email already belongs to a member.
func (s *Service) createInvite(ctx context.Context, workspaceID, by int64, email string, role Role) (Invite, string, error) {
	now := s.now()
	inv := Invite{Email: email, Role: role, CreatedAt: stamp(now), ExpiresAt: stamp(now.Add(s.opts.InviteTTL))}
	token, hash := newToken()
	err := s.write(ctx, func(tx *sql.Tx) error {
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
		inv.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Invite{}, "", err
	}
	return inv, token, nil
}

// pendingInvites lists a workspace's invites that can still be accepted.
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

// deleteInvite withdraws an invite of workspaceID, or fails with errNotFound.
func (s *Service) deleteInvite(ctx context.Context, workspaceID, inviteID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND workspace_id = ? AND accepted_at IS NULL`,
		inviteID, workspaceID)
	if err != nil {
		return err
	}
	return mustAffect(res, errNotFound)
}

// mustAffect returns none if the statement behind res changed no row.
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

// pendingInvite is an invite looked up by its token.
type pendingInvite struct {
	id          int64
	workspaceID int64
	email       string
	role        Role
}

// inviteByToken returns the invite a token belongs to, or errInviteInvalid if it is
// unknown, used or expired.
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

// useInvite marks an invite accepted within tx, or fails with errInviteInvalid if it
// already was or has expired: an invite works once.
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

func join(ctx context.Context, tx *sql.Tx, inv pendingInvite, userID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET accepted_by = ? WHERE id = ?`, userID, inv.id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, inv.workspaceID, userID, inv.role)
	if err != nil {
		return err
	}
	return mustAffect(res, errAlreadyMember)
}

// acceptAsUser adds an existing user to the invite's workspace.
func (s *Service) acceptAsUser(ctx context.Context, inv pendingInvite, userID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.useInvite(ctx, tx, inv); err != nil {
			return err
		}
		return join(ctx, tx, inv, userID)
	})
}

// acceptAsNew creates the invited person's account, adds it to the workspace and logs it
// in.
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
		if err := join(ctx, tx, inv, u.ID); err != nil {
			return err
		}
		token, err = s.insertSession(ctx, tx, u.ID)
		return err
	})
	return u, token, err
}
