package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Long-lived connections made with a session (the agent browser's live view, a conversation's
// event stream) are checked once, when they open, so they have to learn when the login ends.
// WatchSession gives one a context that ends then, in two ways that back each other up: the code
// that ends logins (signing out, revoking from the sessions list, a password change or reset)
// cancels the watches of those logins at once,
// and each watch looks its session up again every interval, so a login ended by another server
// process, or a signal that was missed, still drops the connection within an interval.

// liveWatches holds the open watches by user.
type liveWatches struct {
	mu     sync.Mutex
	byUser map[int64]map[*liveWatch]struct{}
}

type liveWatch struct {
	hash   string // the hash of the session token, "" matches none
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

// end cancels userID's watches: all of them, or only those of the session whose token hash is
// hash, if it isn't empty.
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

// WatchSession returns a context derived from r's that ends when the login r carries ends,
// and a stop func that releases the watch (call it when the connection closes). Without a valid session the context is already ended. every is how often the session
// is looked up again.
func (s *Service) WatchSession(r *http.Request, every time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	token := s.sessionToken(r)
	u, err := s.sessionUser(ctx, token)
	if token == "" || err != nil {
		cancel()
		return ctx, cancel
	}
	w := &liveWatch{hash: hashToken(token), cancel: cancel}
	s.live.add(u.ID, w)
	stop := func() {
		s.live.remove(u.ID, w)
		cancel()
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
					// An error that isn't "no such login" (the database is busy) keeps the
					// connection until the next look; the signal covers the usual case.
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

// endLive tells the watches of userID's logins that they ended: all of them, or the one whose
// token is given.
func (s *Service) endLive(userID int64, token string) {
	hash := ""
	if token != "" {
		hash = hashToken(token)
	}
	s.live.end(userID, hash)
}

// endLiveHashes tells the watches of userID's logins, named by their token hashes, that they ended.
func (s *Service) endLiveHashes(userID int64, hashes ...string) {
	for _, h := range hashes {
		if h != "" {
			s.live.end(userID, h)
		}
	}
}
