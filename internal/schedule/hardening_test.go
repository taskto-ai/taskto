package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

// at is the stored text of a time.
func at(t time.Time) string { return stamp(t) }

// rawInsert adds a row the way damage or a bug would: with the CHECK constraints off, on one
// connection, so a test can show how the worker treats a row the database would refuse.
func rawInsert(t *testing.T, w *world, key, runAt string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := w.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = OFF`) }()
	res, err := conn.ExecContext(ctx, `
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, string(remind), w.alice, w.ws, key, runAt, at(start), at(start))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func state(t *testing.T, w *world, id int64) (state, lastError string) {
	t.Helper()
	var e sql.NullString
	if err := w.db.QueryRow(`SELECT state, last_error FROM scheduled_jobs WHERE id = ?`, id).Scan(&state, &e); err != nil {
		t.Fatal(err)
	}
	return state, e.String
}

func TestATimeOutsideTheSupportedYearsIsRefused(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	ctx := context.Background()
	for name, when := range map[string]time.Time{
		"year 10000":  time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		"year 1700":   time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC),
		"year 1999":   time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC),
		"year 9000":   time.Date(9000, 1, 1, 0, 0, 0, 0, time.UTC),
		"far, far on": time.Date(292277026, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		j := w.job("k", 0)
		j.RunAt = when
		if _, err := w.s.Schedule(ctx, w.db, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("schedule at %s = %v, want ErrInvalid", name, err)
		}
		if _, err := w.s.Reschedule(ctx, w.db, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("reschedule at %s = %v, want ErrInvalid", name, err)
		}
	}
	// The edges of the range are inside it.
	for _, when := range []time.Time{MinRunAt, MaxRunAt} {
		j := w.job("edge-"+when.Format("2006"), 0)
		j.RunAt = when
		w.schedule(j)
		if got := w.find(w.alice, j.DedupeKey); !got.RunAt.Equal(when) {
			t.Errorf("RunAt = %v, want %v", got.RunAt, when)
		}
	}
}

func TestTheDatabaseRefusesATimeThatSortsWrongOrDoesNotParse(t *testing.T) {
	w := newWorld(t)
	for _, bad := range []string{
		"10000-01-01T00:00:00.000Z", "1999-12-31T23:59:59.999Z", "9000-01-01T00:00:00.000Z",
		"2030-01-02T09:00:00Z", "2030-01-02 09:00:00.000", "2030-13-02T09:00:00.000Z", "tomorrow", "",
	} {
		_, err := w.db.Exec(`INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
			VALUES ('k', ?, ?, 'x', ?, ?, ?)`, w.alice, w.ws, bad, at(start), at(start))
		if err == nil {
			t.Errorf("run_at %q was stored", bad)
		}
	}
	id := exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		VALUES ('k', ?, ?, 'ok', ?, ?, ?)`, w.alice, w.ws, at(start), at(start), at(start))
	if _, err := w.db.Exec(`UPDATE scheduled_jobs SET retry_at = '10000-01-01T00:00:00.000Z' WHERE id = ?`, id); err == nil {
		t.Error("retry_at accepted year 10000")
	}
	if _, err := w.db.Exec(`UPDATE scheduled_jobs SET lease_until = '10000-01-01T00:00:00.000Z' WHERE id = ?`, id); err == nil {
		t.Error("lease_until accepted year 10000")
	}
}

func TestARowWithAnUnreadableTimeDoesNotStallTheQueue(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	// Both sort before today's jobs, so the claim query picks them first.
	bad := rawInsert(t, w, "year-10000", "10000-01-01T00:00:00.000Z")
	garbled := rawInsert(t, w, "garbled", "0001-01-01 nonsense")
	w.schedule(w.job("good", 0))
	w.run()
	if h.count() != 1 || h.calls[0].DedupeKey != "good" {
		t.Fatalf("calls = %+v, want the good job once", h.calls)
	}
	for _, id := range []int64{bad, garbled} {
		if st, msg := state(t, w, id); st != StateFailed || msg == "" {
			t.Errorf("malformed job %d is %q (%q), want failed with a reason", id, st, msg)
		}
	}
	if j := w.find(w.alice, "good"); j.State != StateDone {
		t.Fatalf("good job = %+v", j)
	}
}

func TestAnExpiredClaimCantFinishOrAdvanceTheJob(t *testing.T) {
	ctx := context.Background()
	for name, outcome := range map[string]func(s *Service, j Job) error{
		"success":   func(s *Service, j Job) error { return s.succeed(ctx, j) },
		"failure":   func(s *Service, j Job) error { return s.fail(ctx, j, errors.New("late")) },
		"permanent": func(s *Service, j Job) error { return s.fail(ctx, j, Permanent(errors.New("late"))) },
		"access":    func(s *Service, j Job) error { return s.fail(ctx, j, ErrAccessLost) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			h := w.register(remind)
			w.schedule(w.job("k", 0))
			j, ok, err := w.s.claim(ctx)
			if err != nil || !ok {
				t.Fatalf("claim = %v, %v", ok, err)
			}
			// Nobody else has claimed it: the token still matches, but the lease has ended.
			w.clock.advance(defaultLease)
			if err := outcome(w.s, j); err != nil {
				t.Fatal(err)
			}
			got := w.find(w.alice, "k")
			if got.State != StateRunning || got.LastError != "" {
				t.Fatalf("an expired claim changed the job: %+v", got)
			}
			// The recovery of expired leases is what takes it from there, and counts the try.
			w.run()
			if h.count() != 1 || h.calls[0].Attempts != 2 {
				t.Fatalf("calls = %d, attempts %v; want the job taken again, as the second try", h.count(), h.calls)
			}
			if got := w.find(w.alice, "k"); got.State != StateDone {
				t.Fatalf("job = %+v", got)
			}
		})
	}
}

func TestAnExpiredClaimCantAdvanceARecurringJob(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	j := w.job("k", 0)
	j.Every = time.Hour
	w.schedule(j)
	claimed, _, _ := w.s.claim(context.Background())
	w.clock.advance(defaultLease)
	if err := w.s.succeed(context.Background(), claimed); err != nil {
		t.Fatal(err)
	}
	if got := w.find(w.alice, "k"); got.State != StateRunning || !got.RunAt.Equal(start) {
		t.Fatalf("an expired claim advanced the recurring job: %+v", got)
	}
}

func TestRescheduleAtTheSameTimeIsANewOccurrence(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	j := w.job("k", time.Minute)
	w.schedule(j)
	first := w.find(w.alice, "k")
	if first.Revision != 0 {
		t.Fatalf("revision of a new job = %d", first.Revision)
	}
	w.clock.advance(time.Minute)
	w.run()

	j.Payload = []byte(`{"card":8}`)
	if created, err := w.s.Reschedule(context.Background(), w.db, j); err != nil || created {
		t.Fatalf("reschedule = %v, %v", created, err)
	}
	second := w.find(w.alice, "k")
	if !second.RunAt.Equal(first.RunAt) || second.Revision != 1 {
		t.Fatalf("job = %+v, want the same time and revision 1", second)
	}
	if second.Occurrence() == first.Occurrence() {
		t.Fatalf("the rescheduled occurrence has the key of the old one: %q", second.Occurrence())
	}
	w.run()
	if h.count() != 2 || h.calls[1].Occurrence() != second.Occurrence() || h.calls[0].Occurrence() != first.Occurrence() {
		t.Fatalf("occurrences seen: %q, %q", h.calls[0].Occurrence(), h.calls[1].Occurrence())
	}
}

func TestTheRevisionIsStableAcrossRetriesAndAdvancesOnlyOnReschedule(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(n int, _ Job) error {
		if n < 3 {
			return errors.New("later")
		}
		return nil
	}
	j := w.job("k", 0)
	w.schedule(j)
	if _, err := w.s.Reschedule(context.Background(), w.db, j); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w.run()
		w.clock.advance(backoffMax)
	}
	if h.count() != 3 {
		t.Fatalf("calls = %d", h.count())
	}
	for _, c := range h.calls {
		if c.Revision != 1 || c.Occurrence() != h.calls[0].Occurrence() {
			t.Fatalf("a retry saw revision %d, key %q; want revision 1 and one key", c.Revision, c.Occurrence())
		}
	}
}

func TestRearmingAFinishedJobCountsAgainstTheBound(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	ctx := context.Background()
	exec(t, w.db, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		SELECT ?, ?, ?, 'k' || i, ?, ?, ? FROM n`,
		MaxPerUser-1, string(remind), w.alice, w.ws, at(start.Add(time.Hour)), at(start), at(start))
	// A finished job and a failed one: not counted while they are.
	for _, st := range []string{StateDone, StateFailed} {
		exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, string(remind), w.alice, w.ws, st, at(start), st, at(start), at(start))
	}
	// One more takes the owner to the bound.
	w.schedule(w.job("last", time.Hour))
	for _, key := range []string{StateDone, StateFailed} {
		if _, err := w.s.Reschedule(ctx, w.db, w.job(key, time.Hour)); !errors.Is(err, ErrTooMany) {
			t.Fatalf("re-arming the %s job at the bound = %v, want ErrTooMany", key, err)
		}
		if st, _ := state(t, w, w.find(w.alice, key).ID); st != key {
			t.Fatalf("the refused re-arm changed the job to %q", st)
		}
	}
	// Room again: it goes through, and counts.
	exec(t, w.db, `DELETE FROM scheduled_jobs WHERE dedupe_key = 'k1'`)
	if created, err := w.s.Reschedule(ctx, w.db, w.job(StateDone, time.Hour)); err != nil || created {
		t.Fatalf("re-arm with room = %v, %v", created, err)
	}
	if _, err := w.s.Reschedule(ctx, w.db, w.job(StateFailed, time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("the re-arm that filled the bound didn't count: %v", err)
	}
	// A job that is active already can always be changed at the bound.
	if _, err := w.s.Reschedule(ctx, w.db, w.job("last", 2*time.Hour)); err != nil {
		t.Fatalf("changing an active job at the bound = %v", err)
	}
}

// concurrently runs n calls at once, released together, and returns how many of each outcome.
func concurrently(n int, f func(i int) error) (ok, tooMany int, other []error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := f(i)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooMany):
				tooMany++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return ok, tooMany, other
}

func TestTheBoundHoldsForConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	fill := func(w *world, n int) {
		exec(t, w.db, `
			WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
			INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
			SELECT ?, ?, ?, 'k' || i, ?, ?, ? FROM n`,
			n, string(remind), w.alice, w.ws, at(start.Add(time.Hour)), at(start), at(start))
	}
	active := func(w *world) (n int) {
		if err := w.db.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE user_id = ? AND state IN ('pending', 'running')`, w.alice).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	handles := map[string]func(w *world, f func(q DBTX) error) error{
		"the database": func(w *world, f func(q DBTX) error) error { return f(w.db) },
		"a transaction": func(w *world, f func(q DBTX) error) error {
			tx, err := w.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if err := f(tx); err != nil {
				_ = tx.Rollback()
				return err
			}
			return tx.Commit()
		},
	}
	for name, with := range handles {
		t.Run("schedule through "+name, func(t *testing.T) {
			w := newWorld(t)
			w.register(remind)
			fill(w, MaxPerUser-1)
			ok, tooMany, other := concurrently(24, func(i int) error {
				return with(w, func(q DBTX) error {
					_, err := w.s.Schedule(ctx, q, w.job(fmt.Sprintf("new%d", i), time.Hour))
					return err
				})
			})
			if ok != 1 || tooMany != 23 || len(other) != 0 || active(w) != MaxPerUser {
				t.Fatalf("created %d, refused %d, other %v, active %d; want 1, 23, none, %d", ok, tooMany, other, active(w), MaxPerUser)
			}
		})
		t.Run("reschedule through "+name, func(t *testing.T) {
			w := newWorld(t)
			w.register(remind)
			fill(w, MaxPerUser-3)
			for i := range 8 {
				exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, state, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, 'done', ?, ?)`, string(remind), w.alice, w.ws, fmt.Sprintf("old%d", i), at(start), at(start), at(start))
			}
			ok, tooMany, other := concurrently(8, func(i int) error {
				return with(w, func(q DBTX) error {
					_, err := w.s.Reschedule(ctx, q, w.job(fmt.Sprintf("old%d", i), time.Hour))
					return err
				})
			})
			if ok != 3 || tooMany != 5 || len(other) != 0 || active(w) != MaxPerUser {
				t.Fatalf("re-armed %d, refused %d, other %v, active %d; want 3, 5, none, %d", ok, tooMany, other, active(w), MaxPerUser)
			}
		})
	}
}

func TestNextOccurrenceNeverOverflowsOrLeavesTheRange(t *testing.T) {
	anchor := func(y int) time.Time { return time.Date(y, 3, 4, 5, 6, 7, 8_000_000, time.UTC) }
	// The case from the review: an anchor in 1700 and a one-minute interval used to land in 1407.
	if got, ok := nextOccurrence(anchor(1700), time.Minute, start); ok {
		t.Errorf("an anchor in 1700 has a next occurrence, %v", got)
	}
	for _, tc := range []struct {
		name  string
		prev  time.Time
		every time.Duration
		now   time.Time
		none  bool // there is no occurrence inside the range
	}{
		{"a minute from 2000", MinRunAt, time.Minute, start, false},
		{"an hour, now far ahead", anchor(2001), time.Hour, anchor(8000), false},
		{"now before the anchor", anchor(2040), time.Hour, anchor(2001), false},
		{"the longest interval", anchor(2001), time.Duration(1<<63 - 1), start, false},
		{"the longest interval, late", anchor(8900), time.Duration(1<<63 - 1), start, true},
		{"now beyond the range", anchor(2001), time.Minute, time.Date(292277026, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"now long before", anchor(2001), time.Minute, time.Date(-292277022, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"at the end of the range", MaxRunAt, time.Minute, start, true},
		{"one step from the end", MaxRunAt.Add(-time.Minute), time.Minute, MaxRunAt.Add(-time.Minute), false},
		{"just short of the end", MaxRunAt.Add(-59 * time.Second), time.Minute, start, true},
	} {
		got, ok := nextOccurrence(tc.prev, tc.every, tc.now)
		if ok == tc.none {
			t.Errorf("%s: next = %v, %v; want none = %v", tc.name, got, ok, tc.none)
		}
		if !ok {
			continue
		}
		if got.Before(MinRunAt) || got.After(MaxRunAt) {
			t.Errorf("%s: next %v is outside the range", tc.name, got)
		}
		if !got.After(tc.now) || !got.After(tc.prev) {
			t.Errorf("%s: next %v is not after now %v and prev %v", tc.name, got, tc.now, tc.prev)
		}
		// On the grid, in whole milliseconds (a Duration can't hold the span of 6,000 years).
		if d := got.UnixMilli() - tc.prev.UnixMilli(); d%tc.every.Milliseconds() != 0 {
			t.Errorf("%s: next %v is off the grid (%d ms from prev)", tc.name, got, d)
		}
		if prevTick := got.Add(-tc.every); prevTick.After(tc.now) && prevTick.After(tc.prev) {
			t.Errorf("%s: %v is not the first tick after now", tc.name, got)
		}
	}
	if _, ok := nextOccurrence(MaxRunAt.Add(-30*time.Second), time.Minute, start); ok {
		t.Error("a next occurrence past MaxRunAt was returned")
	}
}

func TestARecurringJobWithNoLaterOccurrenceIsDone(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	j := w.job("k", 0)
	// Ten minutes before the end, every ten minutes: the next one is past it.
	j.RunAt, j.Every = time.Date(8999, 12, 31, 23, 50, 0, 0, time.UTC), 10*time.Minute
	w.schedule(j)
	w.clock.t = j.RunAt
	w.run()
	if h.count() != 1 {
		t.Fatalf("calls = %d", h.count())
	}
	if got := w.find(w.alice, "k"); got.State != StateDone {
		t.Fatalf("job = %+v, want done: the next occurrence would pass the last supported time", got)
	}
}

func TestARecurringJobFarBehindStaysOnItsGrid(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	j := w.job("k", 0)
	j.RunAt, j.Every = MinRunAt, time.Minute
	w.schedule(j)
	w.run()
	if h.count() != 1 {
		t.Fatalf("calls = %d", h.count())
	}
	got := w.find(w.alice, "k")
	if !got.RunAt.After(start) || got.RunAt.Sub(start) > time.Minute || got.RunAt.Sub(MinRunAt)%time.Minute != 0 {
		t.Fatalf("next run %v: want the first minute of the grid after %v", got.RunAt, start)
	}
}

// stepClock is a clock whose reads come from a script, so a test can step it backwards between two
// reads of one operation.
type stepClock struct {
	mu    sync.Mutex
	reads []time.Time // the first reads
	then  time.Time   // every read after them
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reads) > 0 {
		t := c.reads[0]
		c.reads = c.reads[1:]
		return t
	}
	return c.then
}

func TestABackwardClockStepDoesNotDispatchTheNextOccurrenceEarly(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	// A recurring job whose worker died on its last try, and another that is due in five minutes.
	a := w.job("a", 0)
	a.Every = time.Hour
	w.schedule(a)
	w.schedule(w.job("b", 5*time.Minute))
	exec(t, w.db, `UPDATE scheduled_jobs SET state = 'running', attempts = ?, lease_until = ? WHERE dedupe_key = 'a'`,
		defaultMaxAttempts, at(start.Add(time.Minute)))

	// The worker reads a late time (the recovery of a is due), then the clock steps back to start.
	late := start.Add(10 * time.Minute)
	c := &stepClock{reads: []time.Time{late, late}, then: start} // the look; the first round; then back
	w.s.clock = c.now
	w.run()
	if h.count() != 0 {
		t.Fatalf("calls = %v: b, due in five minutes, ran after the clock stepped back to now", h.calls)
	}
	if got := w.find(w.alice, "b"); got.State != StatePending {
		t.Fatalf("b = %+v", got)
	}
	if got := w.find(w.alice, "a"); got.State != StatePending || got.Attempts != 0 {
		t.Fatalf("a = %+v, want recovered", got)
	}
}

func TestAHandlerThatReturnsNilAfterItsDeadlineHasFailedTheTry(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Timeout, o.Lease = 20*time.Millisecond, time.Minute })
	h := w.register(remind)
	h.answer = func(int, Job) error { return nil }
	var cooperative atomic.Int32
	w.s.handlers[remind] = func(ctx context.Context, _ Job) error {
		cooperative.Add(1)
		<-ctx.Done() // cut short, and says nothing about it
		return nil
	}
	w.schedule(w.job("k", 0))
	w.run()
	got := w.find(w.alice, "k")
	if cooperative.Load() != 1 || got.State != StatePending || got.LastError == "" || got.Attempts != 1 {
		t.Fatalf("job = %+v after %d calls; want a failed try waiting to be retried", got, cooperative.Load())
	}
	if !strings.Contains(got.LastError, "time limit") {
		t.Fatalf("last error = %q", got.LastError)
	}
}

func TestAHandlerThatBeatsItsDeadlineSucceeds(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Timeout = time.Minute })
	w.register(remind)
	w.schedule(w.job("k", 0))
	w.run()
	if got := w.find(w.alice, "k"); got.State != StateDone {
		t.Fatalf("job = %+v", got)
	}
}

func TestStoppingDoesNotWaitForeverForAHandlerThatIgnoresItsContext(t *testing.T) {
	sqlDB := newDB(t)
	exec(t, sqlDB, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'a', 'x')`)
	exec(t, sqlDB, `INSERT INTO workspaces (name) VALUES ('Home')`)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'member')`)
	s := New(sqlDB, access.New(sqlDB, nil), Options{Poll: 10 * time.Millisecond, StopWait: 50 * time.Millisecond})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := s.Register(remind, func(context.Context, Job) error {
		once.Do(func() { close(started) })
		<-release // deaf to the context
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(context.Background(), sqlDB, Job{Kind: remind, UserID: 1, WorkspaceID: 1, DedupeKey: "k", RunAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stop := s.Start(context.Background())
	<-started
	returned := make(chan struct{})
	go func() { stop(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping waited for a handler that ignores its context")
	}
	j, err := s.Find(context.Background(), sqlDB, 1, remind, "k")
	if err != nil || j.State != StateRunning {
		t.Fatalf("job = %+v, %v; want it left running for its lease to end", j, err)
	}
	close(release)
	time.Sleep(50 * time.Millisecond) // the worker goroutine unwinds without noting an outcome
	if j, _ := s.Find(context.Background(), sqlDB, 1, remind, "k"); j.State != StateRunning {
		t.Fatalf("a handler that returned after the stop settled the job: %+v", j)
	}
}

func TestACleanupGetsATurnWhileTheQueueIsAlwaysDue(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	const jobs = 60
	w.schedule(w.job("old", 0))
	exec(t, w.db, `UPDATE scheduled_jobs SET state = 'done', updated_at = ? WHERE dedupe_key = 'old'`, at(start.Add(-2*keepFinished)))
	for i := range jobs {
		w.schedule(w.job(fmt.Sprintf("due%d", i), 0))
	}
	var ran atomic.Int32
	var purgedBeforeTheEnd atomic.Bool
	w.s.handlers[remind] = func(context.Context, Job) error {
		time.Sleep(5 * time.Millisecond) // the queue stays busy well past a tidy tick
		if ran.Load() == jobs-1 {        // the last one: noted before it is counted, which is what the test waits for
			_, err := w.s.Find(context.Background(), w.db, w.alice, remind, "old")
			purgedBeforeTheEnd.Store(errors.Is(err, ErrNotFound))
		}
		ran.Add(1)
		return nil
	}
	w.s.batch, w.s.tidyEvery, w.s.poll = 3, 10*time.Millisecond, time.Hour
	stop := w.s.Start(context.Background())
	defer stop()
	deadline := time.Now().Add(20 * time.Second)
	for ran.Load() < jobs && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ran.Load() != jobs {
		t.Fatalf("ran %d of %d jobs", ran.Load(), jobs)
	}
	if !purgedBeforeTheEnd.Load() {
		t.Fatal("the purge waited for the queue to drain")
	}
}

func TestTheWorkersQueriesUseTheirIndexes(t *testing.T) {
	w := newWorld(t)
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := w.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "\n")
	}
	check := func(what, got string, want ...string) {
		t.Helper()
		t.Logf("%s:\n%s", what, got)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: the plan doesn't use %s:\n%s", what, w, got)
			}
		}
		if strings.Contains(got, "TEMP B-TREE") || strings.Contains(got, "SCAN scheduled_jobs\n") || strings.HasSuffix(got, "SCAN scheduled_jobs") {
			t.Errorf("%s: the plan sorts or scans:\n%s", what, got)
		}
	}
	args := []any{at(start), "a.b", "c.d"}
	queries := dueQueries(2)
	check("the pending queue", plan(queries[0], args...), "scheduled_jobs_pending_due")
	check("the lease recovery", plan(queries[1], args...), "scheduled_jobs_lease_ended")
	check("the purge", plan(purgeQuery, at(start), purgeChunk), "scheduled_jobs_finished")
	check("the bound", plan(`SELECT count(*) FROM scheduled_jobs WHERE user_id = ? AND state IN ('pending', 'running')`, 1))
}

func TestTheBackoffStaysAtItsCeilingForManyTries(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.MaxAttempts = 40 })
	h := w.register(remind)
	h.answer = func(int, Job) error { return errors.New("down") }
	w.schedule(w.job("k", 0))
	for try := 1; try < 40; try++ {
		w.run()
		var retryAt string
		if err := w.db.QueryRow(`SELECT retry_at FROM scheduled_jobs WHERE dedupe_key = 'k'`).Scan(&retryAt); err != nil {
			t.Fatal(err)
		}
		wait := min(backoffBase<<min(try-1, 10), backoffMax)
		if want := at(w.clock.now().Add(wait)); retryAt != want {
			t.Fatalf("after try %d the next is at %s, want %s (a wait of %v)", try, retryAt, want, wait)
		}
		w.clock.advance(wait)
	}
	w.run()
	if got := w.find(w.alice, "k"); got.State != StateFailed || h.count() != 40 {
		t.Fatalf("job = %+v after %d calls, want failed after 40", got, h.count())
	}
}

// A write whose context ends inside its transaction is rolled back before write returns, so no
// connection is left busy for the next writer or for closing the database.
func TestWriteCancelledInsideItsTransactionLeavesNoConnectionBusy(t *testing.T) {
	w := newWorld(t)
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		err := w.s.write(ctx, func(tx *sql.Tx) error {
			cancel()
			_, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name) VALUES ('Cancelled')`)
			return err
		})
		if err == nil {
			t.Fatal("a write cancelled inside its transaction succeeded")
		}
		if inUse := w.db.Stats().InUse; inUse != 0 {
			t.Fatalf("%d connections still busy after write returned", inUse)
		}
	}
}
