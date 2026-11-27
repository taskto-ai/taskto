package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type liveWatches struct {
	mu     sync.Mutex
	byUser map[int64]map[*liveWatch]struct{}
}

type liveWatch struct {
	hash   string
	cancel context.CancelFunc
}

func (l *liveWatches) add(userID int64, w *liveWatch) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = map[int64]map[*liveWatch]struct{}{}
	}
	if l.byUser[userID] == nil {
		l.byUser[userID] = map[*liveWatch]struct{}{}
	}
	l.byUser[userID][w] = struct{}{}
}

func (l *liveWatches) remove(userID int64, w *liveWatch) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byUser[userID], w)
	if len(l.byUser[userID]) == 0 {
		delete(l.byUser, userID)
	}
}

func (l *liveWatches) end(userID int64, hash string) {
	l.mu.Lock()
	var ended []*liveWatch
	for w := range l.byUser[userID] {
		if hash == "" || w.hash == hash {
			ended = append(ended, w)
		}
	}
	l.mu.Unlock()
	for _, w := range ended {
		w.cancel()
	}
}

func (s *Service) WatchSession(r *http.Request, every time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	token := s.sessionToken(r)
	u, err := s.sessionUser(ctx, token)
	if token == "" || err != nil {
		cancel()
		return ctx, cancel
	}
	s.at("watch-session-looked-up")
	w := &liveWatch{hash: hashToken(token), cancel: cancel}
	s.live.add(u.ID, w)
	stop := func() {
		s.live.remove(u.ID, w)
		cancel()
	}
	if _, err := s.sessionUser(ctx, token); err != nil {
		if !errors.Is(err, errNotFound) && ctx.Err() == nil {
			slog.WarnContext(ctx, "auth: verifying a new live session", "err", err)
		}
		stop()
		return ctx, stop
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := s.sessionUser(ctx, token); err != nil {
					if errors.Is(err, errNotFound) {
						cancel()
						return
					}
					if ctx.Err() == nil {
						slog.WarnContext(ctx, "auth: checking a live session", "err", err)
					}
				}
			}
		}
	}()
	return ctx, stop
}

func (s *Service) endLive(userID int64, token string) {
	hash := ""
	if token != "" {
		hash = hashToken(token)
	}
	s.live.end(userID, hash)
}

func (s *Service) endLiveHashes(userID int64, hashes ...string) {
	for _, h := range hashes {
		if h != "" {
			s.live.end(userID, h)
		}
	}
}
