package conversations

import (
	"context"
	"database/sql"
	"errors"
)

// The agent's tools (internal/agenttools) act for a conversation's owner, with exactly
// their access. The sidecar names the conversation with a capability the server issued
// for it, which every message sent carries; the functions below say whom that is for.

// SetCapabilities makes every message sent to a conversation carry its capability for the
// agent's tools, made by issue from the conversation's ID and the agent runtime's. The
// sidecar keeps the newest one for the conversation's tool calls, so a conversation older
// than this, or one whose capability changed (a new server key), gets it with its next
// message.
func (s *Service) SetCapabilities(issue func(conversation, agentConversation int64) string) {
	s.capability = issue
}

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Principal is whom a conversation's agent works for: its owner, in its workspace.
type Principal struct {
	Conversation int64
	// AgentConversation is the agent runtime's ID of the conversation, which a capability
	// is bound to as well, so it never outlives the agent runtime's conversation.
	AgentConversation int64
	Workspace         int64
	User              int64
}

// ErrInactive is PrincipalOf's answer for a conversation that doesn't exist, is archived, or
// whose owner isn't a member of its workspace any more.
var ErrInactive = errors.New("this conversation can't use tools: it is archived, or its owner left the workspace")

// PrincipalOf returns whom conversation id's agent works for, now: ErrInactive unless it
// exists, isn't archived, and its owner is still a member of its workspace. Ask again for
// every action, so a change takes effect at once.
func PrincipalOf(ctx context.Context, q Querier, id int64) (Principal, error) {
	p := Principal{Conversation: id}
	err := q.QueryRowContext(ctx, `SELECT c.agent_id, c.workspace_id, c.owner_id FROM conversations c
		WHERE c.id = ? AND c.archived_at IS NULL
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id)`,
		id).Scan(&p.AgentConversation, &p.Workspace, &p.User)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrInactive
	}
	return p, err
}

// Owns reports whether user owns conversation id in workspace and is a member there: the
// conversations a user may link a board card to. Archived ones count.
func Owns(ctx context.Context, q Querier, workspace, user, id int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations c
		WHERE c.id = ? AND c.workspace_id = ? AND c.owner_id = ?
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id))`,
		id, workspace, user).Scan(&ok)
	return ok, err
}
