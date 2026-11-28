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

const modelWriteTimeout = 15 * time.Second

type modelHop struct {
	notice, note string
}

type hopClaim struct {
	ready chan struct{}
	hop   *modelHop
	retry bool
	at    time.Time
}

type hopStep int

const (
	hopWritten hopStep = iota
	hopSkipped
	hopAbandoned
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

func (s *Service) settleHop(turn string, c *hopClaim, hop *modelHop, retry bool) {
	s.mu.Lock()
	c.hop, c.retry, c.at = hop, retry, time.Now()
	if retry {
		delete(s.hops, turn)
	}
	s.mu.Unlock()
	close(c.ready)
}

func (s *Service) awaitHop(ctx context.Context, turn string) (*modelHop, bool) {
	for {
		s.mu.Lock()
		c, ok := s.hops[turn]
		s.mu.Unlock()
		if !ok {
			return nil, false
		}
		select {
		case <-c.ready:
		case <-ctx.Done():
			return nil, true
		}
		if !c.retry {
			return c.hop, true
		}
	}
}

func (s *Service) hopOnce(ctx context.Context, p conversations.Principal, turn, stopped, note string) *modelHop {
	for {
		c, owner := s.claimHop(turn)
		if !owner {
			select {
			case <-c.ready:
			case <-ctx.Done():
				return nil
			}
			if c.retry {
				continue
			}
			return c.hop
		}
		hop := s.hopToModel(ctx, p, stopped, note)
		s.settleHop(turn, c, hop, hop == nil && ctx.Err() != nil)
		return hop
	}
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
		case hopAbandoned:
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
	default:
		slog.WarnContext(ctx, "cli turn: setting the conversation's model after a limit", "conversation", p.Conversation, "err", err)
		return hopSkipped
	}
}
