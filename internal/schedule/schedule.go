// Package schedule is the general scheduler: timed work that runs for a person, later, once or on
// a repeat, and survives a restart.
//
// Reminders, integrations' polling and scheduled app actions are built on it. The package has no
// feature of its own: the code that owns a feature registers a handler for its kind (Register),
// and schedules, reschedules and cancels jobs (Schedule, Reschedule, Cancel) as the feature's
// state changes. Those three take the caller's transaction (or the database), so a job is
// scheduled, moved or cancelled in the same commit as the change that needs it.
//
// # Jobs
//
// A job is a kind, the user it runs for, the workspace (and space, where it is about one) it is
// in, a dedupe key, a small JSON payload and when it is due, with an optional fixed interval for
// one that repeats. The kinds are a closed list: the handlers the server's Go code registers
// before the worker starts, and nothing can be registered after. Scheduling a kind with no
// handler is refused, and the worker leaves the jobs of a kind it has no handler for as they are
// (a downgrade doesn't lose them). The dedupe key names what the job is for ("the reminder of
// card 7"), and is unique per owner and kind: scheduling it again changes nothing (Schedule), and
// a changed time replaces it (Reschedule).
//
// The payload holds IDs and settings, never a secret and never a copy of what people wrote: the
// handler reads what it needs when it runs. Errors a handler returns are kept on the job
// (shortened) and logged, so they too name no secret and no content.
//
// # Running
//
// A worker (Start) runs in the server process, which the data directory has one of (package
// lockfile), so no extra process and no lock between workers is needed beyond the claim itself. It
// takes the job that is due with a lease (Options.Lease), runs its handler with a time limit, and
// notes the outcome only if it still holds the lease: a claim raises the job's lease token, and
// so does a reschedule, so a worker that was too slow, or whose job was changed under it, can
// never finish the job for a later claim.
//
// Delivery is at least once. A worker that dies (or is stopped) while it runs a job leaves it
// running; when the lease ends the job is taken again, so a handler can be called more than once
// for an occurrence, and must be idempotent. The occurrence's key (Job.Occurrence) is the same on
// every try of it, and is what a handler should use to make what it makes only once (a
// notification's dedupe key, a card's origin). Tries are bounded: a failure is tried again after
// growing waits, up to Options.MaxAttempts counting the ones that ended in a crash, and a job
// whose tries are used up has failed, and stays listed with its last error until it is purged or
// scheduled again. A recurring job moves on to its next occurrence instead (the failed one is not
// retried further), and stops only after MaxFailedRuns of them in a row. A recurring job runs once
// when it is due, even if several occurrences passed while the server was stopped: the missed ones
// are skipped, and the next is the first on its grid, counted from the one that was due, that is
// still ahead.
//
// # Access is checked when the job runs
//
// What a person could do when a job was scheduled says nothing about what they may do when it
// runs: they may have left the workspace, lost a space, or had their role lowered. Before a
// handler is called the worker checks that the owner can still read the job's space (or, for a
// job with no space, is still a member of its workspace), through access.Checker, and a job whose
// owner can't has failed with ErrAccessLost and its handler is never called. That is a floor, not
// the check: a handler acts for the owner and must check, through access.Checker, every right
// the work needs (write to a space, an app's role, the target of a card) at the moment it needs
// it, as close to the write as it can, and return an error that wraps ErrAccessLost if it is
// refused. Such a job isn't tried again. No handler acts with the authority of whoever scheduled
// it, or of the server.
package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/access"
)

// Kind names the work of a job. It is chosen in Go, never by a client.
type Kind string

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// Handler does the work of one occurrence of a job, as the job's owner. It must be idempotent (see
// the package comment) and check the owner's access itself. It returns nil when the work is done
// (or there was none to do), ErrAccessLost (wrapped or not) if the owner may no longer do it,
// Permanent(err) for a failure that trying again can't fix, and any other error for one that may
// pass.
type Handler func(ctx context.Context, job Job) error

// Errors of the package.
var (
	// ErrAccessLost is what a job fails with when its owner may no longer do its work. A handler
	// returns it (or an error wrapping it) when the owner's access is refused.
	ErrAccessLost = errors.New("the job's owner no longer has access")
	// ErrUnknownKind is a kind with no handler.
	ErrUnknownKind = errors.New("no handler is registered for that kind")
	// ErrInvalid is a job that can't be scheduled as it is.
	ErrInvalid = errors.New("invalid job")
	// ErrTooMany is a person with more jobs waiting than the bound.
	ErrTooMany = errors.New("too many scheduled jobs")
	// ErrNotFound is no job with that kind and key for that owner.
	ErrNotFound = errors.New("job not found")
	// ErrRegistryClosed is a kind registered after the worker started.
	ErrRegistryClosed = errors.New("the registry is closed")
)

// permanent is an error a retry can't fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Permanent marks err as one that trying again can't fix: the job fails at once.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// States of a job.
const (
	StatePending = "pending"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Bounds and timings.
const (
	// MaxPayload bounds a job's payload, in bytes.
	MaxPayload = 16 << 10
	// MaxKeyLength bounds a dedupe key, in bytes.
	MaxKeyLength = 200
	// MinEvery is the shortest interval of a recurring job.
	MinEvery = time.Minute
	// MaxPerUser bounds one person's jobs that wait or run.
	MaxPerUser = 1000

	defaultLease       = 2 * time.Minute
	defaultTimeout     = time.Minute
	defaultPoll        = 5 * time.Second
	defaultMaxAttempts = 5
	// MaxFailedRuns is how many occurrences of a recurring job in a row may use all their tries
	// before the job fails.
	MaxFailedRuns = 10
	backoffBase   = 5 * time.Second
	backoffMax    = 5 * time.Minute
	// keepFinished is how long a job that is done or has failed is kept, for its dedupe key and its
	// error to be seen.
	keepFinished = 7 * 24 * time.Hour
	tidyEvery    = time.Hour
	maxErrorLen  = 300

	// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
	timeFormat = "2006-01-02T15:04:05.000Z"
)

// Job is a scheduled job as handlers and callers see it.
type Job struct {
	ID   int64
	Kind Kind
	// UserID is whom it runs for; WorkspaceID and SpaceID (0: none) where.
	UserID, WorkspaceID, SpaceID int64
	// DedupeKey names what the job is for; unique per owner and kind.
	DedupeKey string
	// Payload is a JSON object.
	Payload json.RawMessage
	// RunAt is when the occurrence is due. It is the same on every try of it.
	RunAt time.Time
	// Every is the interval of a recurring job, 0 for one that runs once.
	Every time.Duration

	// State, Attempts (the tries of this occurrence, this one included while it runs) and LastError
	// are what the worker records.
	State     string
	Attempts  int
	LastError string

	lease      int64
	failedRuns int
}

// Occurrence is a key that is the same on every try of this occurrence of the job and differs
// from every other occurrence of any job: what a handler makes only once is keyed by it.
func (j Job) Occurrence() string {
	return fmt.Sprintf("job:%d:%s", j.ID, j.RunAt.UTC().Format(timeFormat))
}

// DBTX is what Schedule, Reschedule, Cancel and Find need of the database: a *sql.Tx (the usual
// case, so the job is committed with the change that needs it) or the *sql.DB.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Options tune the service; the zero value is what the server uses.
type Options struct {
	// Clock tells the time; tests set it. It defaults to time.Now.
	Clock func() time.Time
	// Poll is how long the worker waits before it looks for due jobs again, if nothing wakes it.
	Poll time.Duration
	// Lease is how long a worker owns a job it took; Timeout bounds one try and must be shorter.
	Lease, Timeout time.Duration
	// MaxAttempts bounds the tries of one occurrence.
	MaxAttempts int
}

// Service is the scheduler.
type Service struct {
	db     *sql.DB
	access *access.Checker
	clock  func() time.Time

	poll, lease, timeout time.Duration
	maxAttempts          int

	mu       sync.Mutex
	handlers map[Kind]Handler
	frozen   bool
	kinds    []Kind // the registered kinds, sorted, once frozen
	wake     chan struct{}
}

// New returns the scheduler over sqlDB, which must be migrated. access is what jobs' owners are
// checked with when their jobs run.
func New(sqlDB *sql.DB, checker *access.Checker, opts Options) *Service {
	s := &Service{
		db: sqlDB, access: checker, clock: opts.Clock,
		poll: opts.Poll, lease: opts.Lease, timeout: opts.Timeout, maxAttempts: opts.MaxAttempts,
		handlers: map[Kind]Handler{}, wake: make(chan struct{}, 1),
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.poll <= 0 {
		s.poll = defaultPoll
	}
	if s.lease <= 0 {
		s.lease = defaultLease
	}
	if s.timeout <= 0 || s.timeout >= s.lease {
		s.timeout = min(defaultTimeout, s.lease/2)
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = defaultMaxAttempts
	}
	return s
}

// Register sets the handler of a kind. It is for server startup, before Start: the list of kinds
// is closed once the worker runs (ErrRegistryClosed), and a kind is registered once.
func (s *Service) Register(kind Kind, h Handler) error {
	if !kindPattern.MatchString(string(kind)) {
		return fmt.Errorf("%w: kind %q is lower-case letters, digits, . _ -, up to 64", ErrInvalid, kind)
	}
	if h == nil {
		return fmt.Errorf("%w: kind %q has no handler", ErrInvalid, kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return ErrRegistryClosed
	}
	if _, dup := s.handlers[kind]; dup {
		return fmt.Errorf("%w: kind %q is registered twice", ErrInvalid, kind)
	}
	s.handlers[kind] = h
	return nil
}

// freeze closes the registry and returns what the worker needs of it.
func (s *Service) freeze() []Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.frozen {
		s.frozen = true
		for k := range s.handlers {
			s.kinds = append(s.kinds, k)
		}
		slices.Sort(s.kinds)
	}
	return s.kinds
}

func (s *Service) handler(kind Kind) (Handler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.handlers[kind]
	return h, ok
}

func stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

func parse(v string) (time.Time, error) { return time.Parse(timeFormat, v) }

// check validates a job for scheduling and fills in what it leaves out; it returns the payload as
// stored.
func (s *Service) check(j Job) (payload string, err error) {
	if _, ok := s.handler(j.Kind); !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, j.Kind)
	}
	switch {
	case j.UserID <= 0 || j.WorkspaceID <= 0 || j.SpaceID < 0:
		return "", fmt.Errorf("%w: a job needs its owner and workspace", ErrInvalid)
	case j.DedupeKey == "" || len(j.DedupeKey) > MaxKeyLength:
		return "", fmt.Errorf("%w: the dedupe key is 1 to %d bytes", ErrInvalid, MaxKeyLength)
	case j.RunAt.IsZero():
		return "", fmt.Errorf("%w: a job needs the time it is due", ErrInvalid)
	case j.Every != 0 && j.Every < MinEvery:
		return "", fmt.Errorf("%w: a recurring job repeats at most once a minute", ErrInvalid)
	case j.Every%time.Second != 0:
		return "", fmt.Errorf("%w: the interval is whole seconds", ErrInvalid)
	}
	if len(j.Payload) == 0 {
		return "{}", nil
	}
	if len(j.Payload) > MaxPayload {
		return "", fmt.Errorf("%w: the payload is at most %d bytes", ErrInvalid, MaxPayload)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(j.Payload, &obj); err != nil || obj == nil {
		return "", fmt.Errorf("%w: the payload is a JSON object", ErrInvalid)
	}
	return string(j.Payload), nil
}

func everySeconds(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return int64(d / time.Second)
}

func spaceArg(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// room checks, unless the job exists already, that its owner has fewer than MaxPerUser jobs that
// wait or run.
func (s *Service) room(ctx context.Context, q DBTX, j Job) error {
	var n int
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM scheduled_jobs WHERE user_id = ? AND state IN ('pending', 'running')`, j.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPerUser {
		return ErrTooMany
	}
	return nil
}

// placed checks that the job's space, if it has one, is in its workspace.
func placed(ctx context.Context, q DBTX, j Job) error {
	if j.SpaceID == 0 {
		return nil
	}
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spaces WHERE id = ? AND workspace_id = ?)`,
		j.SpaceID, j.WorkspaceID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the space is not in the workspace", ErrInvalid)
	}
	return nil
}

func exists(ctx context.Context, q DBTX, j Job) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?)`,
		j.UserID, string(j.Kind), j.DedupeKey).Scan(&found)
	return found, err
}

// Schedule adds the job, unless the owner already has one of that kind and key, in any state: then
// it changes nothing and reports created false. Call it with the transaction of the change the
// job belongs to. The caller has checked that the owner may do what the job will (the job checks
// again when it runs); the job's space must be in its workspace.
func (s *Service) Schedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	if found, err := exists(ctx, q, j); err != nil || found {
		return false, err
	}
	if err := s.room(ctx, q, j); err != nil {
		return false, err
	}
	at := stamp(s.clock())
	res, err := q.ExecContext(ctx, `
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, kind, dedupe_key) DO NOTHING`,
		string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Reschedule sets the job of that owner, kind and key to run as given, adding it if there is none,
// and starts it afresh: it is pending, with no tries and no error, whatever it was (running,
// done, failed). A worker that is running the job at that moment finishes its try without effect
// on the new schedule, so the handler may run once more for the old occurrence. It reports whether
// the job is new.
func (s *Service) Reschedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	found, err := exists(ctx, q, j)
	if err != nil {
		return false, err
	}
	if !found {
		if err := s.room(ctx, q, j); err != nil {
			return false, err
		}
	}
	at := stamp(s.clock())
	_, err = q.ExecContext(ctx, `
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, kind, dedupe_key) DO UPDATE SET
			workspace_id = excluded.workspace_id, space_id = excluded.space_id, payload = excluded.payload,
			run_at = excluded.run_at, every_seconds = excluded.every_seconds,
			state = 'pending', attempts = 0, failed_runs = 0, retry_at = NULL, lease = lease + 1, lease_until = NULL,
			last_error = NULL, done_at = NULL, updated_at = excluded.updated_at`,
		string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at)
	return !found, err
}

// Cancel removes the job of that owner, kind and key, which frees the key, and reports whether
// there was one. A worker that is running it finishes its try without effect.
func (s *Service) Cancel(ctx context.Context, q DBTX, userID int64, kind Kind, key string) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?`, userID, string(kind), key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Find returns the job of that owner, kind and key, or ErrNotFound.
func (s *Service) Find(ctx context.Context, q DBTX, userID int64, kind Kind, key string) (Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?`,
		userID, string(kind), key))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Wake tells the worker, if it runs in this process, to look for due jobs now. Call it after the
// transaction that scheduled a job has committed, for a job that is due already; the worker looks
// by itself every Options.Poll.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

const jobColumns = `id, kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, state, attempts, failed_runs, lease, last_error`

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (Job, error) {
	var j Job
	var kind, payload, runAt string
	var space, every sql.NullInt64
	var lastError sql.NullString
	if err := row.Scan(&j.ID, &kind, &j.UserID, &j.WorkspaceID, &space, &j.DedupeKey, &payload, &runAt, &every,
		&j.State, &j.Attempts, &j.failedRuns, &j.lease, &lastError); err != nil {
		return Job{}, err
	}
	j.Kind, j.SpaceID, j.Payload, j.LastError = Kind(kind), space.Int64, json.RawMessage(payload), lastError.String
	j.Every = time.Duration(every.Int64) * time.Second
	t, err := parse(runAt)
	if err != nil {
		return Job{}, fmt.Errorf("job %d: run_at: %w", j.ID, err)
	}
	j.RunAt = t
	return j, nil
}

// Start starts the worker and returns the function that stops it and waits for it. It closes the
// registry. The data directory has one server, so one worker.
func (s *Service) Start(ctx context.Context) (stop func()) {
	s.freeze()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poll := time.NewTicker(s.poll)
		defer poll.Stop()
		tidy := time.NewTicker(tidyEvery)
		defer tidy.Stop()
		for {
			s.RunDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			case <-s.wake:
			case <-tidy.C:
				if err := s.purge(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("schedule: purging old jobs failed", "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// RunDue runs the jobs that are due now, one after another, and returns when none is: what the
// worker does on each round, and what tests call, since they have no worker.
func (s *Service) RunDue(ctx context.Context) {
	for ctx.Err() == nil {
		ok, err := s.runOne(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("schedule: worker", "err", err)
			}
			return
		}
		if !ok {
			return
		}
	}
}

// purge removes the jobs that are done or have failed and were last touched long ago.
func (s *Service) purge(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE state IN ('done', 'failed') AND updated_at < ?`,
		stamp(s.clock().Add(-keepFinished)))
	return err
}

// write runs fn in a transaction, which takes the write lock when it begins (package db).
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// claim takes the next job that is due: one that waits, or one whose worker's lease has run out,
// of a kind that has a handler. A job whose lease ran out after its last permitted try isn't tried
// again: that occurrence has failed.
func (s *Service) claim(ctx context.Context) (Job, bool, error) {
	kinds := s.freeze()
	if len(kinds) == 0 {
		return Job{}, false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(kinds)), ", ")
	var j Job
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		t := s.clock()
		at := stamp(t)
		args := make([]any, 0, len(kinds)+2)
		for _, k := range kinds {
			args = append(args, string(k))
		}
		args = append(args, at, at)
		for {
			c, err := scanJob(tx.QueryRowContext(ctx, `
				SELECT `+jobColumns+` FROM scheduled_jobs
				WHERE kind IN (`+marks+`)
				  AND ((state = 'pending' AND coalesce(retry_at, run_at) <= ?) OR (state = 'running' AND lease_until <= ?))
				ORDER BY coalesce(retry_at, run_at), id LIMIT 1`, args...))
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if c.Attempts >= s.maxAttempts {
				// Its worker died (or kept being stopped) on the last try: the bound holds.
				if err := s.exhausted(ctx, tx, c, "gave up: its worker stopped on the last try"); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE scheduled_jobs SET state = 'running', attempts = attempts + 1, lease = lease + 1, lease_until = ?, last_run_at = ?, updated_at = ?
				WHERE id = ?`, stamp(t.Add(s.lease)), at, at, c.ID); err != nil {
				return err
			}
			c.Attempts++
			c.lease++
			c.State = StateRunning
			j, found = c, true
			return nil
		}
	})
	return j, found, err
}

// exhausted ends the tries of j's occurrence: a recurring job moves on to its next occurrence
// (and fails after MaxFailedRuns of them in a row), any other fails. The caller holds the write
// lock and the job's lease.
func (s *Service) exhausted(ctx context.Context, tx *sql.Tx, j Job, reason string) error {
	t := s.clock()
	at := stamp(t)
	reason = shorten(reason)
	if j.Every > 0 && j.failedRuns+1 < MaxFailedRuns {
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'pending', run_at = ?, retry_at = NULL, attempts = 0, failed_runs = failed_runs + 1,
				lease_until = NULL, last_error = ?, updated_at = ?
			WHERE id = ? AND lease = ?`, stamp(nextOccurrence(j.RunAt, j.Every, t)), reason, at, j.ID, j.lease)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = ?, updated_at = ?, done_at = ?
		WHERE id = ? AND lease = ?`, reason, at, at, j.ID, j.lease)
	return err
}

// nextOccurrence is the first time on prev's grid of every that is after now.
func nextOccurrence(prev time.Time, every time.Duration, now time.Time) time.Time {
	next := prev.Add(every)
	if next.After(now) {
		return next
	}
	return prev.Add((now.Sub(prev)/every + 1) * every)
}

// runOne runs the next job that is due, if there is one.
func (s *Service) runOne(ctx context.Context) (bool, error) {
	j, found, err := s.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	h, ok := s.handler(j.Kind)
	if !ok { // can't be: claim takes only the kinds that have one
		return false, fmt.Errorf("job %d: no handler for %q", j.ID, j.Kind)
	}
	err = s.authorize(ctx, j)
	if err == nil {
		err = s.call(ctx, h, j)
	}
	if ctx.Err() != nil {
		return false, ctx.Err() // stopping: the lease runs out and the job is taken again
	}
	if err != nil {
		return true, s.fail(ctx, j, err)
	}
	return true, s.succeed(ctx, j)
}

// authorize is the floor of the access check: the owner may still read the job's space, or, with
// none, is still a member of its workspace. A space of another workspace than the job's is no
// access either.
func (s *Service) authorize(ctx context.Context, j Job) error {
	if j.SpaceID != 0 {
		sp, err := s.access.Check(ctx, j.UserID, j.SpaceID)
		switch {
		case errors.Is(err, access.ErrNotFound):
			return ErrAccessLost
		case err != nil:
			return err
		case sp.WorkspaceID != j.WorkspaceID:
			return ErrAccessLost
		}
		return nil
	}
	spaces, err := s.access.Readable(ctx, j.UserID, j.WorkspaceID)
	if err != nil {
		return err
	}
	if len(spaces) == 0 {
		return ErrAccessLost
	}
	return nil
}

// call runs the handler with a time limit; a panic is an error of the try.
func (s *Service) call(ctx context.Context, h Handler, j Job) (err error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return h(ctx, j)
}

// succeed notes the occurrence as done, if the worker still owns the job: a job that runs once is
// done, a recurring one waits for its next occurrence.
func (s *Service) succeed(ctx context.Context, j Job) error {
	t := s.clock()
	at := stamp(t)
	if j.Every > 0 {
		_, err := s.db.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'pending', run_at = ?, retry_at = NULL, attempts = 0, failed_runs = 0,
				lease_until = NULL, last_error = NULL, updated_at = ?
			WHERE id = ? AND state = 'running' AND lease = ?`, stamp(nextOccurrence(j.RunAt, j.Every, t)), at, j.ID, j.lease)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'done', retry_at = NULL, lease_until = NULL, last_error = NULL, updated_at = ?, done_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, at, at, j.ID, j.lease)
	return err
}

// fail notes why a try failed: the job waits for the next one, moves on, or has failed for good.
// Nothing changes if the worker no longer owns the job.
func (s *Service) fail(ctx context.Context, j Job, cause error) error {
	var perm permanent
	switch {
	case errors.Is(cause, ErrAccessLost):
		slog.Info("schedule: job's owner lost access", "job", j.ID, "kind", j.Kind)
		return s.end(ctx, j, "access lost")
	case errors.As(cause, &perm):
		slog.Warn("schedule: job failed", "job", j.ID, "kind", j.Kind, "attempt", j.Attempts, "err", cause)
		return s.end(ctx, j, perm.Error())
	}
	slog.Warn("schedule: job try failed", "job", j.ID, "kind", j.Kind, "attempt", j.Attempts, "err", cause)
	reason := shorten(cause.Error())
	if j.Attempts >= s.maxAttempts {
		return s.write(ctx, func(tx *sql.Tx) error { return s.exhausted(ctx, tx, j, reason) })
	}
	t := s.clock()
	wait := min(backoffBase<<(j.Attempts-1), backoffMax)
	_, err := s.db.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'pending', retry_at = ?, lease_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, stamp(t.Add(wait)), reason, stamp(t), j.ID, j.lease)
	return err
}

// end fails the job for good, whatever it was: a failure no retry fixes stops a recurring job too.
func (s *Service) end(ctx context.Context, j Job, reason string) error {
	at := stamp(s.clock())
	_, err := s.db.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = ?, updated_at = ?, done_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, shorten(reason), at, at, j.ID, j.lease)
	return err
}

func shorten(s string) string {
	if r := []rune(s); len(r) > maxErrorLen {
		return string(r[:maxErrorLen])
	}
	return s
}
