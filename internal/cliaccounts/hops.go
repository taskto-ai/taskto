package cliaccounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/conversations"
)

var ErrModelBusy = errors.New("the conversation keeps its model until its continued reply ends")

var ErrModelUncertain = errors.New("the agent runtime may have set the model")

const (
	modelWriteTimeout = 15 * time.Second
	modelReadTimeout  = 5 * time.Second
)

type ModelReader func(ctx context.Context, agentConversation int64) (provider, modelID string, err error)

func (s *Service) SetModelReader(f ModelReader) { s.readModel = f }

func uncertain(err error) bool {
	return errors.Is(err, ErrModelUncertain) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (s *Service) reconcile(ctx context.Context, p conversations.Principal, e queueEntry) hopStep {
	if s.readModel == nil {
		return hopUnsure
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelReadTimeout)
	defer cancel()
	provider, model, err := s.readModel(rctx, p.AgentConversation)
	if err != nil || provider != e.provider || model != e.model {
		slog.WarnContext(ctx, "cli turn: the model write is not confirmed, no other model is tried", "conversation", p.Conversation, "err", err)
		return hopUnsure
	}
	return hopWritten
}

type modelHop struct {
	notice, note string
}

type hopClaim struct {
	ready chan struct{}
	hop   *modelHop
	at    time.Time
}

type hopStep int

const (
	hopWritten hopStep = iota
	hopSkipped
	hopAbandoned
	hopUnsure
)

func (s *Service) claimHop(turn string) (*hopClaim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.hops[turn]; ok {
		return c, false
	}
	now := time.Now()
	for k, old := range s.hops {
		select {
		case <-old.ready:
			if now.Sub(old.at) > time.Hour {
				delete(s.hops, k)
			}
		default:
		}
	}
	c := &hopClaim{ready: make(chan struct{}), at: now}
	s.hops[turn] = c
	return c, true
}

func (s *Service) settleHop(c *hopClaim, hop *modelHop) {
	s.mu.Lock()
	c.hop, c.at = hop, time.Now()
	s.mu.Unlock()
	close(c.ready)
}

func (s *Service) hopOnce(ctx context.Context, p conversations.Principal, turn, stopped, note string) *modelHop {
	c, owner := s.claimHop(turn)
	if !owner {
		select {
		case <-c.ready:
			return c.hop
		case <-ctx.Done():
			return nil
		}
	}
	hop := s.hopToModel(ctx, p, stopped, note)
	s.settleHop(c, hop)
	return hop
}

func (s *Service) hopToModel(ctx context.Context, p conversations.Principal, stopped, note string) *modelHop {
	q, err := s.queue(ctx, p.User)
	if err != nil {
		slog.ErrorContext(ctx, "cli turn: choosing the next model", "conversation", p.Conversation, "err", err)
		return nil
	}
	for _, e := range q {
		if !e.isModel() {
			continue
		}
		switch s.writeModel(ctx, p, e) {
		case hopWritten:
			return &modelHop{notice: fmt.Sprintf("%s: continuing on %s", stopped, e.model), note: note}
		case hopAbandoned, hopUnsure:
			return nil
		}
	}
	return nil
}

func (s *Service) writeModel(ctx context.Context, p conversations.Principal, e queueEntry) hopStep {
	if s.setModel == nil || ctx.Err() != nil || !s.stillActive(ctx, p) {
		return hopAbandoned
	}
	if on, err := s.switchingOn(ctx, p.User); err != nil || !on {
		return hopAbandoned
	}
	offered, err := s.offers(ctx, e.provider, e.model, p.User, p.Workspace)
	if err != nil {
		slog.ErrorContext(ctx, "cli turn: checking the next model", "conversation", p.Conversation, "err", err)
		return hopAbandoned
	}
	if !offered {
		return hopSkipped
	}
	if ctx.Err() != nil {
		return hopAbandoned
	}
	wctx, cancel := context.WithTimeout(ctx, s.modelWait)
	defer cancel()
	switch err := s.setModel(wctx, p.AgentConversation, e.provider, e.model); {
	case err == nil:
		return hopWritten
	case errors.Is(err, ErrModelBusy):
		slog.InfoContext(ctx, "cli turn: the conversation keeps its model", "conversation", p.Conversation)
		return hopAbandoned
	case uncertain(err):
		slog.WarnContext(ctx, "cli turn: the model write has no answer, reading the model back", "conversation", p.Conversation, "err", err)
		return s.reconcile(ctx, p, e)
	default:
		slog.WarnContext(ctx, "cli turn: setting the conversation's model after a limit", "conversation", p.Conversation, "err", err)
		return hopSkipped
	}
}
