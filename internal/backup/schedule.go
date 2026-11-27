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

type Schedule struct {
	At           time.Duration
	Retries      int
	RetryEvery   time.Duration
	Timeout      time.Duration
	CheckTimeout time.Duration
}

func ParseAt(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a time of day as HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

const maxRetryWindow = 20 * time.Hour

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

func (s Schedule) night(t time.Time) time.Time {
	t = t.UTC()
	n := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(s.At)
	if n.After(t) {
		n = n.AddDate(0, 0, -1)
	}
	return n
}

func (s Schedule) grace() time.Duration {
	return time.Duration(s.Retries)*s.RetryEvery + time.Hour
}

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
		last = st.LastAttempt
	}
	return later(now, last.Add(s.RetryEvery))
}

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

type Runner interface {
	Check(ctx context.Context) error
	Backup(ctx context.Context) (string, error)
}

var ErrBusy = errors.New("another scheduled backup of this data directory is running")

const recheck = 10 * time.Minute

type Scheduler struct {
	DataDir  string
	Schedule Schedule
	Runner   Runner
	Log      *slog.Logger

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
			pause = max(s.Schedule.RetryEvery, time.Minute)
		}
		if pause > 0 {
			_ = s.wait(ctx, pause)
		}
	}
	return nil
}

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

func (s *Scheduler) settings(st *Status) {
	st.At = fmt.Sprintf("%02d:%02d", int(s.Schedule.At.Hours()), int(s.Schedule.At.Minutes())%60)
	st.Retries = s.Schedule.Retries
	st.RetryEverySeconds = int64(s.Schedule.RetryEvery / time.Second)
}

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

var errStatus = errors.New("write the backup status")

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

var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]*@`)

const maxError = 1000

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
