package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/lockfile"
)

// Schedule is when backups run: every night at At, in UTC, and after a failed one again
// every RetryEvery, up to Retries more times that night.
type Schedule struct {
	// At is the time after midnight, UTC.
	At         time.Duration
	Retries    int
	RetryEvery time.Duration
	// Timeout bounds one backup, and CheckTimeout the repository's check before it.
	Timeout      time.Duration
	CheckTimeout time.Duration
}

// ParseAt reads a time of day, "HH:MM", as the time after midnight.
func ParseAt(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a time of day as HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// maxRetryWindow bounds a night's retries, so they end well before the next night.
const maxRetryWindow = 20 * time.Hour

// Validate reports a schedule that can't work.
func (s Schedule) Validate() error {
	switch {
	case s.At < 0 || s.At >= 24*time.Hour:
		return errors.New("the time of day must be between 00:00 and 23:59")
	case s.Retries < 0:
		return errors.New("retries can't be negative")
	case s.Retries > 0 && s.RetryEvery < time.Minute:
		return errors.New("retries must be at least a minute apart")
	case time.Duration(s.Retries)*s.RetryEvery > maxRetryWindow:
		return fmt.Errorf("a night's retries must end within %s", maxRetryWindow)
	case s.Timeout <= 0 || s.CheckTimeout <= 0:
		return errors.New("the timeouts must be positive")
	}
	return nil
}

// night is the scheduled time at or before t.
func (s Schedule) night(t time.Time) time.Time {
	t = t.UTC()
	n := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(s.At)
	if n.After(t) {
		n = n.AddDate(0, 0, -1)
	}
	return n
}

// grace is how long after its scheduled time a night's backup may still succeed, its
// retries done, before the night counts as missed: the retries, and an hour for the last.
func (s Schedule) grace() time.Duration {
	return time.Duration(s.Retries)*s.RetryEvery + time.Hour
}

// next is when the backup after now starts, given st. A night whose backup hasn't
// succeeded is tried at once (the schedule starting late, after a restart or a reboot,
// catches up), then again every RetryEvery after a failure, until it has had Retries
// retries; a night that succeeded, or ran out of retries, waits for the next.
func (s Schedule) next(st Status, now time.Time) time.Time {
	night := s.night(now)
	if !st.LastSuccess.Before(night) {
		return night.AddDate(0, 0, 1)
	}
	failures := 0
	if st.Night.Equal(night) {
		failures = st.Failures
	}
	switch {
	case failures == 0:
		return now
	case failures > s.Retries:
		return night.AddDate(0, 0, 1)
	}
	last := st.LastErrorAt
	if last.IsZero() {
		last = st.LastAttempt // a status written by hand, or damaged: never retry back to back
	}
	return later(now, last.Add(s.RetryEvery))
}

// dueBy is when the next success is due, given st: the night after the last success, plus
// its grace. Before any success, the first start's time plus the grace.
func (s Schedule) dueBy(st Status, now time.Time) time.Time {
	switch {
	case !st.LastSuccess.IsZero():
		return s.night(st.LastSuccess).AddDate(0, 0, 1).Add(s.grace())
	case !st.DueBy.IsZero():
		return st.DueBy
	}
	return now.Add(s.grace())
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Runner runs a backup's two steps.
type Runner interface {
	// Check makes sure the repository answers, before anything is written for the backup.
	Check(ctx context.Context) error
	// Backup backs up the data directory and returns the new snapshot's ID.
	Backup(ctx context.Context) (string, error)
}

// ErrBusy is Attempt's error when another backup of the data directory is running.
var ErrBusy = errors.New("another scheduled backup of this data directory is running")

// recheck is the longest the schedule sleeps without looking at the status again: a
// backup run by hand, a changed clock or a status file put back are seen by then.
const recheck = 10 * time.Minute

// Scheduler runs a data directory's backups on a Schedule, and keeps their status
// (StatusPath) for the server and for monitoring. The status is the marker of the last
// success that retries go by, so a restart neither forgets a failed night nor retries it
// more often.
type Scheduler struct {
	DataDir  string
	Schedule Schedule
	Runner   Runner
	Log      *slog.Logger

	// The clock; tests replace it.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) wait(ctx context.Context, d time.Duration) error {
	if s.sleep != nil {
		return s.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run backs up on the schedule until ctx is done. Its waits end early then, and the loop
// with them.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.Schedule.Validate(); err != nil {
		return err
	}
	announced := time.Time{}
	for ctx.Err() == nil {
		now := s.clock()
		st := s.read()
		next := s.Schedule.next(st, now)
		if d := next.Sub(now); d > 0 {
			if !next.Equal(announced) {
				s.publish(next, now)
				s.log().Info("backup: next one", "at", next.Format(time.RFC3339))
				announced = next
			}
			_ = s.wait(ctx, min(d, recheck))
			continue
		}
		announced = time.Time{}
		_, err := s.Attempt(ctx)
		var pause time.Duration
		switch {
		case errors.Is(err, ErrBusy):
			pause = time.Minute
		case errors.Is(err, errStatus):
			// The failure couldn't be counted: don't try again at once, and forever.
			pause = max(s.Schedule.RetryEvery, time.Minute)
		}
		if pause > 0 {
			_ = s.wait(ctx, pause)
		}
	}
	return nil
}

// read is the status, or a fresh one if it is missing or unreadable: a damaged file
// mustn't stop the backups. A backup that started and never recorded its end (the
// schedule was stopped or killed while it ran) counts as failed, so a backup that keeps
// killing the schedule is retried like any other failure, not at every restart.
func (s *Scheduler) read() Status {
	st, _, err := ReadStatus(s.DataDir)
	if err != nil {
		s.log().Warn("backup: starting a new status", "err", err)
		return Status{}
	}
	if !st.LastAttempt.IsZero() && st.LastAttempt.After(st.LastSuccess) && st.LastAttempt.After(st.LastErrorAt) {
		if night := s.Schedule.night(st.LastAttempt); !st.Night.Equal(night) {
			st.Night, st.Failures = night, 0
		}
		st.Failures++
		st.LastError = "the backup that started then didn't finish: the schedule stopped while it ran"
		st.LastErrorAt = st.LastAttempt
	}
	return st
}

// settings writes the schedule's own settings into st.
func (s *Scheduler) settings(st *Status) {
	st.At = fmt.Sprintf("%02d:%02d", int(s.Schedule.At.Hours()), int(s.Schedule.At.Minutes())%60)
	st.Retries = s.Schedule.Retries
	st.RetryEverySeconds = int64(s.Schedule.RetryEvery / time.Second)
}

// publish records the next backup's time and the schedule's settings, if no backup is
// running (which records them itself when it ends).
func (s *Scheduler) publish(next, now time.Time) {
	lock, err := s.lock()
	if err != nil {
		return
	}
	defer lock.Close()
	st := s.read()
	s.settings(&st)
	st.NextAttempt = next
	st.DueBy = s.Schedule.dueBy(st, now)
	if err := writeStatus(s.DataDir, st); err != nil {
		s.log().Warn("backup: write the status", "err", err)
	}
}

func (s *Scheduler) lock() (*os.File, error) {
	dir := filepath.Join(s.DataDir, StatusDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := lockfile.Lock(filepath.Join(dir, lockName))
	if errors.Is(err, lockfile.ErrLocked) {
		return nil, ErrBusy
	}
	return f, err
}

// errStatus wraps a failure to write the status.
var errStatus = errors.New("write the backup status")

// Attempt runs one backup now, whatever the schedule says, and records how it went. It
// returns the status it recorded, and the backup's error, if any. A backup that fails
// leaves nothing in the data directory: the repository is checked before anything is
// written, and what a failed backup left (in a killed `taskto backup`) is removed.
func (s *Scheduler) Attempt(ctx context.Context) (Status, error) {
	if err := s.Schedule.Validate(); err != nil {
		return Status{}, err
	}
	lock, err := s.lock()
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()

	start := s.clock()
	st := s.read()
	s.settings(&st)
	st.LastAttempt = start
	if st.DueBy.IsZero() {
		st.DueBy = s.Schedule.dueBy(st, start)
	}
	if err := writeStatus(s.DataDir, st); err != nil {
		return st, fmt.Errorf("%w: %w", errStatus, err)
	}
	s.log().Info("backup: starting")

	id, err := s.run(ctx)
	end := s.clock()
	if err == nil {
		st.LastSuccess, st.LastSnapshot = end, id
		st.Night, st.Failures = time.Time{}, 0
		s.log().Info("backup: saved", "snapshot", id)
	} else {
		if night := s.Schedule.night(start); !st.Night.Equal(night) {
			st.Night, st.Failures = night, 0
		}
		st.Failures++
		st.LastError, st.LastErrorAt = Redact(err.Error()), end
		s.log().Error("backup: failed", "err", st.LastError, "failures", st.Failures)
		if err := CleanStale(s.DataDir); err != nil {
			s.log().Warn("backup: remove what the failed backup left", "err", err)
		}
	}
	st.DueBy = s.Schedule.dueBy(st, end)
	st.NextAttempt = s.Schedule.next(st, end)
	if werr := writeStatus(s.DataDir, st); werr != nil {
		err = errors.Join(err, fmt.Errorf("%w: %w", errStatus, werr))
	}
	return st, err
}

// run checks the repository, and only then backs up.
func (s *Scheduler) run(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, s.Schedule.CheckTimeout)
	err := s.Runner.Check(cctx)
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	cancel()
	switch {
	case err == nil:
	case timedOut:
		return "", fmt.Errorf("the repository didn't answer within %s, so nothing was backed up: %w", s.Schedule.CheckTimeout, err)
	default:
		return "", fmt.Errorf("the repository didn't answer, so nothing was backed up: %w", err)
	}
	bctx, cancel := context.WithTimeout(ctx, s.Schedule.Timeout)
	defer cancel()
	id, err := s.Runner.Backup(bctx)
	switch {
	case err == nil:
		return id, nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("stopped before the backup finished: %w", err)
	case errors.Is(bctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("the backup took longer than %s: %w", s.Schedule.Timeout, err)
	}
	return "", err
}

// userinfo is a URL's user and password.
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]*@`)

// maxError bounds an error kept in the status.
const maxError = 1000

// Redact makes an error fit to keep and show: without a URL's user and password (a
// repository's address can carry them), and not too long.
func Redact(msg string) string {
	msg = userinfo.ReplaceAllString(msg, "${1}***@")
	if len(msg) > maxError {
		cut := maxError
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return msg
}
