package access

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

// Member is a shared space's view of one of its members.
type Member struct {
	auth.User
	Role    Role   `json:"role"`
	AddedAt string `json:"added_at"`
}

var (
	errNotWorkspaceMember = errors.New("not a member of this workspace")
	errAlreadyMember      = errors.New("already a member of this space")
	errMemberNotFound     = errors.New("not a member of this space")
)

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
const timeFormat = "2006-01-02T15:04:05.000Z"

// write runs fn in a transaction. Transactions take the write lock when they begin (see
// package db), so checks and the writes that depend on them don't interleave with
// another writer's.
func (c *Checker) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := c.db.BeginTx(ctx, nil)
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

// createShared adds a shared space to workspaceID, with its creator as an editor, and
// returns its ID.
func (c *Checker) createShared(ctx context.Context, workspaceID, creatorID int64, name string) (int64, error) {
	var id int64
	err := c.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', ?)`,
			workspaceID, name)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, ?)`,
			id, creatorID, RoleEditor); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: creatorID, Action: audit.SpaceCreated,
			TargetType: audit.TargetSpace, Target: id, Details: audit.Details{"name": name}})
	})
	return id, err
}

// updateShared renames a shared space, archives it or brings it back, or allows or refuses
// downloading its apps for offline use; nil leaves that part as it is. Archiving an archived space
// keeps its first archiving time.
func (c *Checker) updateShared(ctx context.Context, workspaceID, spaceID int64, name *string, archived, offline *bool) error {
	var archivedAt any // nil: unchanged
	if archived != nil {
		archivedAt = ""
		if *archived {
			archivedAt = time.Now().UTC().Format(timeFormat)
		}
	}
	return c.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE spaces SET
				name = coalesce(?, name),
				archived_at = CASE WHEN ? IS NULL THEN archived_at
				                   WHEN ? = '' THEN NULL
				                   ELSE coalesce(archived_at, ?) END,
				offline_downloads = coalesce(?, offline_downloads)
			WHERE id = ? AND kind = 'shared'`, name, archivedAt, archivedAt, archivedAt, offline, spaceID)
		if err != nil {
			return err
		}
		if err := mustAffect(res, ErrNotFound); err != nil {
			return err
		}
		// What changed, and to what: a name, and two switches, none of them secret.
		details := audit.Details{}
		var fields []string
		if name != nil {
			fields, details["name"] = append(fields, "name"), *name
		}
		if archived != nil {
			fields, details["archived"] = append(fields, "archived"), *archived
		}
		if offline != nil {
			fields, details["offline_downloads"] = append(fields, "offline_downloads"), *offline
		}
		details["fields"] = fields
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.SpaceUpdated,
			TargetType: audit.TargetSpace, Target: spaceID, Details: details})
	})
}

// members lists a shared space's members, by name.
func (c *Checker) members(ctx context.Context, spaceID int64) ([]Member, error) {
	return spaceMembers(ctx, c.db, spaceID, auth.RosterPage{})
}

// spaceMembers lists a shared space's members, as auth.Roster does a workspace's.
func spaceMembers(ctx context.Context, q auth.Queryer, spaceID int64, page auth.RosterPage) ([]Member, error) {
	order := "u.name, u.id"
	if page.ByID || page.AfterID > 0 {
		order = "u.id"
	}
	query := `
		SELECT u.id, u.email, u.name, sm.role, sm.created_at
		FROM space_members sm JOIN users u ON u.id = sm.user_id
		WHERE sm.space_id = ? AND u.id > ?
		ORDER BY ` + order
	args := []any{spaceID, max(page.AfterID, 0)}
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
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.AddedAt); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// addMember adds userID to a shared space of workspaceID. The user must be a member of
// the workspace; the schema refuses anyone else too.
func (c *Checker) addMember(ctx context.Context, workspaceID, spaceID, userID int64, role Role) error {
	return c.write(ctx, func(tx *sql.Tx) error {
		var member bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ?)`,
			workspaceID, userID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return errNotWorkspaceMember
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, spaceID, userID, role)
		if err != nil {
			return err
		}
		if err := mustAffect(res, errAlreadyMember); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.SpaceMemberAdded,
			TargetType: audit.TargetSpace, Target: spaceID, Details: audit.Details{"user_id": userID, "role": string(role)}})
	})
}

func (c *Checker) setMemberRole(ctx context.Context, workspaceID, spaceID, userID int64, role Role) error {
	return c.write(ctx, func(tx *sql.Tx) error {
		var from Role
		err := tx.QueryRowContext(ctx, `SELECT role FROM space_members WHERE space_id = ? AND user_id = ?`, spaceID, userID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return errMemberNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ?`,
			role, spaceID, userID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.SpaceMemberRoleChanged,
			TargetType: audit.TargetSpace, Target: spaceID,
			Details: audit.Details{"user_id": userID, "from": string(from), "to": string(role)}})
	})
}

func (c *Checker) removeMember(ctx context.Context, workspaceID, spaceID, userID int64) error {
	return c.write(ctx, func(tx *sql.Tx) error {
		var role Role
		err := tx.QueryRowContext(ctx, `DELETE FROM space_members WHERE space_id = ? AND user_id = ? RETURNING role`,
			spaceID, userID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errMemberNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Action: audit.SpaceMemberRemoved,
			TargetType: audit.TargetSpace, Target: spaceID, Details: audit.Details{"user_id": userID, "role": string(role)}})
	})
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
