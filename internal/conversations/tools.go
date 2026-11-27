package conversations

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Service) SetCapabilities(issue func(conversation, agentConversation int64) string) {
	s.capability = issue
}

type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Principal struct {
	Conversation      int64
	AgentConversation int64
	Workspace         int64
	User              int64
}

var ErrInactive = errors.New("this conversation can't use tools: it is archived, or its owner left the workspace or was disabled")

func PrincipalOf(ctx context.Context, q Querier, id int64) (Principal, error) {
	p := Principal{Conversation: id}
	err := q.QueryRowContext(ctx, `SELECT c.agent_id, c.workspace_id, c.owner_id FROM conversations c
		WHERE c.id = ? AND c.archived_at IS NULL
		AND EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
		            WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id AND u.disabled_at IS NULL)`,
		id).Scan(&p.AgentConversation, &p.Workspace, &p.User)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrInactive
	}
	return p, err
}

func Owns(ctx context.Context, q Querier, workspace, user, id int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations c
		WHERE c.id = ? AND c.workspace_id = ? AND c.owner_id = ?
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id))`,
		id, workspace, user).Scan(&ok)
	return ok, err
}
