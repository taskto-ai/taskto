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
// never finish the job for a later claim. The lease must also still be running: the outcome of a
// try that ended after its lease did is dropped, whether or not anyone else has claimed the job
// since, and the job is taken again by the recovery of expired leases like one whose worker died.
// The token, the state and the lease's end are checked together, under the write lock, on every
// outcome.
//
// A handler must honour its context. The time limit (Options.Timeout) and the worker's stop are
// told to it only by cancelling the context it is given; a handler that ignores it holds the
// worker until it returns (the queue waits behind it), and its result, if it comes after the
// deadline, counts as a failed try, never as a success. Stopping waits a bounded time
// (Options.StopWait) for a handler that does not return, then leaves its job running for the lease
// to end.
//
// Delivery is at least once. A worker that dies (or is stopped) while it runs a job leaves it
// running; when the lease ends the job is taken again, so a handler can be called more than once
// for an occurrence, and must be idempotent. The occurrence's key (Job.Occurrence) is the same on
// every try of it, and is what a handler should use to make what it makes only once (a
// notification's dedupe key, a card's origin). It includes the job's revision, which a reschedule
// raises, so work scheduled again at the same time with new content is a new occurrence. Tries are bounded: a failure is tried again after
// growing waits, up to Options.MaxAttempts counting the ones that ended in a crash, and a job
// whose tries are used up has failed, and stays listed with its last error until it is purged or
// scheduled again. A recurring job moves on to its next occurrence instead (the failed one is not
// retried further), and stops only after MaxFailedRuns of them in a row. A recurring job runs once
// when it is due, even if several occurrences passed while the server was stopped: the missed ones
// are skipped, and the next is the first on its grid, counted from the one that was due, that is
// still ahead. A recurring job with no later occurrence before MaxRunAt is done.
//
// # Times
//
// A job is due between MinRunAt and MaxRunAt (UTC): the times it stores sort as text, which holds
// for the years 2000 to 8999 only, and the database refuses any other. Scheduling, rescheduling
// and a recurring job's next occurrence all stay inside the range, with integer arithmetic that
// can't overflow.
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
// refused. Such a job isn't tried again. An owner whose account is disabled
// (access.ErrDisabled) has not lost access for good: the job is held, not failed (below). No handler acts with the authority of whoever scheduled
// it, or of the server.
//
// # A disabled owner's jobs wait
//
// An account an admin has disabled keeps everything it owns and gets its access back as it was when
// it is enabled. Its jobs therefore neither run nor fail: the worker finds the owner disabled
// (ErrOwnerDisabled, or access.ErrDisabled from a handler's own check), puts the job back to
// pending to look again after holdDelay, without using up the try, recording nothing as a failure
// (a recurring job's occurrences are not counted as failed runs), and calls no handler. Enabling the
// account lets them run at the next look, a recurring one on its own grid from then on.
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
	// ErrOwnerDisabled is what the worker finds when a job's owner's account is disabled: the job
	// is held until it is enabled, not failed. A handler may return it, or an error wrapping
	// access.ErrDisabled, for the same.
	ErrOwnerDisabled = errors.New("the job's owner's account is disabled")
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
	// MaxPerUser bounds one person's jobs that wait or run, counted at every change into one of
	// those states (a new job, and a finished job scheduled again).
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
	// holdDelay is how long a job of a disabled owner waits before the worker looks again.
	holdDelay = time.Minute
	// keepFinished is how long a job that is done or has failed is kept, for its dedupe key and its
	// error to be seen.
	keepFinished = 7 * 24 * time.Hour
	tidyEvery    = time.Hour
	maxErrorLen  = 300
	// defaultBatch is how many jobs the worker runs before it lets the cleanup have its turn and
	// looks at the clock and the stop again; a queue that is always due can't starve either.
	defaultBatch = 100
	// defaultStopWait is how long stopping the worker waits for a handler that ignores its context.
	defaultStopWait = 10 * time.Second
	// purgeChunk bounds the rows one delete removes, so the write lock is given back between them.
	purgeChunk = 500
	// maxBackoffShift keeps backoffBase<<shift from overflowing, far above where backoffMax cuts it.
	maxBackoffShift = 20

	// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
	timeFormat = "2006-01-02T15:04:05.000Z"
)

// The times a job may be due, in UTC, inclusive: the years its stored text sorts correctly.
var (
	MinRunAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	MaxRunAt = time.Date(8999, 12, 31, 23, 59, 59, 999_000_000, time.UTC)
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
	// Revision is how many times the job has been rescheduled; it is 0 for a new one.
	Revision int64

	// State, Attempts (the tries of this occurrence, this one included while it runs) and LastError
	// are what the worker records.
	State     string
	Attempts  int
	LastError string

	lease      int64
	failedRuns int
}

// Occurrence is a key that is the same on every try of this occurrence of the job and differs
// from every other occurrence of any job, and from the same time scheduled again by Reschedule
// (new revision): what a handler makes only once is keyed by it.
func (j Job) Occurrence() string {
	return fmt.Sprintf("job:%d:r%d:%s", j.ID, j.Revision, j.RunAt.UTC().Format(timeFormat))
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
	// Batch is how many jobs the worker runs before cleanup and the stop get a turn.
	Batch int
	// TidyEvery is how often the worker purges old finished jobs.
	TidyEvery time.Duration
	// StopWait bounds how long the function Start returns waits for the worker.
	StopWait time.Duration
}

// Service is the scheduler.
type Service struct {
	db     *sql.DB
	access *access.Checker
	clock  func() time.Time

	poll, lease, timeout time.Duration
	tidyEvery, stopWait  time.Duration
	maxAttempts, batch   int

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
		batch: opts.Batch, tidyEvery: opts.TidyEvery, stopWait: opts.StopWait,
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
	if s.batch <= 0 {
		s.batch = defaultBatch
	}
	if s.tidyEvery <= 0 {
		s.tidyEvery = tidyEvery
	}
	if s.stopWait <= 0 {
		s.stopWait = defaultStopWait
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
	case j.RunAt.Before(MinRunAt) || j.RunAt.After(MaxRunAt):
		return "", fmt.Errorf("%w: a job is due between the years 2000 and 8999", ErrInvalid)
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

// activeCount is the number of jobs of one owner that wait or run: the ones the bound counts.
const activeCount = `(SELECT count(*) FROM scheduled_jobs WHERE user_id = %s AND state IN ('pending', 'running'))`

const insertColumns = `kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, created_at, updated_at`

// Schedule adds the job, unless the owner already has one of that kind and key, in any state: then
// it changes nothing and reports created false. Call it with the transaction of the change the
// job belongs to. The caller has checked that the owner may do what the job will (the job checks
// again when it runs); the job's space must be in its workspace. The owner's bound is checked by
// the statement that inserts, so it holds for concurrent callers on the database too.
func (s *Service) Schedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	at := stamp(s.clock())
	var id int64
	err = q.QueryRowContext(ctx, `
		INSERT INTO scheduled_jobs (`+insertColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE `+fmt.Sprintf(activeCount, "?")+` < ?
		ON CONFLICT (user_id, kind, dedupe_key) DO NOTHING
		RETURNING id`,
		string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at,
		j.UserID, MaxPerUser).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	// Nothing was inserted: the job exists already (nothing to do), or the owner is at the bound.
	found, err := exists(ctx, q, j)
	switch {
	case err != nil:
		return false, err
	case found:
		return false, nil
	}
	return false, ErrTooMany
}

// Reschedule sets the job of that owner, kind and key to run as given, adding it if there is none,
// and starts it afresh: it is pending, with no tries and no error, whatever it was (running,
// done, failed), and its revision is one higher, so its occurrences are new ones even at the same
// time. A worker that is running the job at that moment finishes its try without effect on the new
// schedule, so the handler may run once more for the old occurrence. It reports whether the job is
// new. Making a job that is done or failed pending again counts against the owner's bound like a
// new job, and is refused with ErrTooMany at it.
func (s *Service) Reschedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	at := stamp(s.clock())
	// The insert is attempted when the owner has room or the job exists (a conflict then updates it);
	// the update applies when the job is active already, or the owner has room for it to become so.
	// Both are decided inside the one statement. A revision of 0 is a row that was just inserted.
	var revision int64
	err = q.QueryRowContext(ctx, `
		INSERT INTO scheduled_jobs (`+insertColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE `+fmt.Sprintf(activeCount, "?")+` < ?
		   OR EXISTS (SELECT 1 FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?)
		ON CONFLICT (user_id, kind, dedupe_key) DO UPDATE SET
			workspace_id = excluded.workspace_id, space_id = excluded.space_id, payload = excluded.payload,
			run_at = excluded.run_at, every_seconds = excluded.every_seconds,
			state = 'pending', attempts = 0, failed_runs = 0, retry_at = NULL, lease = lease + 1, lease_until = NULL,
			revision = revision + 1, last_error = NULL, done_at = NULL, updated_at = excluded.updated_at
			WHERE scheduled_jobs.state IN ('pending', 'running') OR `+fmt.Sprintf(activeCount, "excluded.user_id")+` < ?
		RETURNING revision`,
		string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at,
		j.UserID, MaxPerUser, j.UserID, string(j.Kind), j.DedupeKey, MaxPerUser).Scan(&revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrTooMany
	case err != nil:
		return false, err
	}
	return revision == 0, nil
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

// Now is the scheduler's clock: what a feature decides "already past" against, so it agrees with
// the worker (and with a test's fake clock).
func (s *Service) Now() time.Time { return s.clock() }

// Wake tells the worker, if it runs in this process, to look for due jobs now. Call it after the
// transaction that scheduled a job has committed, for a job that is due already; the worker looks
// by itself every Options.Poll.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

const jobColumns = `id, kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, state, attempts, failed_runs, lease, revision, last_error`

type scanner interface{ Scan(dest ...any) error }

// malformedError is a row whose stored time can't be read or is out of range. The database refuses
// such a row, so it means a bug or damage; the worker fails the job and goes on (quarantine).
type malformedError struct {
	id  int64
	err error
}

func (e *malformedError) Error() string { return fmt.Sprintf("job %d: run_at: %v", e.id, e.err) }
func (e *malformedError) Unwrap() error { return e.err }

// scanJob reads a row of jobColumns, then extra. A row whose time is bad is a *malformedError.
func scanJob(row scanner, extra ...any) (Job, error) {
	var j Job
	var kind, payload, runAt string
	var space, every sql.NullInt64
	var lastError sql.NullString
	dest := append([]any{&j.ID, &kind, &j.UserID, &j.WorkspaceID, &space, &j.DedupeKey, &payload, &runAt, &every,
		&j.State, &j.Attempts, &j.failedRuns, &j.lease, &j.Revision, &lastError}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Job{}, err
	}
	j.Kind, j.SpaceID, j.Payload, j.LastError = Kind(kind), space.Int64, json.RawMessage(payload), lastError.String
	j.Every = time.Duration(every.Int64) * time.Second
	t, err := parse(runAt)
	if err == nil && (t.Before(MinRunAt) || t.After(MaxRunAt)) {
		err = errors.New("outside the supported years")
	}
	if err != nil {
		return Job{ID: j.ID}, &malformedError{id: j.ID, err: err}
	}
	j.RunAt = t
	return j, nil
}

// Start starts the worker and returns the function that stops it and waits for it, for at most
// Options.StopWait: a handler that ignores its context is not waited for longer, and its job is
// left running for its lease to end. The function says whether the worker returned: false means a
// handler may still be using the database, and the caller must not close it. It closes the
// registry. The data directory has one server, so
// one worker.
func (s *Service) Start(ctx context.Context) (stop func() bool) {
	s.freeze()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poll := time.NewTicker(s.poll)
		defer poll.Stop()
		tidy := time.NewTicker(s.tidyEvery)
		defer tidy.Stop()
		for {
			more := s.runBatch(ctx, s.batch)
			// The cleanup gets its turn between batches, so a queue that is always due can't starve it.
			select {
			case <-tidy.C:
				s.tidy(ctx)
			default:
			}
			if more {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			case <-s.wake:
			case <-tidy.C:
				s.tidy(ctx)
			}
		}
	}()
	return func() bool {
		cancel()
		select {
		case <-done:
			return true
		case <-time.After(s.stopWait):
			slog.Warn("schedule: a handler did not return when the worker was stopped; its job is left to its lease", "wait", s.stopWait)
			return false
		}
	}
}

func (s *Service) tidy(ctx context.Context) {
	if err := s.purge(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("schedule: purging old jobs failed", "err", err)
	}
}

// RunDue runs the jobs that are due now, one after another, and returns when none is: what tests
// call, since they have no worker. The worker runs them in batches (runBatch).
func (s *Service) RunDue(ctx context.Context) { s.runBatch(ctx, 0) }

// runBatch runs due jobs one after another until none is due, the context is done, or limit jobs
// have run (0: no limit); it reports whether it stopped at the limit, with more perhaps due.
func (s *Service) runBatch(ctx context.Context, limit int) (more bool) {
	for n := 0; ctx.Err() == nil; n++ {
		if limit > 0 && n >= limit {
			return true
		}
		ok, err := s.runOne(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("schedule: worker", "err", err)
			}
			return false
		}
		if !ok {
			return false
		}
	}
	return false
}

// purgeQuery deletes a chunk of the finished jobs that were last touched before the first argument.
const purgeQuery = `
	DELETE FROM scheduled_jobs WHERE id IN (
		SELECT id FROM scheduled_jobs WHERE state IN ('done', 'failed') AND updated_at < ? ORDER BY updated_at LIMIT ?)`

// purge removes the jobs that are done or have failed and were last touched long ago, a chunk at a
// time so the write lock is given back between chunks.
func (s *Service) purge(ctx context.Context) error {
	cutoff := stamp(s.clock().Add(-keepFinished))
	for {
		res, err := s.db.ExecContext(ctx, purgeQuery, cutoff, purgeChunk)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n < purgeChunk {
			return err
		}
	}
}

// write runs fn in a transaction, which takes the write lock when it begins (package db).
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The transaction's own context is never cancelled. database/sql rolls a transaction back on a
	// goroutine of its own when its context ends, and the connection stays busy until that has
	// finished: the deferred Rollback below then answers sql.ErrTxDone at once, and a caller that
	// closes the database or begins another write (a worker at shutdown) can't tell. Cancelling
	// ctx still stops fn's statements, which take ctx themselves, and fn's error is rolled back here,
	// before write returns. A BEGIN waiting for the write lock gives up after busy_timeout.
	tx, err := s.db.BeginTx(context.WithoutCancel(ctx), nil)
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
	// Commit no longer sees the caller's cancellation (the transaction's context is detached): a
	// caller that gave up is told so, and its writes are rolled back, not committed behind it.
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// dueQueries are nextDue's two queries, for n kinds; the first argument of each is the time, the
// rest the kinds. A test reads their plans.
func dueQueries(n int) []string {
	marks := strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
	return []string{
		`SELECT ` + jobColumns + `, coalesce(retry_at, run_at) FROM scheduled_jobs
		 WHERE state = 'pending' AND coalesce(retry_at, run_at) <= ? AND kind IN (` + marks + `)
		 ORDER BY coalesce(retry_at, run_at), id LIMIT 1`,
		`SELECT ` + jobColumns + `, lease_until FROM scheduled_jobs
		 WHERE state = 'running' AND lease_until <= ? AND kind IN (` + marks + `)
		 ORDER BY lease_until, id LIMIT 1`,
	}
}

// nextDue reads the job that is due first at `at` among the kinds that have a handler: the pending
// job whose next try (or run_at) has come, or the running job whose lease has ended. Each is the
// first row of a partial index (scheduled_jobs_pending_due, scheduled_jobs_lease_ended), so the
// cost doesn't grow with the queue. A row with a bad time is returned as a *malformedError.
func nextDue(ctx context.Context, q DBTX, kinds []Kind, at string) (Job, bool, error) {
	args := func() []any {
		a := make([]any, 0, len(kinds)+1)
		a = append(a, at)
		for _, k := range kinds {
			a = append(a, string(k))
		}
		return a
	}
	var best Job
	var bestDue string
	var found bool
	for _, query := range dueQueries(len(kinds)) {
		var due string
		c, err := scanJob(q.QueryRowContext(ctx, query, args()...), &due)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return c, false, err
		}
		if !found || due < bestDue {
			best, bestDue, found = c, due, true
		}
	}
	return best, found, nil
}

// claim takes the next job that is due: one that waits, or one whose worker's lease has run out,
// of a kind that has a handler. A job whose lease ran out after its last permitted try isn't tried
// again: that occurrence has failed. A job with a bad time has failed, and the claim goes on.
func (s *Service) claim(ctx context.Context) (Job, bool, error) {
	kinds := s.freeze()
	if len(kinds) == 0 {
		return Job{}, false, nil
	}
	// A look without the write lock: an idle poll, and a queue that is waiting, don't take it.
	var mal *malformedError
	if _, found, err := nextDue(ctx, s.db, kinds, stamp(s.clock())); !found && (err == nil || !errors.As(err, &mal)) {
		return Job{}, false, err
	}
	var j Job
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		for {
			// The time is read again every round: recovering a job takes a moment, and a clock that
			// stepped back meanwhile must not leave the cutoff (or the lease's start) ahead of it, which
			// would dispatch an occurrence early.
			t := s.clock()
			at := stamp(t)
			c, ok, err := nextDue(ctx, tx, kinds, at)
			var mal *malformedError
			switch {
			case errors.As(err, &mal):
				slog.Error("schedule: a job has a time that can't be read; failing it", "job", mal.id, "err", mal.err)
				if err := s.quarantine(ctx, tx, mal.id, at); err != nil {
					return err
				}
				continue
			case err != nil:
				return err
			case !ok:
				return nil
			}
			if c.Attempts >= s.maxAttempts {
				// Its worker died (or kept being stopped) on the last try: the bound holds.
				if err := s.exhausted(ctx, tx, c, t, "gave up: its worker stopped on the last try"); err != nil {
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

// quarantine fails a job whose stored time can't be read, with no handler call, so one bad row
// can't stall the others.
func (s *Service) quarantine(ctx context.Context, tx *sql.Tx, id int64, at string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = 'its time could not be read',
			updated_at = ?, done_at = ?
		WHERE id = ?`, at, at, id)
	return err
}

// exhausted ends the tries of j's occurrence at t: a recurring job moves on to its next occurrence
// (and fails after MaxFailedRuns of them in a row, or when there is none within the supported
// years), any other fails. The caller holds the write lock and the job's claim.
func (s *Service) exhausted(ctx context.Context, tx *sql.Tx, j Job, t time.Time, reason string) error {
	at := stamp(t)
	reason = shorten(reason)
	if j.Every > 0 && j.failedRuns+1 < MaxFailedRuns {
		if next, ok := nextOccurrence(j.RunAt, j.Every, t); ok {
			_, err := tx.ExecContext(ctx, `
				UPDATE scheduled_jobs SET state = 'pending', run_at = ?, retry_at = NULL, attempts = 0, failed_runs = failed_runs + 1,
					lease_until = NULL, last_error = ?, updated_at = ?
				WHERE id = ? AND lease = ?`, stamp(next), reason, at, j.ID, j.lease)
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = ?, updated_at = ?, done_at = ?
		WHERE id = ? AND lease = ?`, reason, at, at, j.ID, j.lease)
	return err
}

// nextOccurrence is the first time on prev's grid of every that is after now, and false if that is
// past MaxRunAt (or prev is outside the supported years). It is integer arithmetic in
// milliseconds, checked so that nothing overflows, whatever prev, every and now are.
func nextOccurrence(prev time.Time, every time.Duration, now time.Time) (time.Time, bool) {
	p, e := prev.UnixMilli(), every.Milliseconds()
	lo, hi := MinRunAt.UnixMilli(), MaxRunAt.UnixMilli()
	if e <= 0 || p < lo || p > hi {
		return time.Time{}, false
	}
	// n is now, held inside [p, hi] (compared as times first: UnixMilli itself overflows for the
	// extremes), so no step below can overflow.
	n := p
	switch {
	case now.After(MaxRunAt):
		n = hi
	case now.After(prev):
		n = now.UnixMilli()
	}
	k := (n-p)/e + 1
	if k > (hi-p)/e {
		return time.Time{}, false
	}
	return time.UnixMilli(p + k*e).UTC(), true
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
		case errors.Is(err, access.ErrDisabled):
			return ErrOwnerDisabled
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
		// A disabled account sees no spaces: that is no loss of access to fail the job for.
		if off, err := s.access.Disabled(ctx, j.UserID); err != nil {
			return err
		} else if off {
			return ErrOwnerDisabled
		}
		return ErrAccessLost
	}
	return nil
}

// errTimeout is a try that ran past its time limit.
var errTimeout = errors.New("the handler ran past its time limit")

// call runs the handler with a time limit; a panic is an error of the try. A handler that returns
// nil after the limit has passed has not done its work as far as the worker can tell (it may have
// been cut short and not said so): the try failed.
func (s *Service) call(ctx context.Context, h Handler, j Job) (err error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	err = h(cctx, j)
	if err == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		err = errTimeout
	}
	return err
}

// Holds reports whether j, the job as a handler was given it, is still the claim that runs: the
// job exists and is running under the same lease token, and the lease has not ended. A handler
// that makes something other than a note of its own outcome asks it in the transaction that makes
// it, which holds the write lock, and makes nothing if it does not: the job may have been
// cancelled or rescheduled, or taken again by another claim, since the handler was called.
func (s *Service) Holds(ctx context.Context, q DBTX, j Job) (bool, error) {
	return s.holds(ctx, q, j, s.clock())
}

func (s *Service) holds(ctx context.Context, q DBTX, j Job, t time.Time) (holds bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scheduled_jobs WHERE id = ? AND state = 'running' AND lease = ? AND lease_until > ?)`,
		j.ID, j.lease, stamp(t)).Scan(&holds)
	return holds, err
}

// settle runs fn, the outcome of j's try, in a transaction, if j's claim still holds: the job is
// running, under the same lease token, and the lease has not ended. Those are checked under the
// write lock, together, so a try that ran past its lease changes nothing, whether or not another
// worker took the job since; the recovery of the expired lease (claim) is the one path that deals
// with it. fn gets the time the check was made.
func (s *Service) settle(ctx context.Context, j Job, fn func(tx *sql.Tx, t time.Time) error) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		t := s.clock()
		holds, err := s.holds(ctx, tx, j, t)
		if err != nil {
			return err
		}
		if !holds {
			slog.Info("schedule: a try ended without its lease; its outcome is dropped", "job", j.ID, "kind", j.Kind)
			return nil
		}
		return fn(tx, t)
	})
}

// succeed notes the occurrence as done, if the worker still holds the job: a job that runs once is
// done, a recurring one waits for its next occurrence (and is done when it has none).
func (s *Service) succeed(ctx context.Context, j Job) error {
	return s.settle(ctx, j, func(tx *sql.Tx, t time.Time) error {
		at := stamp(t)
		if j.Every > 0 {
			if next, ok := nextOccurrence(j.RunAt, j.Every, t); ok {
				_, err := tx.ExecContext(ctx, `
					UPDATE scheduled_jobs SET state = 'pending', run_at = ?, retry_at = NULL, attempts = 0, failed_runs = 0,
						lease_until = NULL, last_error = NULL, updated_at = ?
					WHERE id = ?`, stamp(next), at, j.ID)
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'done', retry_at = NULL, lease_until = NULL, last_error = NULL, updated_at = ?, done_at = ?
			WHERE id = ?`, at, at, j.ID)
		return err
	})
}

// fail notes why a try failed: the job waits for the next one, moves on, or has failed for good.
// Nothing changes if the worker no longer holds the job.
func (s *Service) fail(ctx context.Context, j Job, cause error) error {
	var perm permanent
	switch {
	case errors.Is(cause, ErrOwnerDisabled), errors.Is(cause, access.ErrDisabled):
		return s.hold(ctx, j)
	case errors.Is(cause, ErrAccessLost):
		slog.Info("schedule: job's owner lost access", "job", j.ID, "kind", j.Kind)
		return s.end(ctx, j, "access lost")
	case errors.As(cause, &perm):
		slog.Warn("schedule: job failed", "job", j.ID, "kind", j.Kind, "attempt", j.Attempts, "err", cause)
		return s.end(ctx, j, perm.Error())
	}
	slog.Warn("schedule: job try failed", "job", j.ID, "kind", j.Kind, "attempt", j.Attempts, "err", cause)
	reason := shorten(cause.Error())
	return s.settle(ctx, j, func(tx *sql.Tx, t time.Time) error {
		if j.Attempts >= s.maxAttempts {
			return s.exhausted(ctx, tx, j, t, reason)
		}
		wait := min(backoffBase<<min(j.Attempts-1, maxBackoffShift), backoffMax)
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'pending', retry_at = ?, lease_until = NULL, last_error = ?, updated_at = ?
			WHERE id = ?`, stamp(t.Add(wait)), reason, stamp(t), j.ID)
		return err
	})
}

// hold puts a job whose owner is disabled back to pending, to be looked at again after holdDelay,
// with the try it took given back, and nothing noted as a failure. Nothing changes if the worker no
// longer holds the job.
func (s *Service) hold(ctx context.Context, j Job) error {
	slog.Info("schedule: job's owner is disabled; holding it", "job", j.ID, "kind", j.Kind)
	return s.settle(ctx, j, func(tx *sql.Tx, t time.Time) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'pending', retry_at = ?, attempts = max(attempts - 1, 0), lease_until = NULL, updated_at = ?
			WHERE id = ?`, stamp(t.Add(holdDelay)), stamp(t), j.ID)
		return err
	})
}

// end fails the job for good, whatever it was: a failure no retry fixes stops a recurring job too.
func (s *Service) end(ctx context.Context, j Job, reason string) error {
	return s.settle(ctx, j, func(tx *sql.Tx, t time.Time) error {
		at := stamp(t)
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = ?, updated_at = ?, done_at = ?
			WHERE id = ?`, shorten(reason), at, at, j.ID)
		return err
	})
}

func shorten(s string) string {
	if r := []rune(s); len(r) > maxErrorLen {
		return string(r[:maxErrorLen])
	}
	return s
}
