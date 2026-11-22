package access

import (
	"context"
	"database/sql"
	"errors"
	"time"

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
		_, err = tx.ExecContext(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, ?)`,
			id, creatorID, RoleEditor)
		return err
	})
	return id, err
}

// updateShared renames a shared space, archives it or brings it back; nil leaves that
// part as it is. Archiving an archived space keeps its first archiving time.
func (c *Checker) updateShared(ctx context.Context, spaceID int64, name *string, archived *bool) error {
	var archivedAt any // nil: unchanged
	if archived != nil {
		archivedAt = ""
		if *archived {
			archivedAt = time.Now().UTC().Format(timeFormat)
		}
	}
	res, err := c.db.ExecContext(ctx, `
		UPDATE spaces SET
			name = coalesce(?, name),
			archived_at = CASE WHEN ? IS NULL THEN archived_at
			                   WHEN ? = '' THEN NULL
			                   ELSE coalesce(archived_at, ?) END
		WHERE id = ? AND kind = 'shared'`, name, archivedAt, archivedAt, archivedAt, spaceID)
	if err != nil {
		return err
	}
	return mustAffect(res, ErrNotFound)
}

// members lists a shared space's members, by name.
func (c *Checker) members(ctx context.Context, spaceID int64) ([]Member, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.name, sm.role, sm.created_at
		FROM space_members sm JOIN users u ON u.id = sm.user_id
		WHERE sm.space_id = ?
		ORDER BY u.name, u.id`, spaceID)
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
		return mustAffect(res, errAlreadyMember)
	})
}

func (c *Checker) setMemberRole(ctx context.Context, spaceID, userID int64, role Role) error {
	res, err := c.db.ExecContext(ctx, `UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ?`,
		role, spaceID, userID)
	if err != nil {
		return err
	}
	return mustAffect(res, errMemberNotFound)
}

func (c *Checker) removeMember(ctx context.Context, spaceID, userID int64) error {
	res, err := c.db.ExecContext(ctx, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, spaceID, userID)
	if err != nil {
		return err
	}
	return mustAffect(res, errMemberNotFound)
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
