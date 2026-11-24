package access

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/auth"
)

// read runs fn in one read-only transaction: a deferred one, a single snapshot that takes no
// write lock (see package db), so the checks and the lists of fn can't see different states.
func (c *Checker) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // nothing was written
	return fn(tx)
}

// WorkspaceMembers lists the members of workspaceID as userID may see them: every member sees
// the others (it is who a card can be assigned to). It answers ErrNotFound unless userID is
// itself a member of the workspace. page selects the part of the roster, by user ID if it
// says so. The check and the list are of one snapshot, so a member removed meanwhile gets
// nothing. It works without the request machinery, for the agent's tools.
func (c *Checker) WorkspaceMembers(ctx context.Context, userID, workspaceID int64, page auth.RosterPage) ([]auth.Member, error) {
	var members []auth.Member
	err := c.read(ctx, func(tx *sql.Tx) error {
		ok, err := auth.IsMember(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		members, err = auth.Roster(ctx, tx, workspaceID, page)
		return err
	})
	return members, err
}

// ReadableSpaceMembers returns spaceID as userID sees it and the people who have access to
// it, the part of them page selects: a shared space's members, or a private space's owner
// alone. It answers ErrNotFound unless userID may read the space and the space is of
// workspaceID, whether it exists or not, so IDs reveal nothing. The check and the list are
// of one snapshot, so a member removed from the space meanwhile gets nothing.
func (c *Checker) ReadableSpaceMembers(ctx context.Context, userID, workspaceID, spaceID int64, page auth.RosterPage) (Space, []Member, error) {
	var sp Space
	var members []Member
	err := c.read(ctx, func(tx *sql.Tx) (err error) {
		sp, err = checkIn(ctx, tx, userID, spaceID)
		if err != nil {
			return err
		}
		if sp.WorkspaceID != workspaceID {
			return ErrNotFound
		}
		if sp.Kind == Shared {
			members, err = spaceMembers(ctx, tx, sp.ID, page)
			return err
		}
		var owner Member
		err = tx.QueryRowContext(ctx, `
			SELECT u.id, u.email, u.name, s.created_at
			FROM spaces s JOIN users u ON u.id = s.owner_id
			WHERE s.id = ?`, sp.ID).Scan(&owner.ID, &owner.Email, &owner.Name, &owner.AddedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		owner.Role = RoleOwner
		members = []Member{}
		if owner.ID > page.AfterID {
			members = append(members, owner)
		}
		return nil
	})
	if err != nil {
		return Space{}, nil, err
	}
	return sp, members, nil
}
