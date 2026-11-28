package conversations

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/notifications"
)

const maxMembers = 50

type Member struct {
	User    Person `json:"user"`
	Role    string `json:"role"`
	AddedAt string `json:"added_at"`
}

type Members struct {
	Members []Member `json:"members"`
}

var (
	errNotWorkspaceMember = errors.New("not a member of the workspace")
	errMembersLimit       = errors.New("too many members")
	errNotShared          = errors.New("not shared with that person")
)

func (s *Service) Readable(ctx context.Context, workspace, user, id int64) (title, owner string, ok bool, err error) {
	c, err := s.reach(ctx, workspace, user, id)
	if errors.Is(err, errNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return c.Title, c.Owner.Name, true, nil
}

func stillReaches(ctx context.Context, tx *sql.Tx, c Conversation, actor int64) error {
	var same bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+reachFrom+`
		WHERE c.id = ?3 AND c.owner_id = ?4 AND c.agent_id = ?5 AND `+reachWhere+`)`,
		actor, c.WorkspaceID, c.ID, c.Owner.ID, c.agentID).Scan(&same); err != nil {
		return err
	}
	if !same {
		return errNotFound
	}
	return nil
}

func (s *Service) members(ctx context.Context, c Conversation) (Members, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, m.role, m.added_at
		FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN users u ON u.id = m.user_id
		WHERE c.id = ? AND c.workspace_id = ? AND c.owner_id = ? AND c.agent_id = ?
		ORDER BY m.added_at, u.id`, c.ID, c.WorkspaceID, c.Owner.ID, c.agentID)
	if err != nil {
		return Members{}, err
	}
	defer rows.Close()
	out := Members{Members: []Member{}}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.User.ID, &m.User.Name, &m.Role, &m.AddedAt); err != nil {
			return Members{}, err
		}
		out.Members = append(out.Members, m)
	}
	return out, rows.Err()
}

func (s *Service) member(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id, user int64) (Member, error) {
	var m Member
	err := q.QueryRowContext(ctx, `SELECT u.id, u.name, m.role, m.added_at
		FROM conversation_members m JOIN users u ON u.id = m.user_id
		WHERE m.conversation_id = ? AND m.user_id = ?`, id, user).Scan(&m.User.ID, &m.User.Name, &m.Role, &m.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, errNotShared
	}
	return m, err
}

func (s *Service) share(ctx context.Context, c Conversation, actor, target int64, role string) (Member, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if actor != c.Owner.ID {
		return Member{}, errNotFound
	}
	if err := stillReaches(ctx, tx, c, actor); err != nil {
		return Member{}, err
	}
	var inWorkspace bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ?)`,
		c.WorkspaceID, target).Scan(&inWorkspace); err != nil {
		return Member{}, err
	}
	if !inWorkspace {
		return Member{}, errNotWorkspaceMember
	}
	event := audit.Event{Workspace: c.WorkspaceID, TargetType: audit.TargetConversation, Target: c.ID}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT role FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, c.ID, target).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM conversation_members WHERE conversation_id = ?`, c.ID).Scan(&count); err != nil {
			return Member{}, err
		}
		if count >= maxMembers {
			return Member{}, errMembersLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_members (conversation_id, user_id, role, added_by) VALUES (?, ?, ?, ?)`,
			c.ID, target, role, actor); err != nil {
			return Member{}, err
		}
		event.Action, event.Details = audit.ConversationShared, audit.Details{"user_id": target, "role": role}
		if err := audit.Record(ctx, tx, event); err != nil {
			return Member{}, err
		}
		if err := notifications.Add(ctx, tx, notifications.Notice{
			UserID: target, WorkspaceID: c.WorkspaceID, Kind: notifications.KindConversationShared,
			Ref: notifications.Ref{Conversation: c.ID},
		}); err != nil {
			return Member{}, err
		}
	case err != nil:
		return Member{}, err
	case current != role:
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_members SET role = ? WHERE conversation_id = ? AND user_id = ?`,
			role, c.ID, target); err != nil {
			return Member{}, err
		}
		event.Action, event.Details = audit.ConversationShareChanged, audit.Details{"user_id": target, "role": role}
		if err := audit.Record(ctx, tx, event); err != nil {
			return Member{}, err
		}
	}
	m, err := s.member(ctx, tx, c.ID, target)
	if err != nil {
		return Member{}, err
	}
	return m, tx.Commit()
}

func (s *Service) unshare(ctx context.Context, c Conversation, actor, target int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if actor != c.Owner.ID && actor != target {
		return errNotFound
	}
	if err := stillReaches(ctx, tx, c, actor); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, c.ID, target)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return errNotShared
	}
	if err := audit.Record(ctx, tx, audit.Event{Workspace: c.WorkspaceID, Action: audit.ConversationUnshared,
		TargetType: audit.TargetConversation, Target: c.ID, Details: audit.Details{"user_id": target}}); err != nil {
		return err
	}
	return tx.Commit()
}

func memberInPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
		return 0, false
	}
	return id, true
}

func (s *Service) handleMembers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	list, err := s.members(r.Context(), c)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) handleShare(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ownedInPath(w, r)
	if !ok {
		return
	}
	target, ok := memberInPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Role != RoleViewer {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `role must be "viewer"`)
		return
	}
	if target == c.Owner.ID {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "the owner already has the conversation: share it with someone else")
		return
	}
	user, _ := caller(r)
	m, err := s.share(r.Context(), c, user, target, req.Role)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeConversationNotFound, errNotFound.Error())
	case errors.Is(err, errNotWorkspaceMember):
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
	case errors.Is(err, errMembersLimit):
		writeError(w, http.StatusConflict, apierr.CodeConversationMembersLimit,
			"a conversation can be shared with at most 50 people", apierr.Params{"max": maxMembers})
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Service) handleUnshare(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	target, ok := memberInPath(w, r)
	if !ok {
		return
	}
	user, _ := caller(r)
	if c.Role != RoleOwner && target != user {
		writeError(w, http.StatusForbidden, apierr.CodeConversationReadOnly, "this conversation is shared with you to read: only its owner can remove other people")
		return
	}
	switch err := s.unshare(r.Context(), c, user, target); {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeConversationNotFound, errNotFound.Error())
	case errors.Is(err, errNotShared):
		writeError(w, http.StatusNotFound, apierr.CodeConversationMemberGone, "that person doesn't have this conversation shared")
	case err != nil:
		serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
