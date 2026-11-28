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

type Kind string

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

type Handler func(ctx context.Context, job Job) error

var (
	ErrAccessLost     = errors.New("the job's owner no longer has access")
	ErrOwnerDisabled  = errors.New("the job's owner's account is disabled")
	ErrUnknownKind    = errors.New("no handler is registered for that kind")
	ErrInvalid        = errors.New("invalid job")
	ErrTooMany        = errors.New("too many scheduled jobs")
	ErrNotFound       = errors.New("job not found")
	ErrRegistryClosed = errors.New("the registry is closed")
)

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

const (
	StatePending = "pending"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

const (
	MaxPayload       = 16 << 10
	MaxKeyLength     = 200
	MinEvery         = time.Minute
	MaxPerUser       = 1000
	MaxServerPerUser = 16

	defaultLease       = 2 * time.Minute
	defaultTimeout     = time.Minute
	defaultPoll        = 5 * time.Second
	defaultMaxAttempts = 5
	MaxFailedRuns      = 10
	backoffBase        = 5 * time.Second
	backoffMax         = 5 * time.Minute
	holdDelay          = time.Minute
	keepFinished       = 7 * 24 * time.Hour
	tidyEvery          = time.Hour
	maxErrorLen        = 300
	defaultBatch       = 100
	defaultStopWait    = 10 * time.Second
	purgeChunk         = 500
	maxBackoffShift    = 20

	timeFormat = "2006-01-02T15:04:05.000Z"
)

var (
	MinRunAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	MaxRunAt = time.Date(8999, 12, 31, 23, 59, 59, 999_000_000, time.UTC)
)

type Job struct {
	ID                           int64
	Kind                         Kind
	UserID, WorkspaceID, SpaceID int64
	DedupeKey                    string
	Payload                      json.RawMessage
	RunAt                        time.Time
	Every                        time.Duration
	Revision                     int64

	State     string
	Attempts  int
	LastError string

	lease      int64
	failedRuns int
}

func (j Job) Occurrence() string {
	return fmt.Sprintf("job:%d:r%d:%s", j.ID, j.Revision, j.RunAt.UTC().Format(timeFormat))
}

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Options struct {
	Clock          func() time.Time
	Poll           time.Duration
	Lease, Timeout time.Duration
	MaxAttempts    int
	Batch          int
	TidyEvery      time.Duration
	StopWait       time.Duration
}

type Service struct {
	db     *sql.DB
	access *access.Checker
	clock  func() time.Time

	poll, lease, timeout time.Duration
	tidyEvery, stopWait  time.Duration
	maxAttempts, batch   int

	mu       sync.Mutex
	handlers map[Kind]Handler
	server   map[Kind]bool
	frozen   bool
	kinds    []Kind
	wake     chan struct{}
}

func New(sqlDB *sql.DB, checker *access.Checker, opts Options) *Service {
	s := &Service{
		db: sqlDB, access: checker, clock: opts.Clock,
		poll: opts.Poll, lease: opts.Lease, timeout: opts.Timeout, maxAttempts: opts.MaxAttempts,
		batch: opts.Batch, tidyEvery: opts.TidyEvery, stopWait: opts.StopWait,
		handlers: map[Kind]Handler{}, server: map[Kind]bool{}, wake: make(chan struct{}, 1),
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

func (s *Service) RegisterServer(kind Kind, h Handler) error {
	if err := s.Register(kind, h); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server[kind] = true
	return nil
}

func (s *Service) isServer(kind Kind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server[kind]
}

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

const activeCount = `(SELECT count(*) FROM scheduled_jobs WHERE user_id = %s AND state IN ('pending', 'running')%s)`

func (s *Service) pool(kind Kind) (cond string, args []any, bound int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	server := make([]string, 0, len(s.server))
	for k := range s.server {
		server = append(server, string(k))
	}
	slices.Sort(server)
	if len(server) == 0 {
		return "", nil, MaxPerUser
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(server)), ", ")
	for _, k := range server {
		args = append(args, k)
	}
	if s.server[kind] {
		return " AND kind IN (" + marks + ")", args, MaxServerPerUser
	}
	return " AND kind NOT IN (" + marks + ")", args, MaxPerUser
}

const insertColumns = `kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, created_at, updated_at`

func (s *Service) Schedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	at := stamp(s.clock())
	cond, kinds, bound := s.pool(j.Kind)
	args := []any{string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at,
		j.UserID}
	args = append(append(args, kinds...), bound)
	var id int64
	err = q.QueryRowContext(ctx, `
		INSERT INTO scheduled_jobs (`+insertColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE `+fmt.Sprintf(activeCount, "?", cond)+` < ?
		ON CONFLICT (user_id, kind, dedupe_key) DO NOTHING
		RETURNING id`, args...).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	found, err := exists(ctx, q, j)
	switch {
	case err != nil:
		return false, err
	case found:
		return false, nil
	}
	return false, ErrTooMany
}

func (s *Service) Reschedule(ctx context.Context, q DBTX, j Job) (created bool, err error) {
	payload, err := s.check(j)
	if err != nil {
		return false, err
	}
	if err := placed(ctx, q, j); err != nil {
		return false, err
	}
	at := stamp(s.clock())
	cond, kinds, bound := s.pool(j.Kind)
	args := []any{string(j.Kind), j.UserID, j.WorkspaceID, spaceArg(j.SpaceID), j.DedupeKey, payload, stamp(j.RunAt), everySeconds(j.Every), at, at,
		j.UserID}
	args = append(append(args, kinds...), bound, j.UserID, string(j.Kind), j.DedupeKey)
	args = append(append(args, kinds...), bound)
	var revision int64
	err = q.QueryRowContext(ctx, `
		INSERT INTO scheduled_jobs (`+insertColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE `+fmt.Sprintf(activeCount, "?", cond)+` < ?
		   OR EXISTS (SELECT 1 FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?)
		ON CONFLICT (user_id, kind, dedupe_key) DO UPDATE SET
			workspace_id = excluded.workspace_id, space_id = excluded.space_id, payload = excluded.payload,
			run_at = excluded.run_at, every_seconds = excluded.every_seconds,
			state = 'pending', attempts = 0, failed_runs = 0, retry_at = NULL, lease = lease + 1, lease_until = NULL,
			revision = revision + 1, last_error = NULL, done_at = NULL, updated_at = excluded.updated_at
			WHERE scheduled_jobs.state IN ('pending', 'running') OR `+fmt.Sprintf(activeCount, "excluded.user_id", cond)+` < ?
		RETURNING revision`, args...).Scan(&revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrTooMany
	case err != nil:
		return false, err
	}
	return revision == 0, nil
}

func (s *Service) Cancel(ctx context.Context, q DBTX, userID int64, kind Kind, key string) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?`, userID, string(kind), key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Service) Relocate(ctx context.Context, q DBTX, userID int64, kind Kind, key string, spaceID int64) error {
	_, err := q.ExecContext(ctx, `UPDATE scheduled_jobs SET space_id = ?, updated_at = ?
		WHERE user_id = ? AND kind = ? AND dedupe_key = ? AND state IN ('pending', 'running') AND space_id IS NOT ?
			AND EXISTS (SELECT 1 FROM spaces WHERE id = ? AND workspace_id = scheduled_jobs.workspace_id)`,
		spaceID, stamp(s.clock()), userID, string(kind), key, spaceID, spaceID)
	return err
}

func (s *Service) CancelMatching(ctx context.Context, q DBTX, userID int64, kind Kind, match string, keep ...string) error {
	query, args := `DELETE FROM scheduled_jobs WHERE kind = ? AND dedupe_key GLOB ?`, []any{string(kind), match}
	if userID != 0 {
		query, args = query+` AND user_id = ?`, append(args, userID)
	}
	for _, k := range keep {
		query, args = query+` AND dedupe_key NOT GLOB ?`, append(args, k)
	}
	_, err := q.ExecContext(ctx, query, args...)
	return err
}

func (s *Service) Find(ctx context.Context, q DBTX, userID int64, kind Kind, key string) (Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?`,
		userID, string(kind), key))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

func (s *Service) Now() time.Time { return s.clock() }

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

const jobColumns = `id, kind, user_id, workspace_id, space_id, dedupe_key, payload, run_at, every_seconds, state, attempts, failed_runs, lease, revision, last_error`

type scanner interface{ Scan(dest ...any) error }

type malformedError struct {
	id  int64
	err error
}

func (e *malformedError) Error() string { return fmt.Sprintf("job %d: run_at: %v", e.id, e.err) }
func (e *malformedError) Unwrap() error { return e.err }

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

func (s *Service) RunDue(ctx context.Context) { s.runBatch(ctx, 0) }

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

const purgeQuery = `
	DELETE FROM scheduled_jobs WHERE id IN (
		SELECT id FROM scheduled_jobs WHERE state IN ('done', 'failed') AND updated_at < ? ORDER BY updated_at LIMIT ?)`

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

func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

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

func (s *Service) claim(ctx context.Context) (Job, bool, error) {
	kinds := s.freeze()
	if len(kinds) == 0 {
		return Job{}, false, nil
	}
	var mal *malformedError
	if _, found, err := nextDue(ctx, s.db, kinds, stamp(s.clock())); !found && (err == nil || !errors.As(err, &mal)) {
		return Job{}, false, err
	}
	var j Job
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		for {
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

func (s *Service) quarantine(ctx context.Context, tx *sql.Tx, id int64, at string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE scheduled_jobs SET state = 'failed', retry_at = NULL, lease_until = NULL, last_error = 'its time could not be read',
			updated_at = ?, done_at = ?
		WHERE id = ?`, at, at, id)
	return err
}

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

func nextOccurrence(prev time.Time, every time.Duration, now time.Time) (time.Time, bool) {
	p, e := prev.UnixMilli(), every.Milliseconds()
	lo, hi := MinRunAt.UnixMilli(), MaxRunAt.UnixMilli()
	if e <= 0 || p < lo || p > hi {
		return time.Time{}, false
	}
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

func (s *Service) runOne(ctx context.Context) (bool, error) {
	j, found, err := s.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	h, ok := s.handler(j.Kind)
	if !ok {
		return false, fmt.Errorf("job %d: no handler for %q", j.ID, j.Kind)
	}
	if !s.isServer(j.Kind) {
		err = s.authorize(ctx, j)
	}
	if err == nil {
		err = s.call(ctx, h, j)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return true, s.fail(ctx, j, err)
	}
	return true, s.succeed(ctx, j)
}

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
		if off, err := s.access.Disabled(ctx, j.UserID); err != nil {
			return err
		} else if off {
			return ErrOwnerDisabled
		}
		return ErrAccessLost
	}
	return nil
}

var errTimeout = errors.New("the handler ran past its time limit")

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

func (s *Service) Holds(ctx context.Context, q DBTX, j Job) (bool, error) {
	return s.holds(ctx, q, j, s.clock())
}

func (s *Service) holds(ctx context.Context, q DBTX, j Job, t time.Time) (holds bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scheduled_jobs WHERE id = ? AND state = 'running' AND lease = ? AND lease_until > ?)`,
		j.ID, j.lease, stamp(t)).Scan(&holds)
	return holds, err
}

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

func (s *Service) hold(ctx context.Context, j Job) error {
	slog.Info("schedule: job's owner is disabled; holding it", "job", j.ID, "kind", j.Kind)
	return s.settle(ctx, j, func(tx *sql.Tx, t time.Time) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE scheduled_jobs SET state = 'pending', retry_at = ?, attempts = max(attempts - 1, 0), lease_until = NULL, updated_at = ?
			WHERE id = ?`, stamp(t.Add(holdDelay)), stamp(t), j.ID)
		return err
	})
}

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
