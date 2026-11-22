package backup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/lockfile"
)

// fakeClock is a clock tests move by hand; sleeping moves it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.t = c.t.Add(d)
	return nil
}

// fakeRunner fails its checks or its backups while told to, and records what it ran,
// when. A backup writes into the data directory, as `taskto backup` does, and leaves it
// there when it fails, as a killed one does.
type fakeRunner struct {
	dir       string
	clock     *fakeClock
	unreached bool // the repository doesn't answer
	failing   int  // backups still to fail
	checks    []time.Time
	backups   []time.Time
	onBackup  func()
}

func (r *fakeRunner) Check(ctx context.Context) error {
	r.checks = append(r.checks, r.clock.now())
	if r.unreached {
		return errors.New("Fatal: unable to open config file: dial tcp: connection refused")
	}
	return nil
}

func (r *fakeRunner) Backup(ctx context.Context) (string, error) {
	r.backups = append(r.backups, r.clock.now())
	if r.onBackup != nil {
		r.onBackup()
	}
	staging, err := os.MkdirTemp(r.dir, tmpPrefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "snapshot.db"), []byte("x"), 0o600); err != nil {
		return "", err
	}
	// As Create does; the lock goes with the process.
	lock, err := lockfile.Lock(filepath.Join(staging, lockName))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	r.clock.t = r.clock.t.Add(5 * time.Minute)
	if r.failing > 0 {
		r.failing--
		// Killed: nothing removes the staging directory, and nothing holds its lock.
		return "", errors.New("restic backup: Fatal: unable to save snapshot (exit status 1)")
	}
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	return "0123abcd", nil
}

var quiet = slog.New(slog.DiscardHandler)

func testSchedule() Schedule {
	return Schedule{At: 3*time.Hour + 15*time.Minute, Retries: 6, RetryEvery: time.Hour, Timeout: time.Hour, CheckTimeout: time.Minute}
}

func newTestScheduler(t *testing.T, start time.Time) (*Scheduler, *fakeClock, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	clock := &fakeClock{t: start}
	runner := &fakeRunner{dir: dir, clock: clock}
	return &Scheduler{DataDir: dir, Schedule: testSchedule(), Runner: runner, Log: quiet, now: clock.now, sleep: clock.sleep}, clock, runner
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func readStatus(t *testing.T, dir string) Status {
	t.Helper()
	st, ok, err := ReadStatus(dir)
	if err != nil || !ok {
		t.Fatalf("ReadStatus = %v, %v", ok, err)
	}
	return st
}

func staging(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestParseAt(t *testing.T) {
	for in, want := range map[string]time.Duration{"03:15": 3*time.Hour + 15*time.Minute, "00:00": 0, "23:59": 23*time.Hour + 59*time.Minute} {
		if got, err := ParseAt(in); err != nil || got != want {
			t.Errorf("ParseAt(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "3", "24:00", "03:60", "3pm", "03:15:00"} {
		if _, err := ParseAt(bad); err == nil {
			t.Errorf("ParseAt(%q) succeeded", bad)
		}
	}
}

func TestScheduleValidate(t *testing.T) {
	if err := testSchedule().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Schedule){
		"negative retries":          func(s *Schedule) { s.Retries = -1 },
		"retries a second apart":    func(s *Schedule) { s.RetryEvery = time.Second },
		"retries into next night":   func(s *Schedule) { s.Retries = 24 },
		"no timeout":                func(s *Schedule) { s.Timeout = 0 },
		"a day's time out of a day": func(s *Schedule) { s.At = 25 * time.Hour },
	} {
		s := testSchedule()
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s: valid", name)
		}
	}
	none := testSchedule()
	none.Retries, none.RetryEvery = 0, 0
	if err := none.Validate(); err != nil {
		t.Errorf("no retries: %v", err)
	}
}

func TestScheduleNext(t *testing.T) {
	s := testSchedule()
	night := at("2026-03-10T03:15:00Z")
	for _, c := range []struct {
		name string
		st   Status
		now  time.Time
		want time.Time
	}{
		{"never backed up: at once", Status{}, at("2026-03-10T12:00:00Z"), at("2026-03-10T12:00:00Z")},
		{"before tonight, last night done: tonight", Status{LastSuccess: at("2026-03-09T03:20:00Z")}, at("2026-03-10T01:00:00Z"), night},
		{"tonight done: tomorrow", Status{LastSuccess: at("2026-03-10T03:20:00Z")}, at("2026-03-10T03:30:00Z"), night.AddDate(0, 0, 1)},
		{"tonight not done: at once", Status{LastSuccess: at("2026-03-09T03:20:00Z")}, at("2026-03-10T09:00:00Z"), at("2026-03-10T09:00:00Z")},
		{"failed once: an hour after", Status{LastSuccess: at("2026-03-09T03:20:00Z"), Night: night, Failures: 1, LastErrorAt: at("2026-03-10T03:16:00Z")}, at("2026-03-10T03:16:00Z"), at("2026-03-10T04:16:00Z")},
		{"a retry overdue: at once", Status{Night: night, Failures: 3, LastErrorAt: at("2026-03-10T05:00:00Z")}, at("2026-03-10T08:00:00Z"), at("2026-03-10T08:00:00Z")},
		{"retries used up: tomorrow", Status{Night: night, Failures: 7, LastErrorAt: at("2026-03-10T09:16:00Z")}, at("2026-03-10T09:16:00Z"), night.AddDate(0, 0, 1)},
		{"failures without an error's time: an hour after the last attempt", Status{Night: night, Failures: 2, LastAttempt: at("2026-03-10T05:00:00Z")}, at("2026-03-10T05:10:00Z"), at("2026-03-10T06:00:00Z")},
		{"last night's failures don't count tonight", Status{Night: night.AddDate(0, 0, -1), Failures: 7, LastErrorAt: at("2026-03-09T09:16:00Z")}, at("2026-03-10T03:15:00Z"), at("2026-03-10T03:15:00Z")},
	} {
		if got := s.next(c.st, c.now); !got.Equal(c.want) {
			t.Errorf("%s: next = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHealth(t *testing.T) {
	now := at("2026-03-10T12:00:00Z")
	for _, c := range []struct {
		name string
		st   Status
		want Health
	}{
		{"started, nothing yet", Status{DueBy: now.Add(time.Hour)}, HealthPending},
		{"succeeded", Status{LastSuccess: now.Add(-time.Hour), DueBy: now.Add(20 * time.Hour)}, HealthOK},
		{"an old error, then a success", Status{LastSuccess: now.Add(-time.Hour), LastErrorAt: now.Add(-2 * time.Hour), DueBy: now.Add(20 * time.Hour)}, HealthOK},
		{"failing, retries left", Status{LastSuccess: now.Add(-10 * time.Hour), LastErrorAt: now.Add(-time.Hour), DueBy: now.Add(time.Hour)}, HealthFailing},
		{"past due", Status{LastSuccess: now.Add(-40 * time.Hour), DueBy: now.Add(-time.Minute)}, HealthLate},
	} {
		if got := c.st.Health(now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A failed night is retried every RetryEvery, Retries times, then left for the next
// night, which starts afresh. The status counts the failures, so a restart neither
// forgets them nor tries more often; a success clears them and says when the next is due.
func TestRetriesAndTheMarkerOfTheLastSuccess(t *testing.T) {
	s, clock, runner := newTestScheduler(t, at("2026-03-10T03:15:00Z"))
	runner.failing = 100
	night := at("2026-03-10T03:15:00Z")
	for i := 1; i <= 7; i++ {
		if want := s.Schedule.next(s.read(), clock.now()); !want.Equal(clock.now()) {
			t.Fatalf("attempt %d: next = %v, want now (%v)", i, want, clock.now())
		}
		st, err := s.Attempt(context.Background())
		if err == nil {
			t.Fatalf("attempt %d succeeded", i)
		}
		if st.Failures != i || !st.Night.Equal(night) || st.LastError == "" {
			t.Fatalf("attempt %d: status %+v", i, st)
		}
		// A restart: a new scheduler reads the same marker.
		s = &Scheduler{DataDir: s.DataDir, Schedule: s.Schedule, Runner: runner, Log: quiet, now: clock.now, sleep: clock.sleep}
		next := s.Schedule.next(s.read(), clock.now())
		if i < 7 {
			if want := st.LastErrorAt.Add(time.Hour); !next.Equal(want) || !st.NextAttempt.Equal(want) {
				t.Fatalf("after failure %d: next = %v (status %v), want %v", i, next, st.NextAttempt, want)
			}
		} else if want := night.AddDate(0, 0, 1); !next.Equal(want) {
			t.Fatalf("after the last retry: next = %v, want the next night %v", next, want)
		}
		clock.t = next
	}
	if len(runner.backups) != 7 {
		t.Fatalf("%d backups for one night, want 1 and 6 retries", len(runner.backups))
	}

	// The next night succeeds: the failures are cleared, and the one after is due.
	runner.failing = 0
	st, err := s.Attempt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Failures != 0 || st.LastSnapshot != "0123abcd" || !st.LastSuccess.Equal(clock.now()) {
		t.Fatalf("after a success: %+v", st)
	}
	if want := night.AddDate(0, 0, 2); !st.NextAttempt.Equal(want) {
		t.Fatalf("next after a success = %v, want %v", st.NextAttempt, want)
	}
	if want := night.AddDate(0, 0, 2).Add(7 * time.Hour); !st.DueBy.Equal(want) {
		t.Fatalf("due by = %v, want the night after plus its retries and an hour, %v", st.DueBy, want)
	}
	if got := readStatus(t, s.DataDir); got.LastSnapshot != "0123abcd" || got.Health(clock.now()) != HealthOK {
		t.Fatalf("written status: %+v", got)
	}
	// What the failed backups left was removed after each of them.
	if left := staging(t, s.DataDir); len(left) != 0 {
		t.Fatalf("left in the data directory: %v", left)
	}
}

// When the repository doesn't answer, the backup isn't started at all, so nothing is
// written in the data directory, and the error says why.
func TestAnUnreachableRepositoryStopsEarly(t *testing.T) {
	s, clock, runner := newTestScheduler(t, at("2026-03-10T03:15:00Z"))
	runner.unreached = true
	st, err := s.Attempt(context.Background())
	if err == nil {
		t.Fatal("the attempt succeeded")
	}
	if len(runner.checks) != 1 || len(runner.backups) != 0 {
		t.Fatalf("checks %d, backups %d: want the check only", len(runner.checks), len(runner.backups))
	}
	if !strings.Contains(st.LastError, "didn't answer") || !strings.Contains(st.LastError, "connection refused") {
		t.Fatalf("error = %q", st.LastError)
	}
	if left := staging(t, s.DataDir); len(left) != 0 {
		t.Fatalf("left in the data directory: %v", left)
	}
	if got := readStatus(t, s.DataDir); got.Health(clock.now()) != HealthFailing || got.Failures != 1 {
		t.Fatalf("status = %+v", got)
	}
	// Once the night's time is up, the night is late.
	clock.t = clock.t.Add(8 * time.Hour)
	if got := readStatus(t, s.DataDir); got.Health(clock.now()) != HealthLate {
		t.Fatalf("health 8 hours on = %s, want late", got.Health(clock.now()))
	}
}

// What a killed backup left is removed, unless a running backup holds it.
func TestCleanStale(t *testing.T) {
	dir := t.TempDir()
	mk := func() string {
		p, err := os.MkdirTemp(dir, tmpPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "snapshot.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	killed := mk()
	if f, err := lockfile.Lock(filepath.Join(killed, lockName)); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close() // its process is gone
	}
	running := mk()
	held, err := lockfile.Lock(filepath.Join(running, lockName))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	fresh := mk() // just made, not locked yet
	old := mk()   // from a version without the lock file, long ago
	long := time.Now().Add(-2 * staleAge)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}

	if err := CleanStale(dir); err != nil {
		t.Fatal(err)
	}
	left := staging(t, dir)
	want := []string{filepath.Base(running), filepath.Base(fresh)}
	slices.Sort(left)
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

// A backup that started and never recorded its end (the schedule was killed) counts as a
// failure, so a backup that keeps killing the schedule doesn't run at every restart.
func TestAnUnfinishedBackupCountsAsFailed(t *testing.T) {
	s, clock, runner := newTestScheduler(t, at("2026-03-10T03:15:00Z"))
	runner.onBackup = func() { panic("killed") }
	func() {
		defer func() { _ = recover() }()
		_, _ = s.Attempt(context.Background())
	}()
	st := s.read()
	if st.Failures != 1 || !st.LastErrorAt.Equal(at("2026-03-10T03:15:00Z")) || st.LastError == "" {
		t.Fatalf("status after a killed backup: %+v", st)
	}
	if next := s.Schedule.next(st, clock.now()); !next.Equal(at("2026-03-10T04:15:00Z")) {
		t.Fatalf("next = %v, want an hour after the killed one", next)
	}
}

// Run sleeps until the night, backs up, and on failure retries on time.
func TestRun(t *testing.T) {
	s, clock, runner := newTestScheduler(t, at("2026-03-10T03:15:00Z"))
	if err := writeStatus(s.DataDir, Status{LastSuccess: at("2026-03-10T03:20:00Z")}); err != nil {
		t.Fatal(err)
	}
	clock.t = at("2026-03-10T12:00:00Z")
	runner.failing = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.onBackup = func() {
		if len(runner.backups) == 3 {
			cancel() // the third is the last the test waits for
		}
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []time.Time{at("2026-03-11T03:15:00Z"), at("2026-03-11T04:20:00Z"), at("2026-03-11T05:25:00Z")}
	if !slices.EqualFunc(runner.backups, want, time.Time.Equal) {
		t.Fatalf("backups at %v, want %v", runner.backups, want)
	}
	st := readStatus(t, s.DataDir)
	if st.At != "03:15" || st.Retries != 6 || st.RetryEverySeconds != 3600 {
		t.Fatalf("settings in the status: %+v", st)
	}
}

// Two backups of one data directory never run at once.
func TestOneAttemptAtATime(t *testing.T) {
	s, _, runner := newTestScheduler(t, at("2026-03-10T03:15:00Z"))
	runner.onBackup = func() {
		if _, err := s.Attempt(context.Background()); !errors.Is(err, ErrBusy) {
			t.Errorf("a second attempt during the first = %v, want ErrBusy", err)
		}
	}
	if _, err := s.Attempt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRedact(t *testing.T) {
	in := "Fatal: unable to open repository at rest:https://taskto:fake-secret@backup.example.com:8000/taskto/: connection refused"
	got := Redact(in)
	if strings.Contains(got, "fake-secret") || !strings.Contains(got, "https://***@backup.example.com:8000/taskto/") {
		t.Fatalf("Redact = %q", got)
	}
	long := Redact(strings.Repeat("é", maxError))
	if len(long) > maxError+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatalf("a long error is %d bytes", len(long))
	}
}

func TestLastMessage(t *testing.T) {
	for in, want := range map[string]string{
		"reading...\nFatal: unable to open config file: Stat: connection refused\n\n":                     "Fatal: unable to open config file: Stat: connection refused",
		`{"message_type":"exit_error","code":1,"message":"Fatal: wrong password or no key found"}` + "\n": "Fatal: wrong password or no key found",
		`{"message_type":"error","error":{"message":"read: broken pipe"},"during":"archival"}`:            "read: broken pipe",
		"": "",
	} {
		if got := lastMessage(in); got != want {
			t.Errorf("lastMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

// The server's side only adds: the schedule never asks restic to remove anything.
func TestBackupArgsOnlyAdd(t *testing.T) {
	args := BackupArgs("/usr/local/bin/taskto", "/data")
	if args[0] != "backup" || !slices.Contains(args, "--stdin-from-command") || !slices.Contains(args, "-uncompressed") {
		t.Fatalf("args = %v", args)
	}
	for _, a := range args {
		if a == "forget" || a == "prune" || strings.HasPrefix(a, "--prune") {
			t.Fatalf("args = %v", args)
		}
	}
}

// The schedule's status is about this server, not data: a backup leaves it out.
func TestTheStatusIsLeftOut(t *testing.T) {
	f := newFixture(t)
	if err := writeStatus(f.dir, Status{LastSuccess: time.Now()}); err != nil {
		t.Fatal(err)
	}
	archive, _ := backup(t, f.dir)
	for _, n := range entries(t, archive.Bytes()) {
		if strings.HasPrefix(n, StatusDir) {
			t.Errorf("the backup has %s", n)
		}
	}
}
