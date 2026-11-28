package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/providers"
)

type Origin struct {
	Kind                      string
	Key                       string
	SpaceID, AppID, RoutineID int64
}

type Start struct {
	Workspace int64
	User      int64
	Origin    Origin
	Title     string
	Model     agentrt.ModelRef
	Prompt    string
	Tools     bool
}

type Started struct {
	Conversation Conversation
	Submission   int64
}

type Ref struct {
	Workspace    int64
	User         int64
	Conversation int64
	OriginKey    string
}

type Outcome struct {
	Settled bool
	Status  string
	Reply   string
}

var (
	ErrNotMember    = errors.New("the user is not an active member of the workspace")
	ErrModelUnknown = errors.New("the model provider isn't available to the user")
	ErrOrigin       = errors.New("the origin key is invalid or already belongs to another conversation")
	ErrTitle        = errors.New("the title must be up to 200 printable characters")
	ErrArchived     = errors.New("the conversation is archived: unarchive it first")
)

func (s *Service) Start(ctx context.Context, st Start) (Started, error) {
	if s.agent == nil {
		return Started{}, ErrRuntimeDown
	}
	if err := validOrigin(st.Origin); err != nil {
		return Started{}, err
	}
	if strings.TrimSpace(st.Prompt) == "" || !utf8.ValidString(st.Prompt) {
		return Started{}, errors.New("the prompt must not be empty")
	}
	title, titleOK := cleanTitle(st.Title)
	if !titleOK {
		return Started{}, ErrTitle
	}
	st.Title = title
	if (st.Model.Provider == "") != (st.Model.ModelID == "") {
		return Started{}, errors.New("the model must include both provider and model id")
	}
	if st.Model.Provider != "" && !providers.Usable(st.Model.Provider, st.User, st.Workspace) {
		return Started{}, ErrModelUnknown
	}
	if err := s.activeMember(ctx, st.Workspace, st.User); err != nil {
		return Started{}, err
	}
	conversation, err := s.findOrigin(ctx, st)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Started{}, err
	}
	rt, err := s.startRuntime(ctx)
	if err != nil {
		return Started{}, err
	}
	if missing {
		agentID, createErr := rt.CreateConversationOn(ctx, st.Model)
		if createErr != nil {
			return Started{}, runtimeError(ctx, createErr)
		}
		conversation, err = s.insertWithOrigin(ctx, st, agentID)
		if err != nil {
			return Started{}, err
		}
	}
	in := agentrt.Input{Content: st.Prompt, RequestID: "origin:" + st.Origin.Key}
	if s.userLanguage != nil {
		in.Language = s.userLanguage(ctx, st.User)
	}
	if st.Tools && s.capability != nil {
		in.Capability = s.capability(conversation.ID, conversation.agentID)
	}
	model, _ := rt.Model(ctx, conversation.agentID)
	if err := s.ensureUsageMark(ctx, rt, conversation.ID, conversation.agentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Started{}, errNotFound
		}
		return Started{}, runtimeError(ctx, err)
	}
	if err := s.stillEligible(ctx, st, conversation); err != nil {
		return Started{}, err
	}
	submission, err := rt.Submit(ctx, conversation.agentID, in)
	if err != nil {
		return Started{}, runtimeError(ctx, err)
	}
	if err := s.recordSubmission(context.WithoutCancel(ctx), conversation.ID, conversation.agentID, submission); err != nil {
		return Started{}, err
	}
	s.markWhenSettled(rt, Principal{Conversation: conversation.ID, AgentConversation: conversation.agentID, Workspace: conversation.WorkspaceID, User: st.User}, model.Provider, submission)
	return Started{Conversation: conversation, Submission: submission}, nil
}

func (s *Service) activeMember(ctx context.Context, workspace, user int64) error {
	var disabled bool
	err := s.db.QueryRowContext(ctx, `SELECT u.disabled_at IS NOT NULL FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND m.user_id = ?`, workspace, user).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotMember
	}
	if err != nil {
		return err
	}
	if disabled {
		return access.ErrDisabled
	}
	return nil
}

func (s *Service) stillEligible(ctx context.Context, st Start, c Conversation) error {
	var archived, member, disabled bool
	err := s.db.QueryRowContext(ctx, `SELECT c.archived_at IS NOT NULL, m.user_id IS NOT NULL, COALESCE(u.disabled_at IS NOT NULL, 0)
		FROM conversations c
		LEFT JOIN memberships m ON m.workspace_id = c.workspace_id AND m.user_id = c.owner_id
		LEFT JOIN users u ON u.id = c.owner_id
		WHERE c.id = ? AND c.workspace_id = ? AND c.owner_id = ? AND c.agent_id = ? AND c.origin = ? AND c.origin_kind = ?
		AND c.origin_space_id IS ? AND c.origin_app_id IS ? AND c.origin_routine_id IS ?`,
		c.ID, st.Workspace, st.User, c.agentID, st.Origin.Key, st.Origin.Kind,
		nullableID(st.Origin.SpaceID), nullableID(st.Origin.AppID), nullableID(st.Origin.RoutineID)).
		Scan(&archived, &member, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	switch {
	case !member:
		return ErrNotMember
	case disabled:
		return access.ErrDisabled
	case archived:
		return ErrArchived
	}
	return nil
}

func validOrigin(origin Origin) error {
	if origin.Kind != "app" && origin.Kind != "routine" || len(origin.Key) == 0 || len(origin.Key) > 100 {
		return ErrOrigin
	}
	for _, c := range []byte(origin.Key) {
		if c < 0x20 || c > 0x7e {
			return ErrOrigin
		}
	}
	return nil
}

func (s *Service) findOrigin(ctx context.Context, st Start) (Conversation, error) {
	var owner int64
	row := s.db.QueryRowContext(ctx, `SELECT owner_id, `+columns+` FROM conversations WHERE origin = ?`, st.Origin.Key)
	c, err := scan(ownerFirst{row: row, owner: &owner})
	if err != nil {
		return Conversation{}, err
	}
	if owner != st.User || c.WorkspaceID != st.Workspace || c.Origin == nil || c.Origin.Kind != st.Origin.Kind ||
		c.Origin.SpaceID != st.Origin.SpaceID || c.Origin.AppID != st.Origin.AppID || c.Origin.RoutineID != st.Origin.RoutineID {
		return Conversation{}, ErrOrigin
	}
	return c, nil
}

type ownerFirst struct {
	row   interface{ Scan(...any) error }
	owner *int64
}

func (o ownerFirst) Scan(dest ...any) error {
	return o.row.Scan(append([]any{o.owner}, dest...)...)
}

func (s *Service) insertWithOrigin(ctx context.Context, st Start, agentID int64) (Conversation, error) {
	c, err := scan(s.db.QueryRowContext(ctx, `INSERT INTO conversations
		(workspace_id, owner_id, agent_id, title, origin, origin_kind, origin_space_id, origin_app_id, origin_routine_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (origin) DO NOTHING RETURNING `+columns,
		st.Workspace, st.User, agentID, st.Title, st.Origin.Key, st.Origin.Kind, nullableID(st.Origin.SpaceID),
		nullableID(st.Origin.AppID), nullableID(st.Origin.RoutineID)))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, err
	}
	c, err = s.findOrigin(ctx, st)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Conversation{}, ErrOrigin
		}
		return Conversation{}, err
	}
	return c, nil
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Service) startRuntime(ctx context.Context) (Runtime, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.readyWait)
	defer cancel()
	rt, err := s.agent(callCtx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRuntimeDown, err)
	}
	return rt, nil
}

func runtimeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var status *agentrt.StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return ErrGone
	}
	return fmt.Errorf("%w: %w", ErrRuntimeDown, err)
}

func (s *Service) Outcome(ctx context.Context, ref Ref, submission int64, wait time.Duration) (Outcome, error) {
	if s.agent == nil {
		return Outcome{}, ErrRuntimeDown
	}
	agentConversation, err := s.agentConversation(ctx, ref, sql.NullInt64{Int64: submission, Valid: true})
	if err != nil {
		return Outcome{}, err
	}
	rt, err := s.startRuntime(ctx)
	if err != nil {
		return Outcome{}, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	settled, err := rt.Wait(waitCtx, submission)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return Outcome{Settled: false}, nil
		}
		return Outcome{}, runtimeError(ctx, err)
	}
	messagesCtx, messagesCancel := context.WithTimeout(ctx, s.exportTimeout)
	defer messagesCancel()
	msgs, err := rt.Messages(messagesCtx, agentConversation)
	if err != nil {
		return Outcome{}, runtimeError(ctx, err)
	}
	out := Outcome{Settled: true, Status: settled.Status}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			out.Reply = msgs[i].Text
			break
		}
	}
	return out, nil
}

func (s *Service) Stop(ctx context.Context, ref Ref) error {
	if s.agent == nil {
		return ErrRuntimeDown
	}
	agentConversation, err := s.agentConversation(ctx, ref, sql.NullInt64{})
	if err != nil {
		return err
	}
	rt, err := s.startRuntime(ctx)
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	if err := rt.Abort(stopCtx, agentConversation); err != nil {
		return runtimeError(ctx, err)
	}
	return nil
}

func (s *Service) recordSubmission(ctx context.Context, id, agentID, submission int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET origin_submission = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND agent_id = ?`, submission, id, agentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(err, errNotFound)
	}
	return nil
}

func (s *Service) agentConversation(ctx context.Context, ref Ref, submission sql.NullInt64) (int64, error) {
	var agentConversation int64
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM conversations
		WHERE id = ? AND workspace_id = ? AND owner_id = ? AND origin = ? AND (? IS NULL OR origin_submission = ?)`,
		ref.Conversation, ref.Workspace, ref.User, ref.OriginKey, submission, submission).Scan(&agentConversation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errNotFound
	}
	return agentConversation, err
}
