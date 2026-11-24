package access

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/auth"
)

// WorkspaceMembers lists the members of workspaceID, by name, as userID may see them: every
// member sees the others (it is who a card can be assigned to). It answers ErrNotFound unless
// userID is itself a member of the workspace. It works without the request machinery, for the
// agent's tools.
func (c *Checker) WorkspaceMembers(ctx context.Context, userID, workspaceID int64) ([]auth.Member, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.name, m.role, m.created_at
		FROM memberships me
		JOIN memberships m ON m.workspace_id = me.workspace_id
		JOIN users u ON u.id = m.user_id
		WHERE me.workspace_id = ? AND me.user_id = ?
		ORDER BY u.name, u.id`, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []auth.Member{}
	for rows.Next() {
		var m auth.Member
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, ErrNotFound
	}
	return all, nil
}

// ReadableSpaceMembers returns spaceID as userID sees it and the people who have access to
// it: a shared space's members, or a private space's owner alone. It answers ErrNotFound
// unless userID may read the space, whether it exists or not, so IDs reveal nothing.
func (c *Checker) ReadableSpaceMembers(ctx context.Context, userID, spaceID int64) (Space, []Member, error) {
	sp, err := c.Check(ctx, userID, spaceID)
	if err != nil {
		return Space{}, nil, err
	}
	if sp.Kind == Shared {
		members, err := c.members(ctx, sp.ID)
		return sp, members, err
	}
	var owner Member
	err = c.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name, s.created_at
		FROM spaces s JOIN users u ON u.id = s.owner_id
		WHERE s.id = ?`, sp.ID).Scan(&owner.ID, &owner.Email, &owner.Name, &owner.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Space{}, nil, ErrNotFound
	}
	if err != nil {
		return Space{}, nil, err
	}
	owner.Role = RoleOwner
	return sp, []Member{owner}, nil
}
