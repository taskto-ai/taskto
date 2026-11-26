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
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/providers"
)

// The steps of a purge that are about files and other processes, not rows (purge_cleanups,
// migration 0050): the agent runtime's records of the account's conversations, the CLI worker's
// directory of the account, and the providers file and key files, which hold the account's provider
// accounts. Each is recorded before it is tried and removed when it is done, so one that can't be
// done now (the agent runtime or the CLI worker is off, or doesn't answer) is tried again by the
// next purge attempt, by the next start and while the server runs (Maintain), and the purge is not
// reported complete while one is left.
const (
	cleanAgentConversation = "agent_conversation"
	cleanCLIHome           = "cli_home"
	cleanProviders         = "providers"
)

// The reasons a step fails, as stored in purge_cleanups.last_error: a fixed word per kind of
// failure, never the text of the error. What a subsystem says in an error can hold what a person
// wrote or what a provider answered (an email in a refusal, a path with a user's id), and this
// column is read by operators and kept with the purge, so nothing free-form reaches it. The word says
// which part failed; the part's own logs say more.
const (
	failSidecarBusy        = "sidecar_busy"           // the agent runtime answered that it is busy
	failSidecarRefused     = "sidecar_refused"        // it answered with another refusal
	failSidecarUnreachable = "sidecar_unreachable"    // it didn't answer, or isn't ready
	failCLIWorker          = "cli_worker_failed"      // the CLI worker couldn't remove the account's files
	failFileRemove         = "file_remove_failed"     // a file or directory here couldn't be removed
	failProviderReload     = "provider_reload_failed" // the runtime refused the providers file, or it couldn't be written
	failProviderKey        = "provider_key_not_removed"
	failBadTarget          = "invalid_target"
	failUnknownStep        = "unknown_step"
	failOther              = "failed"
)

// stepError is a failure of a step, as its fixed reason and, when the cause has one that names no
// one, a number (an HTTP status). The cause itself is held for errors.Is and errors.As, and is never
// printed or stored: Error is the reason alone.
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

// reasonOf is what is stored for a failed step: a stepError's reason (and status), or "failed".
func reasonOf(err error) string {
	var se *stepError
	if errors.As(err, &se) {
		return se.Error()
	}
	return failOther
}

// sidecarFailure classifies an error of a call to the agent runtime.
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

// errWaiting is a step that can't be done until something is on again: not a failure, and not
// done. It stays recorded.
var errWaiting = errors.New("waiting for the agent runtime or the CLI workers to be on")

// SetProviders sets what rewrites the agent runtime's providers file and key files from the
// database and has the runtime reload them (providers.Feed.Sync), for a server that starts the
// agent runtime after the scheduler has the purge's handler. Without one (the agent runtime is
// off) the files are deleted: the next start writes them again from the database.
func (s *Service) SetProviders(sync func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers = sync
}

// providersTarget is the target of userID's rewriting of the providers file. Every account's is its
// own record, never one shared: a record is removed by the run that read it, which began before a
// later purge's accounts were deleted, and a shared one would be that later purge's only retry.
func providersTarget(userID int64) string { return "feed:" + strconv.FormatInt(userID, 10) }

// record notes a step to do for userID, within q. Noting it again changes nothing.
func (s *Service) record(ctx context.Context, q execer, userID int64, kind, target string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO purge_cleanups (user_id, kind, target, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, target) DO NOTHING`, userID, kind, target, s.o.Clock().UTC().Format("2006-01-02T15:04:05.000Z"))
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type cleanup struct {
	id           int64
	user         int64
	kind, target string
}

// pending lists the recorded steps, of userID's purge, or of every account's if userID is 0.
func (s *Service) pending(ctx context.Context, userID int64) ([]cleanup, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, user_id, kind, target FROM purge_cleanups
		WHERE ? = 0 OR user_id = ? ORDER BY id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cleanup
	for rows.Next() {
		var c cleanup
		if err := rows.Scan(&c.id, &c.user, &c.kind, &c.target); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Pending counts the steps still recorded for userID's purge (all accounts' if userID is 0).
func (s *Service) Pending(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.o.DB.QueryRowContext(ctx, `SELECT count(*) FROM purge_cleanups WHERE ? = 0 OR user_id = ?`, userID, userID).Scan(&n)
	return n, err
}

// waiting is the steps that wait for something to be on, by what they wait for (waitsFor) and
// account: how many steps each.
type waiting map[string]map[int64]int

// waitsFor names what a step that is waiting waits for, for the log.
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

// runCleanups does the recorded steps of userID's purge (every account's if userID is 0), and
// removes each that is done. It returns the steps that were tried and failed; a step that is
// waiting for a subsystem to be on is neither (it is counted in waiting).
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
			// Only the step's reason is kept (last_error: a fixed word, see failSidecarBusy), and
			// it is not in the error returned, which the server's log prints. The error says which
			// step failed.
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
		// Bounded from before the runtime is looked up: waiting for a sidecar that keeps
		// restarting, or one that accepts the call and never answers, must not stop the
		// maintenance loop's other steps.
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
	case cleanCLIHome:
		kind, id, _ := strings.Cut(c.target, ":")
		user, err := strconv.ParseInt(id, 10, 64)
		if err != nil || user <= 0 || (kind != cliLocal && kind != cliContainer) {
			return failed(failBadTarget, nil)
		}
		return s.forgetCLI(ctx, kind, user)
	case cleanProviders:
		return s.syncProviders(ctx)
	}
	return failed(failUnknownStep, nil)
}

// defaultAgentTimeout bounds one attempt at an agent runtime step: longer than the runtime's own wait
// for a pinned log, so its busy answer arrives first.
const defaultAgentTimeout = time.Minute

func (s *Service) agentTimeout() time.Duration {
	if s.stepTimeout > 0 {
		return s.stepTimeout
	}
	return defaultAgentTimeout
}

// The kinds of CLI worker, as cli_worker_history names them (migration 0055): where a worker keeps
// a user's files.
const (
	cliLocal     = "local"     // under the data directory, <data>/cli/u<id>
	cliContainer = "container" // in the container worker's own volume
	cliUnknown   = "unknown"   // either may have run, before the record was kept
)

// recordCLIHomes records, within tx, a step for each kind of CLI worker that may hold userID's files:
// each kind that has run on this data directory, and both if one of unknown kind may have run while
// the account existed. None when no worker could have run for them, so an install that never had CLI
// workers has nothing waiting. A kind that runs later, before the account is gone, is added by the
// purge's next try.
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
		return err // a partial list would leave out a worker that holds their files
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

// forgetCLI removes what a CLI worker of kind kept for user's CLI logins (sessions, working
// directories and the browser's profile). Only that kind of worker can say its copy is gone: the
// local one's directory is under the data directory, and is removed from here whatever the mode (by
// the local worker first when it runs, so its logins in progress stop); the container worker's is in
// its own volume, and the step waits until this server runs that worker, which removes it.
func (s *Service) forgetCLI(ctx context.Context, kind string, user int64) error {
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
		if err := os.RemoveAll(filepath.Join(s.o.DataDir, "cli", "u"+strconv.FormatInt(user, 10))); err != nil {
			return failed(failFileRemove, err)
		}
	}
	return nil
}

// syncProviders makes the agent runtime's providers file and key files match the database, which
// no longer has the account's provider accounts. Without the runtime's feed the files are removed:
// the runtime isn't running, and the next start writes them again.
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

// RunCleanups does every recorded step of every account's purge that isn't done, and logs what it
// couldn't. Call it at start, once the agent runtime's client is set (SetAgent), and again whenever
// something a step waits for comes on; Maintain does it on a timer.
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

// Maintain does, every interval until ctx ends, what must go on without a restart: the recorded
// steps that aren't done, and the purge job of an account whose tries ran out or whose job was lost
// (Reconcile), so a purge that failed is tried again for as long as it takes.
//
// It starts with a pass of its own, so the server runs it in the background: the steps pending at
// start are tried at once and never hold up the server's start.
func (s *Service) Maintain(ctx context.Context, interval time.Duration) {
	pass := func() {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("account deletion: scheduling a purge failed; it is tried again", "err", err)
		}
		if err := s.RunCleanups(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("account deletion: a step of a purge failed; it is tried again", "err", err)
		}
	}
	// The first pass only does the recorded steps: the server has just reconciled the purges.
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
