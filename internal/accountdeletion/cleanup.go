package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/providers"
)

const (
	cleanAgentConversation = "agent_conversation"
	cleanBrowser           = "browser"
	cleanCLIHome           = "cli_home"
	cleanProviders         = "providers"
)

const (
	failSidecarBusy        = "sidecar_busy"
	failSidecarRefused     = "sidecar_refused"
	failSidecarUnreachable = "sidecar_unreachable"
	failCLIWorker          = "cli_worker_failed"
	failFileRemove         = "file_remove_failed"
	failBrowser            = "browser_failed"
	failProviderReload     = "provider_reload_failed"
	failProviderKey        = "provider_key_not_removed"
	failBadTarget          = "invalid_target"
	failUnknownStep        = "unknown_step"
	failOther              = "failed"
)

type stepError struct {
	reason string
	status int
	cause  error
}

func (e *stepError) Error() string {
	if e.status != 0 {
		return e.reason + " status=" + strconv.Itoa(e.status)
	}
	return e.reason
}
func (e *stepError) Unwrap() error { return e.cause }

func failed(reason string, cause error) error { return &stepError{reason: reason, cause: cause} }

func reasonOf(err error) string {
	var se *stepError
	if errors.As(err, &se) {
		return se.Error()
	}
	return failOther
}

func sidecarFailure(err error) error {
	var st *agentrt.StatusError
	if errors.As(err, &st) {
		reason := failSidecarRefused
		if st.Code == http.StatusConflict || st.Code == http.StatusLocked || st.Code == http.StatusTooManyRequests || st.Code == http.StatusServiceUnavailable {
			reason = failSidecarBusy
		}
		return &stepError{reason: reason, status: st.Code, cause: err}
	}
	return failed(failSidecarUnreachable, err)
}

var errWaiting = errors.New("waiting for the agent runtime or the CLI workers to be on")

func (s *Service) SetProviders(sync func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers = sync
}

func providersTarget(userID int64) string { return "feed:" + strconv.FormatInt(userID, 10) }

func (s *Service) record(ctx context.Context, q execer, userID int64, kind, target string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO purge_cleanups (user_id, kind, target, created_at, cli_identity)
		VALUES (?, ?, ?, ?, CASE WHEN ? IN ('browser', 'cli_home') THEN (SELECT cli_identity FROM users WHERE id = ?) END)
		ON CONFLICT (kind, target) DO NOTHING`, userID, kind, target, s.o.Clock().UTC().Format("2006-01-02T15:04:05.000Z"), kind, userID)
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type cleanup struct {
	id           int64
	user         int64
	kind, target string
	identity     string
}

func (s *Service) pending(ctx context.Context, userID int64) ([]cleanup, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, user_id, kind, target, coalesce(cli_identity, '') FROM purge_cleanups
		WHERE ? = 0 OR user_id = ? ORDER BY kind <> ?, id`, userID, userID, cleanBrowser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cleanup
	for rows.Next() {
		var c cleanup
		if err := rows.Scan(&c.id, &c.user, &c.kind, &c.target, &c.identity); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) Pending(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.o.DB.QueryRowContext(ctx, `SELECT count(*) FROM purge_cleanups WHERE ? = 0 OR user_id = ?`, userID, userID).Scan(&n)
	return n, err
}

type waiting map[string]map[int64]int

func waitsFor(c cleanup) string {
	switch {
	case c.kind == cleanAgentConversation:
		return "agent_runtime"
	case c.kind == cleanCLIHome && strings.HasPrefix(c.target, cliContainer+":"):
		return "cli_worker_container"
	case c.kind == cleanCLIHome:
		return "cli_worker_local"
	}
	return c.kind
}

func (s *Service) runCleanups(ctx context.Context, userID int64) (wait waiting, err error) {
	wait = waiting{}
	todo, err := s.pending(ctx, userID)
	if err != nil {
		return wait, err
	}
	var errs []error
	for _, c := range todo {
		if err := ctx.Err(); err != nil {
			return wait, errors.Join(append(errs, err)...)
		}
		switch rerr := s.runCleanup(ctx, c); {
		case rerr == nil:
			if _, err := s.o.DB.ExecContext(ctx, `DELETE FROM purge_cleanups WHERE id = ?`, c.id); err != nil {
				errs = append(errs, err)
			}
		case errors.Is(rerr, errWaiting):
			on := waitsFor(c)
			if wait[on] == nil {
				wait[on] = map[int64]int{}
			}
			wait[on][c.user]++
		default:
			reason := reasonOf(rerr)
			_, _ = s.o.DB.ExecContext(ctx, `UPDATE purge_cleanups SET attempts = attempts + 1, last_error = ? WHERE id = ?`,
				reason, c.id)
			errs = append(errs, fmt.Errorf("step %d (%s) of account %d failed: %s", c.id, c.kind, c.user, reason))
		}
	}
	return wait, errors.Join(errs...)
}

func (s *Service) runCleanup(ctx context.Context, c cleanup) error {
	switch c.kind {
	case cleanAgentConversation:
		id, err := strconv.ParseInt(c.target, 10, 64)
		if err != nil {
			return failed(failBadTarget, err)
		}
		actx, cancel := context.WithTimeout(ctx, s.agentTimeout())
		defer cancel()
		rt, err := s.runtime(actx)
		if err != nil {
			return failed(failSidecarUnreachable, err)
		}
		if rt == nil {
			return errWaiting
		}
		if err := conversations.PurgeConversation(actx, rt, id); err != nil {
			return sidecarFailure(err)
		}
		return nil
	case cleanBrowser:
		user, err := strconv.ParseInt(c.target, 10, 64)
		if err != nil || user <= 0 {
			return failed(failBadTarget, nil)
		}
		return s.forgetBrowser(cliowner.With(ctx, c.identity), user)
	case cleanCLIHome:
		kind, id, _ := strings.Cut(c.target, ":")
		user, err := strconv.ParseInt(id, 10, 64)
		if err != nil || user <= 0 || (kind != cliLocal && kind != cliContainer) {
			return failed(failBadTarget, nil)
		}
		cctx, cancel := context.WithTimeout(ctx, cliForgetTimeout)
		defer cancel()
		return s.forgetCLI(cliowner.With(cctx, c.identity), kind, user)
	case cleanProviders:
		return s.syncProviders(ctx)
	}
	return failed(failUnknownStep, nil)
}

const cliForgetTimeout = 2 * time.Minute

const defaultAgentTimeout = time.Minute

func (s *Service) agentTimeout() time.Duration {
	if s.stepTimeout > 0 {
		return s.stepTimeout
	}
	return defaultAgentTimeout
}

const (
	cliLocal     = "local"
	cliContainer = "container"
	cliUnknown   = "unknown"
)

func (s *Service) recordCLIHomes(ctx context.Context, tx *sql.Tx, userID int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT h.worker FROM cli_worker_history h
		WHERE h.worker <> ? OR h.since >= coalesce((SELECT created_at FROM users WHERE id = ?), '')`, cliUnknown, userID)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return err
		}
		if k == cliUnknown {
			kinds[cliLocal], kinds[cliContainer] = true, true
		} else {
			kinds[k] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, k := range []string{cliLocal, cliContainer} {
		if !kinds[k] {
			continue
		}
		if err := s.record(ctx, tx, userID, cleanCLIHome, k+":"+strconv.FormatInt(userID, 10)); err != nil {
			return err
		}
	}
	return nil
}

const defaultBrowserTimeout = 2 * time.Minute

func (s *Service) forgetBrowser(ctx context.Context, user int64) error {
	if s.o.Browsers == nil {
		return nil
	}
	timeout := defaultBrowserTimeout
	if s.stepTimeout > 0 {
		timeout = s.stepTimeout
	}
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.o.Browsers.Forget(bctx, user); err != nil {
		return failed(failBrowser, err)
	}
	return nil
}

func (s *Service) browserPending(ctx context.Context, user int64) (bool, error) {
	var n int
	err := s.o.DB.QueryRowContext(ctx, `SELECT count(*) FROM purge_cleanups WHERE user_id = ? AND kind = ?`, user, cleanBrowser).Scan(&n)
	return n > 0, err
}

func (s *Service) forgetCLI(ctx context.Context, kind string, user int64) error {
	if pending, err := s.browserPending(ctx, user); err != nil {
		return err
	} else if pending {
		return errWaiting
	}
	running := ""
	if s.o.CLI != nil {
		running = s.o.CLI.WorkerKind()
	}
	if running == kind {
		if err := s.o.CLI.Forget(ctx, user); err != nil {
			if errors.Is(err, cliaccounts.ErrOff) {
				return errWaiting
			}
			return failed(failCLIWorker, err)
		}
	} else if kind == cliContainer {
		return errWaiting
	}
	if kind == cliLocal && s.o.DataDir != "" {
		identity, _ := cliowner.From(ctx)
		if err := cliworker.ForgetLocal(cliworker.Layout{Root: filepath.Join(s.o.DataDir, "cli")}, identity, user); err != nil {
			return failed(failFileRemove, err)
		}
	}
	return nil
}

func (s *Service) syncProviders(ctx context.Context) error {
	s.mu.Lock()
	sync := s.providers
	s.mu.Unlock()
	if sync != nil {
		if err := sync(ctx); err != nil {
			if errors.Is(err, providers.ErrKeyNotRemoved) {
				return failed(failProviderKey, err)
			}
			return failed(failProviderReload, err)
		}
		return nil
	}
	if s.o.DataDir == "" {
		return nil
	}
	dir := filepath.Join(s.o.DataDir, "agent")
	for _, name := range []string{"providers.json", "keys"} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return failed(failFileRemove, err)
		}
	}
	return nil
}

func (s *Service) RunCleanups(ctx context.Context) error {
	wait, err := s.runCleanups(ctx, 0)
	for _, on := range slices.Sorted(maps.Keys(wait)) {
		steps := 0
		for _, n := range wait[on] {
			steps += n
		}
		slog.Info("account deletion: steps of purges are waiting for what they need to be on",
			"waits_for", on, "accounts", len(wait[on]), "steps", steps)
	}
	return err
}

func (s *Service) Maintain(ctx context.Context, interval time.Duration) {
	pass := func() {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("account deletion: scheduling a purge failed; it is tried again", "err", err)
		}
		if err := s.RunCleanups(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("account deletion: a step of a purge failed; it is tried again", "err", err)
		}
	}
	if err := s.RunCleanups(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("account deletion: a step of a purge failed; it is tried again", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
