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

type Outcome struct {
	Settled bool
	Status  string
	Reply   string
}

var (
	ErrNotMember    = errors.New("the user is not an active member of the workspace")
	ErrModelUnknown = errors.New("the model provider isn't available to the user")
	ErrOrigin       = errors.New("the origin key is invalid or already belongs to another conversation")
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
	if (st.Model.Provider == "") != (st.Model.ModelID == "") {
		return Started{}, errors.New("the model must include both provider and model id")
	}
	if st.Model.Provider != "" && !providers.Usable(st.Model.Provider, st.User, st.Workspace) {
		return Started{}, ErrModelUnknown
	}
	var active bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND m.user_id = ? AND u.disabled_at IS NULL
	)`, st.Workspace, st.User).Scan(&active); err != nil {
		return Started{}, err
	}
	if !active {
		return Started{}, ErrNotMember
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
	submission, err := rt.Submit(ctx, conversation.agentID, in)
	if err != nil {
		return Started{}, runtimeError(ctx, err)
	}
	if err := s.touch(context.WithoutCancel(ctx), conversation.ID, conversation.agentID); err != nil {
		return Started{}, err
	}
	s.markWhenSettled(rt, conversation.ID, conversation.agentID, submission)
	return Started{Conversation: conversation, Submission: submission}, nil
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
	c, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM conversations
		WHERE origin = ? AND workspace_id = ? AND owner_id = ?`, st.Origin.Key, st.Workspace, st.User))
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if lookupErr := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE origin = ?)`, st.Origin.Key).Scan(&exists); lookupErr != nil {
			return Conversation{}, lookupErr
		}
		if exists {
			return Conversation{}, ErrOrigin
		}
	}
	if err != nil {
		return Conversation{}, err
	}
	if c.Origin == nil || c.Origin.Kind != st.Origin.Kind || c.Origin.SpaceID != st.Origin.SpaceID ||
		c.Origin.AppID != st.Origin.AppID || c.Origin.RoutineID != st.Origin.RoutineID {
		return Conversation{}, ErrOrigin
	}
	return c, nil
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

func (s *Service) Outcome(ctx context.Context, conversation, submission int64, wait time.Duration) (Outcome, error) {
	if s.agent == nil {
		return Outcome{}, ErrRuntimeDown
	}
	agentConversation, err := s.agentConversation(ctx, conversation)
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

func (s *Service) Stop(ctx context.Context, conversation int64) error {
	if s.agent == nil {
		return ErrRuntimeDown
	}
	agentConversation, err := s.agentConversation(ctx, conversation)
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

func (s *Service) agentConversation(ctx context.Context, conversation int64) (int64, error) {
	var agentConversation int64
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM conversations WHERE id = ?`, conversation).Scan(&agentConversation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errNotFound
	}
	return agentConversation, err
}
