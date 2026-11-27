package access

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/auth"
)

func (c *Checker) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

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
