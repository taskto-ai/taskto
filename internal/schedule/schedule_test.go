package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/db"
)

// clock is a clock the tests move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var start = time.Date(2030, 1, 2, 9, 0, 0, 0, time.UTC)

// world is a workspace with people in every relation to its spaces:
//
//   - alice, bob: members of ws, editors of team (a shared space);
//   - carol: a member of ws, in no shared space;
//   - eve: a member of another workspace only.
type world struct {
	t                          *testing.T
	db                         *sql.DB
	clock                      *clock
	access                     *access.Checker
	s                          *Service
	ws, other                  int64
	alice, bob, carol, eve     int64
	team, alicePrivate, evePrv int64
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func exec(t *testing.T, sqlDB *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := sqlDB.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func newWorld(t *testing.T, opts ...func(*Options)) *world {
	t.Helper()
	sqlDB := newDB(t)
	w := &world{t: t, db: sqlDB, clock: &clock{t: start}}
	w.access = access.New(sqlDB, nil)
	user := func(name string) int64 {
		return exec(t, sqlDB, `INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x')`, name+"@example.com", name)
	}
	w.alice, w.bob, w.carol, w.eve = user("alice"), user("bob"), user("carol"), user("eve")
	w.ws = exec(t, sqlDB, `INSERT INTO workspaces (name) VALUES ('Home')`)
	w.other = exec(t, sqlDB, `INSERT INTO workspaces (name) VALUES ('Elsewhere')`)
	for _, u := range []int64{w.alice, w.bob, w.carol} {
		exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, u)
	}
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.other, w.eve)
	w.team = exec(t, sqlDB, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Team')`, w.ws)
	exec(t, sqlDB, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, w.team, w.alice)
	exec(t, sqlDB, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, w.team, w.bob)
	w.alicePrivate = w.privateSpace(w.ws, w.alice)
	w.evePrv = w.privateSpace(w.other, w.eve)

	o := Options{Clock: w.clock.now}
	for _, f := range opts {
		f(&o)
	}
	w.s = New(sqlDB, w.access, o)
	return w
}

func (w *world) privateSpace(ws, user int64) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.QueryRow(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, ws, user).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// recorder is a handler that notes its calls and answers from a script.
type recorder struct {
	mu    sync.Mutex
	calls []Job
	// answer returns the error of call n (1-based); nil by default.
	answer func(n int, j Job) error
}

func (r *recorder) handle(_ context.Context, j Job) error {
	r.mu.Lock()
	r.calls = append(r.calls, j)
	n := len(r.calls)
	answer := r.answer
	r.mu.Unlock()
	if answer != nil {
		return answer(n, j)
	}
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (w *world) register(kind Kind) *recorder {
	w.t.Helper()
	r := &recorder{}
	if err := w.s.Register(kind, r.handle); err != nil {
		w.t.Fatal(err)
	}
	return r
}

const remind Kind = "test.remind"

// job is a job of alice's in her private space, due after d.
func (w *world) job(key string, after time.Duration) Job {
	return Job{
		Kind: remind, UserID: w.alice, WorkspaceID: w.ws, SpaceID: w.alicePrivate,
		DedupeKey: key, Payload: json.RawMessage(`{"card":7}`), RunAt: w.clock.now().Add(after),
	}
}

func (w *world) schedule(j Job) bool {
	w.t.Helper()
	created, err := w.s.Schedule(context.Background(), w.db, j)
	if err != nil {
		w.t.Fatalf("schedule %q: %v", j.DedupeKey, err)
	}
	return created
}

func (w *world) find(user int64, key string) Job {
	w.t.Helper()
	j, err := w.s.Find(context.Background(), w.db, user, remind, key)
	if err != nil {
		w.t.Fatalf("find %q: %v", key, err)
	}
	return j
}

func (w *world) run() { w.s.RunDue(context.Background()) }

func TestAJobRunsOnceWhenItIsDue(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	if !w.schedule(w.job("card:7", time.Minute)) {
		t.Fatal("a new job was not created")
	}

	w.run()
	if h.count() != 0 {
		t.Fatal("a job ran before it was due")
	}
	w.clock.advance(time.Minute)
	w.run()
	if h.count() != 1 {
		t.Fatalf("calls = %d, want 1", h.count())
	}
	got := h.calls[0]
	if got.UserID != w.alice || got.WorkspaceID != w.ws || got.SpaceID != w.alicePrivate || string(got.Payload) != `{"card":7}` || got.Attempts != 1 {
		t.Fatalf("the handler got %+v", got)
	}
	if j := w.find(w.alice, "card:7"); j.State != StateDone || j.LastError != "" {
		t.Fatalf("job after its run = %+v", j)
	}
	w.clock.advance(24 * time.Hour)
	w.run()
	if h.count() != 1 {
		t.Fatalf("a job that is done ran again: %d calls", h.count())
	}
}

func TestJobsSurviveARestart(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	w.schedule(w.job("card:7", time.Minute))

	// The server stops; a new one starts on the same database, with its own handlers.
	again := New(w.db, w.access, Options{Clock: w.clock.now})
	h := &recorder{}
	if err := again.Register(remind, h.handle); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(time.Hour)
	again.RunDue(context.Background())
	again.RunDue(context.Background())
	if h.count() != 1 {
		t.Fatalf("calls after a restart = %d, want 1", h.count())
	}
}

func TestDedupeKeyMakesSchedulingIdempotent(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	if !w.schedule(w.job("card:7", time.Minute)) {
		t.Fatal("first schedule did not create")
	}
	second := w.job("card:7", time.Hour)
	second.Payload = json.RawMessage(`{"card":8}`)
	if w.schedule(second) {
		t.Fatal("scheduling the same key again created a job")
	}
	var n int
	if err := w.db.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v; want 1", n, err)
	}
	if j := w.find(w.alice, "card:7"); string(j.Payload) != `{"card":7}` || !j.RunAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("scheduling again changed the job: %+v", j)
	}

	// The key is per owner and kind.
	other := w.job("card:7", time.Minute)
	other.UserID, other.SpaceID = w.bob, w.privateSpace(w.ws, w.bob)
	if !w.schedule(other) {
		t.Fatal("another person's job with the same key was refused")
	}
	w.register("test.other")
	third := w.job("card:7", time.Minute)
	third.Kind = "test.other"
	if !w.schedule(third) {
		t.Fatal("another kind's job with the same key was refused")
	}

	// A job that has run still holds its key: the reminder is not made twice.
	w.clock.advance(time.Minute)
	w.run()
	if h.count() != 2 { // alice's and bob's
		t.Fatalf("calls = %d, want 2", h.count())
	}
	if w.schedule(w.job("card:7", time.Minute)) {
		t.Fatal("a job that already ran was scheduled again")
	}
}

func TestRescheduleReplacesTheJob(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	w.schedule(w.job("card:7", time.Minute))

	moved := w.job("card:7", 2*time.Hour)
	moved.Payload = json.RawMessage(`{"card":7,"v":2}`)
	created, err := w.s.Reschedule(context.Background(), w.db, moved)
	if err != nil || created {
		t.Fatalf("reschedule = %v, %v; want an update", created, err)
	}
	w.clock.advance(time.Hour)
	w.run()
	if h.count() != 0 {
		t.Fatal("the old time still ran the job")
	}
	w.clock.advance(time.Hour)
	w.run()
	if h.count() != 1 || string(h.calls[0].Payload) != `{"card":7,"v":2}` {
		t.Fatalf("calls = %+v", h.calls)
	}

	// A job that is done is armed again by a new time.
	if _, err := w.s.Reschedule(context.Background(), w.db, w.job("card:7", time.Minute)); err != nil {
		t.Fatal(err)
	}
	if j := w.find(w.alice, "card:7"); j.State != StatePending || j.Attempts != 0 {
		t.Fatalf("job after a reschedule = %+v", j)
	}
	w.clock.advance(time.Minute)
	w.run()
	if h.count() != 2 {
		t.Fatalf("calls = %d, want 2", h.count())
	}

	// Reschedule adds a job that doesn't exist.
	created, err = w.s.Reschedule(context.Background(), w.db, w.job("card:9", time.Minute))
	if err != nil || !created {
		t.Fatalf("reschedule of a new key = %v, %v", created, err)
	}
}

func TestRescheduleWhileRunningKeepsTheNewSchedule(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	w.schedule(w.job("card:7", 0))

	claimed, ok, err := w.s.claim(context.Background())
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if _, err := w.s.Reschedule(context.Background(), w.db, w.job("card:7", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The worker that was running the old occurrence finishes: it must not mark the new one done.
	if err := w.s.succeed(context.Background(), claimed); err != nil {
		t.Fatal(err)
	}
	j := w.find(w.alice, "card:7")
	if j.State != StatePending || !j.RunAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("job = %+v, want pending at the new time", j)
	}
}

func TestCancel(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	w.schedule(w.job("card:7", time.Minute))

	// Someone else's cancel finds nothing: the key is the owner's.
	if ok, err := w.s.Cancel(context.Background(), w.db, w.bob, remind, "card:7"); err != nil || ok {
		t.Fatalf("another user's cancel = %v, %v", ok, err)
	}
	if ok, err := w.s.Cancel(context.Background(), w.db, w.alice, remind, "card:7"); err != nil || !ok {
		t.Fatalf("cancel = %v, %v", ok, err)
	}
	if ok, err := w.s.Cancel(context.Background(), w.db, w.alice, remind, "card:7"); err != nil || ok {
		t.Fatalf("second cancel = %v, %v", ok, err)
	}
	w.clock.advance(time.Hour)
	w.run()
	if h.count() != 0 {
		t.Fatal("a cancelled job ran")
	}
	if _, err := w.s.Find(context.Background(), w.db, w.alice, remind, "card:7"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find after cancel = %v", err)
	}
	// The key is free again.
	if !w.schedule(w.job("card:7", time.Minute)) {
		t.Fatal("the key of a cancelled job was not free")
	}
}

func TestCancelWhileRunningLeavesNothing(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	w.schedule(w.job("card:7", 0))
	claimed, _, err := w.s.claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Cancel(context.Background(), w.db, w.alice, remind, "card:7"); err != nil {
		t.Fatal(err)
	}
	if err := w.s.succeed(context.Background(), claimed); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.db.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs = %d, %v; want none", n, err)
	}
}

func TestSchedulingIsPartOfTheCallersTransaction(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	ctx := context.Background()

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Schedule(ctx, tx, w.job("rolled-back", time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Find(ctx, w.db, w.alice, remind, "rolled-back"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rolled-back job is there: %v", err)
	}

	tx, err = w.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Schedule(ctx, tx, w.job("committed", time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Seen inside the transaction; cancelling there takes it back before it is ever visible.
	if _, err := w.s.Find(ctx, tx, w.alice, remind, "committed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.find(w.alice, "committed")
}

func TestFailedTriesAreRetriedWithGrowingWaits(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(n int, _ Job) error {
		if n < 3 {
			return fmt.Errorf("the network was down (try %d)", n)
		}
		return nil
	}
	w.schedule(w.job("card:7", 0))

	w.run()
	j := w.find(w.alice, "card:7")
	if h.count() != 1 || j.State != StatePending || j.Attempts != 1 || !strings.Contains(j.LastError, "network") {
		t.Fatalf("after the first try: calls %d, job %+v", h.count(), j)
	}
	w.run() // too soon: the wait is 5s
	if h.count() != 1 {
		t.Fatal("a failed try was repeated at once")
	}
	w.clock.advance(backoffBase)
	w.run()
	if h.count() != 2 {
		t.Fatalf("calls = %d, want 2", h.count())
	}
	w.clock.advance(backoffBase) // the second wait is twice as long
	w.run()
	if h.count() != 2 {
		t.Fatal("the second wait was no longer than the first")
	}
	w.clock.advance(backoffBase)
	w.run()
	j = w.find(w.alice, "card:7")
	if h.count() != 3 || j.State != StateDone || j.LastError != "" {
		t.Fatalf("calls %d, job %+v", h.count(), j)
	}
	// Every try of the occurrence has the same key and the same due time.
	for _, c := range h.calls {
		if c.Occurrence() != h.calls[0].Occurrence() || !c.RunAt.Equal(start) {
			t.Fatalf("occurrence changed between tries: %q vs %q", c.Occurrence(), h.calls[0].Occurrence())
		}
	}
}

func TestTriesAreBounded(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(int, Job) error { return errors.New("always") }
	w.schedule(w.job("card:7", 0))

	for range 20 {
		w.run()
		w.clock.advance(backoffMax)
	}
	j := w.find(w.alice, "card:7")
	if h.count() != defaultMaxAttempts || j.State != StateFailed || j.LastError != "always" {
		t.Fatalf("calls %d, job %+v", h.count(), j)
	}
}

func TestAFailureThatRetryingCantFixIsNotRetried(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(int, Job) error { return Permanent(errors.New("the card is gone")) }
	w.schedule(w.job("card:7", 0))
	w.run()
	w.clock.advance(time.Hour)
	w.run()
	if j := w.find(w.alice, "card:7"); h.count() != 1 || j.State != StateFailed || j.LastError != "the card is gone" {
		t.Fatalf("calls %d, job %+v", h.count(), j)
	}
}

func TestAPanickingHandlerIsATryThatFailed(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(n int, _ Job) error {
		if n == 1 {
			panic("boom")
		}
		return nil
	}
	w.schedule(w.job("card:7", 0))
	w.run()
	if j := w.find(w.alice, "card:7"); j.State != StatePending || !strings.Contains(j.LastError, "boom") {
		t.Fatalf("job = %+v", j)
	}
	w.clock.advance(backoffBase)
	w.run()
	if j := w.find(w.alice, "card:7"); j.State != StateDone {
		t.Fatalf("job = %+v", j)
	}
}

func TestALongErrorIsShortenedAndNothingElseIsKept(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(int, Job) error { return Permanent(errors.New(strings.Repeat("é", 1000))) }
	w.schedule(w.job("card:7", 0))
	w.run()
	if j := w.find(w.alice, "card:7"); len([]rune(j.LastError)) != maxErrorLen {
		t.Fatalf("error length = %d", len([]rune(j.LastError)))
	}
}

func TestALeaseThatRanOutAfterACrashIsTakenAgain(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	w.schedule(w.job("card:7", 0))

	// A worker takes the job and dies before it notes anything.
	dead, ok, err := w.s.claim(context.Background())
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	w.run()
	if h.count() != 0 {
		t.Fatal("a job another worker holds was run before its lease ended")
	}
	w.clock.advance(defaultLease - time.Second)
	w.run()
	if h.count() != 0 {
		t.Fatal("a job was taken a second before its lease ended")
	}
	w.clock.advance(time.Second)
	w.run()
	if h.count() != 1 {
		t.Fatalf("calls = %d after the lease ended, want 1", h.count())
	}
	j := w.find(w.alice, "card:7")
	if j.State != StateDone || h.calls[0].Attempts != 2 {
		t.Fatalf("job %+v, attempts seen %d (the crash counts as a try)", j, h.calls[0].Attempts)
	}

	// The worker that was thought dead wakes up: it owns nothing.
	if err := w.s.fail(context.Background(), dead, errors.New("late")); err != nil {
		t.Fatal(err)
	}
	if err := w.s.succeed(context.Background(), dead); err != nil {
		t.Fatal(err)
	}
	if j := w.find(w.alice, "card:7"); j.State != StateDone || j.LastError != "" {
		t.Fatalf("a worker that lost its lease changed the job: %+v", j)
	}
}

func TestACrashOnTheLastTryIsTheEnd(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	w.schedule(w.job("card:7", 0))
	for range defaultMaxAttempts {
		if _, ok, err := w.s.claim(context.Background()); err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		w.clock.advance(defaultLease)
	}
	w.run()
	if h.count() != 0 {
		t.Fatal("a job whose every try crashed was run again")
	}
	if j := w.find(w.alice, "card:7"); j.State != StateFailed || j.LastError == "" {
		t.Fatalf("job = %+v", j)
	}
}

func TestAJobWhoseOwnerLostAccessDoesNotRun(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	// The schema drops the jobs of a person who leaves (TestAJobGoesWithItsOwnersAccess); this is the
	// floor under it, for access that is lost by any other way, so the triggers are left out here.
	exec(t, w.db, `DROP TRIGGER space_members_drop_jobs`)
	exec(t, w.db, `DROP TRIGGER memberships_drop_jobs`)

	inTeam := w.job("team", 0)
	inTeam.SpaceID = w.team
	noSpace := w.job("workspace-wide", 0)
	noSpace.SpaceID = 0
	private := w.job("private", 0)
	bobs := w.job("bobs", 0)
	bobs.UserID, bobs.SpaceID = w.bob, w.team
	for _, j := range []Job{inTeam, noSpace, private, bobs} {
		w.schedule(j)
	}

	// Alice is taken out of the shared space; and, for the others, out of the workspace.
	exec(t, w.db, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team, w.alice)
	w.run()
	if h.count() != 3 {
		t.Fatalf("calls = %d, want 3 (all but the job in the space she lost)", h.count())
	}
	if j := w.find(w.alice, "team"); j.State != StateFailed || j.LastError != "access lost" {
		t.Fatalf("job in a lost space = %+v", j)
	}
	for _, c := range h.calls {
		if c.DedupeKey == "team" {
			t.Fatal("the handler was called for a space its owner can't read")
		}
	}
	if j := w.find(w.bob, "bobs"); j.State != StateDone {
		t.Fatalf("another person's job in the same space = %+v", j)
	}

	// Leaving the workspace ends her space-less jobs too.
	w.schedule(w.job("later-private", time.Minute))
	w.schedule(func() Job { j := w.job("later-wide", time.Minute); j.SpaceID = 0; return j }())
	exec(t, w.db, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.alice)
	w.clock.advance(time.Hour)
	before := h.count()
	w.run()
	if h.count() != before {
		t.Fatalf("a job ran for a person who left the workspace")
	}
	for _, key := range []string{"later-private", "later-wide"} {
		if j := w.find(w.alice, key); j.State != StateFailed || j.LastError != "access lost" {
			t.Fatalf("%s = %+v", key, j)
		}
	}
}

func TestAJobGoesWithItsOwnersAccess(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	inTeam := w.job("team", time.Hour)
	inTeam.SpaceID = w.team
	bobs := w.job("bobs", time.Hour)
	bobs.UserID, bobs.SpaceID = w.bob, w.team
	private := w.job("private", time.Hour)
	for _, j := range []Job{inTeam, bobs, private} {
		w.schedule(j)
	}
	gone := func(u int64, key string) bool {
		_, err := w.s.Find(context.Background(), w.db, u, "test.remind", key)
		return errors.Is(err, ErrNotFound)
	}

	exec(t, w.db, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team, w.alice)
	if !gone(w.alice, "team") || gone(w.bob, "bobs") || gone(w.alice, "private") {
		t.Fatal("leaving a space must end that person's jobs of it, and no one else's, nor their other jobs")
	}
	exec(t, w.db, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.alice)
	if !gone(w.alice, "private") || gone(w.bob, "bobs") {
		t.Fatal("leaving the workspace must end all that person's jobs in it, and no one else's")
	}
}

func TestAHandlerThatIsRefusedStopsTheJob(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(int, Job) error { return fmt.Errorf("writing the card: %w", ErrAccessLost) }
	every := w.job("poll", 0)
	every.Every = time.Hour
	w.schedule(every)
	w.run()
	w.clock.advance(5 * time.Hour)
	w.run()
	if j := w.find(w.alice, "poll"); h.count() != 1 || j.State != StateFailed {
		t.Fatalf("calls %d, job %+v: a refused recurring job must stop", h.count(), j)
	}
}

func TestAJobInAnotherWorkspacesSpaceIsRefused(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	j := w.job("x", 0)
	j.SpaceID = w.evePrv
	if _, err := w.s.Schedule(context.Background(), w.db, j); !errors.Is(err, ErrInvalid) {
		t.Fatalf("schedule with a space of another workspace = %v", err)
	}
	// Even a row that got there some other way doesn't run. Its owner can read the space (it is
	// eve's own), so only the comparison of the space's workspace with the job's refuses it.
	exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, space_id, dedupe_key, run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'x', ?, ?, ?)`, string(remind), w.eve, w.ws, w.evePrv, stamp(start), stamp(start), stamp(start))
	w.run()
	if j := w.find(w.eve, "x"); j.State != StateFailed || j.LastError != "access lost" {
		t.Fatalf("job = %+v", j)
	}
}

func TestARecurringJobRunsOnItsGrid(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	j := w.job("poll", time.Minute)
	j.Every = time.Hour
	w.schedule(j)

	w.clock.advance(time.Minute)
	w.run()
	got := w.find(w.alice, "poll")
	if h.count() != 1 || got.State != StatePending || !got.RunAt.Equal(start.Add(time.Minute+time.Hour)) {
		t.Fatalf("calls %d, job %+v", h.count(), got)
	}
	// A run a little late doesn't move the grid.
	w.clock.advance(time.Hour + 10*time.Minute)
	w.run()
	if got := w.find(w.alice, "poll"); h.count() != 2 || !got.RunAt.Equal(start.Add(time.Minute+2*time.Hour)) {
		t.Fatalf("calls %d, job %+v", h.count(), got)
	}
	// The server was off for a day: the job runs once, not once per missed hour.
	w.clock.advance(24 * time.Hour)
	w.run()
	got = w.find(w.alice, "poll")
	if h.count() != 3 || !got.RunAt.After(w.clock.now()) || got.RunAt.Sub(w.clock.now()) > time.Hour {
		t.Fatalf("calls %d, job %+v at %v", h.count(), got, w.clock.now())
	}
	if (got.RunAt.Sub(start)-time.Minute)%time.Hour != 0 {
		t.Fatalf("the next run %v left the grid", got.RunAt)
	}
}

func TestARecurringJobThatKeepsFailingMovesOnThenStops(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(int, Job) error { return errors.New("the service is down") }
	j := w.job("poll", 0)
	j.Every = time.Hour
	w.schedule(j)

	runs := 0
	for range MaxFailedRuns + 2 {
		for range defaultMaxAttempts {
			w.run()
			w.clock.advance(backoffMax)
		}
		w.clock.advance(time.Hour)
		if got := w.find(w.alice, "poll"); got.State == StateFailed {
			break
		}
		runs++
	}
	got := w.find(w.alice, "poll")
	if got.State != StateFailed || runs != MaxFailedRuns-1 {
		t.Fatalf("job %+v after %d runs that used all their tries, want it failed after %d", got, runs, MaxFailedRuns)
	}
	if h.count() != MaxFailedRuns*defaultMaxAttempts {
		t.Fatalf("calls = %d, want %d", h.count(), MaxFailedRuns*defaultMaxAttempts)
	}
}

func TestOneSuccessEndsAStreakOfFailedRuns(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	var failing atomic.Bool
	failing.Store(true)
	h.answer = func(int, Job) error {
		if failing.Load() {
			return errors.New("down")
		}
		return nil
	}
	j := w.job("poll", 0)
	j.Every = 24 * time.Hour
	w.schedule(j)
	for range defaultMaxAttempts {
		w.run()
		w.clock.advance(backoffMax)
	}
	if got := w.find(w.alice, "poll"); got.State != StatePending || got.failedRuns != 1 || got.Attempts != 0 {
		t.Fatalf("job = %+v", got)
	}
	failing.Store(false)
	w.clock.advance(24 * time.Hour)
	w.run()
	if got := w.find(w.alice, "poll"); got.failedRuns != 0 || got.LastError != "" {
		t.Fatalf("job = %+v", got)
	}
}

func TestTheRegistryIsClosed(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	if err := w.s.Register(remind, func(context.Context, Job) error { return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("registering a kind twice = %v", err)
	}
	for _, bad := range []Kind{"", "Upper", "has space", "1x", Kind(strings.Repeat("a", 65))} {
		if err := w.s.Register(bad, func(context.Context, Job) error { return nil }); !errors.Is(err, ErrInvalid) {
			t.Fatalf("registering %q = %v", bad, err)
		}
	}
	if err := w.s.Register("test.nil", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("registering no handler = %v", err)
	}
	j := w.job("x", 0)
	j.Kind = "test.unregistered"
	if _, err := w.s.Schedule(context.Background(), w.db, j); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("scheduling an unknown kind = %v", err)
	}
	w.run() // closes it
	if err := w.s.Register("test.late", func(context.Context, Job) error { return nil }); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("registering after the worker ran = %v", err)
	}
}

func TestAKindWithoutAHandlerIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	w.register("test.newer")
	newer := w.job("n", 0)
	newer.Kind = "test.newer"
	w.schedule(newer)
	w.schedule(w.job("r", 0))

	// An older server that doesn't know "test.newer" starts on the same database.
	old := New(w.db, w.access, Options{Clock: w.clock.now})
	h := &recorder{}
	if err := old.Register(remind, h.handle); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(time.Hour)
	old.RunDue(context.Background())
	if h.count() != 1 {
		t.Fatalf("calls = %d, want 1", h.count())
	}
	j, err := old.Find(context.Background(), w.db, w.alice, "test.newer", "n")
	if err != nil || j.State != StatePending || j.Attempts != 0 {
		t.Fatalf("a job of an unknown kind = %+v, %v; want it untouched", j, err)
	}
	// A server with no handler at all does nothing.
	New(w.db, w.access, Options{Clock: w.clock.now}).RunDue(context.Background())
}

func TestInvalidJobsAreRefused(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	with := func(f func(*Job)) Job {
		j := w.job("k", time.Minute)
		f(&j)
		return j
	}
	for name, j := range map[string]Job{
		"no owner":          with(func(j *Job) { j.UserID = 0 }),
		"no workspace":      with(func(j *Job) { j.WorkspaceID = 0 }),
		"no key":            with(func(j *Job) { j.DedupeKey = "" }),
		"a long key":        with(func(j *Job) { j.DedupeKey = strings.Repeat("k", MaxKeyLength+1) }),
		"no time":           with(func(j *Job) { j.RunAt = time.Time{} }),
		"a short interval":  with(func(j *Job) { j.Every = time.Second }),
		"a split second":    with(func(j *Job) { j.Every = time.Minute + time.Millisecond }),
		"an array payload":  with(func(j *Job) { j.Payload = json.RawMessage(`[1]`) }),
		"a string payload":  with(func(j *Job) { j.Payload = json.RawMessage(`"x"`) }),
		"a null payload":    with(func(j *Job) { j.Payload = json.RawMessage(`null`) }),
		"a broken payload":  with(func(j *Job) { j.Payload = json.RawMessage(`{`) }),
		"a huge payload":    with(func(j *Job) { j.Payload = json.RawMessage(`{"a":"` + strings.Repeat("x", MaxPayload) + `"}`) }),
		"a negative space":  with(func(j *Job) { j.SpaceID = -1 }),
		"a space elsewhere": with(func(j *Job) { j.SpaceID = w.evePrv }),
	} {
		if _, err := w.s.Schedule(context.Background(), w.db, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("schedule with %s = %v, want ErrInvalid", name, err)
		}
		if _, err := w.s.Reschedule(context.Background(), w.db, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("reschedule with %s = %v, want ErrInvalid", name, err)
		}
	}
	// No payload is an empty object.
	j := w.job("empty", time.Minute)
	j.Payload = nil
	w.schedule(j)
	if got := w.find(w.alice, "empty"); string(got.Payload) != "{}" {
		t.Fatalf("payload = %s", got.Payload)
	}
}

func TestOnePersonCantFillTheQueue(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	ctx := context.Background()
	// One short of the bound, made in bulk; the last one goes through Schedule.
	exec(t, w.db, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		SELECT ?, ?, ?, 'k' || i, ?, ?, ? FROM n`,
		MaxPerUser-1, string(remind), w.alice, w.ws, stamp(start.Add(time.Hour)), stamp(start), stamp(start))
	if !w.schedule(w.job("k0", time.Hour)) {
		t.Fatal("the last job within the bound was refused")
	}
	if _, err := w.s.Schedule(ctx, w.db, w.job("one-more", time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("job over the bound = %v", err)
	}
	if _, err := w.s.Reschedule(ctx, w.db, w.job("one-more", time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("reschedule of a new job over the bound = %v", err)
	}
	// Changing one they have, or scheduling one already there, is not more jobs.
	if _, err := w.s.Reschedule(ctx, w.db, w.job("k0", 2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if created, err := w.s.Schedule(ctx, w.db, w.job("k0", 2*time.Hour)); err != nil || created {
		t.Fatalf("schedule of a job that exists = %v, %v", created, err)
	}
	// Someone else is not held back by it.
	other := w.job("mine", time.Hour)
	other.UserID, other.SpaceID = w.bob, w.privateSpace(w.ws, w.bob)
	if !w.schedule(other) {
		t.Fatal("another person's job was refused")
	}
}

func TestOldFinishedJobsArePurged(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	w.schedule(w.job("done", 0))
	w.schedule(w.job("waiting", 100*24*time.Hour))
	w.run()
	w.clock.advance(keepFinished - time.Hour)
	if err := w.s.purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.find(w.alice, "done")
	w.clock.advance(2 * time.Hour)
	if err := w.s.purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Find(context.Background(), w.db, w.alice, remind, "done"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job finished long ago is still there: %v", err)
	}
	w.find(w.alice, "waiting")
}

func TestJobsEndWithTheirOwnerSpaceOrWorkspace(t *testing.T) {
	w := newWorld(t)
	w.register(remind)
	inTeam := w.job("team", time.Hour)
	inTeam.SpaceID = w.team
	w.schedule(inTeam)
	w.schedule(w.job("private", time.Hour))
	exec(t, w.db, `DELETE FROM spaces WHERE id = ?`, w.team)
	if _, err := w.s.Find(context.Background(), w.db, w.alice, remind, "team"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job of a deleted space is still there: %v", err)
	}
	exec(t, w.db, `DELETE FROM workspaces WHERE id = ?`, w.ws)
	if _, err := w.s.Find(context.Background(), w.db, w.alice, remind, "private"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job of a deleted workspace is still there: %v", err)
	}
	// And an account's jobs go with it.
	w.schedule(Job{Kind: remind, UserID: w.eve, WorkspaceID: w.other, DedupeKey: "k", RunAt: start})
	exec(t, w.db, `DELETE FROM spaces WHERE owner_id = ?`, w.eve)
	exec(t, w.db, `DELETE FROM users WHERE id = ?`, w.eve)
	if _, err := w.s.Find(context.Background(), w.db, w.eve, remind, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job of a deleted account is still there: %v", err)
	}
}

func TestTheWorkerRunsJobsInTheBackground(t *testing.T) {
	sqlDB := newDB(t)
	exec(t, sqlDB, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'a', 'x')`)
	exec(t, sqlDB, `INSERT INTO workspaces (name) VALUES ('Home')`)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'member')`)
	s := New(sqlDB, access.New(sqlDB, nil), Options{Poll: 10 * time.Millisecond})
	ran := make(chan Job, 4)
	if err := s.Register(remind, func(_ context.Context, j Job) error { ran <- j; return nil }); err != nil {
		t.Fatal(err)
	}
	stop := s.Start(context.Background())
	defer stop()

	// Scheduled after the worker started, in a transaction, and woken after the commit.
	ctx := context.Background()
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(ctx, tx, Job{Kind: remind, UserID: 1, WorkspaceID: 1, DedupeKey: "k", RunAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Wake()
	select {
	case j := <-ran:
		if j.DedupeKey != "k" {
			t.Fatalf("ran %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never ran the job")
	}
	if err := s.Register("test.late", func(context.Context, Job) error { return nil }); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("register after Start = %v", err)
	}
	stop()
	stop2 := s.Start(context.Background()) // a stopped worker can be started again, as a new server would
	stop2()
}

func TestStoppingTheWorkerLeavesTheJobToBeTakenAgain(t *testing.T) {
	sqlDB := newDB(t)
	exec(t, sqlDB, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'a', 'x')`)
	exec(t, sqlDB, `INSERT INTO workspaces (name) VALUES ('Home')`)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'member')`)
	c := &clock{t: start}
	s := New(sqlDB, access.New(sqlDB, nil), Options{Clock: c.now})
	started := make(chan struct{})
	if err := s.Register(remind, func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done() // a handler that is still working when the server stops
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Schedule(ctx, sqlDB, Job{Kind: remind, UserID: 1, WorkspaceID: 1, DedupeKey: "k", RunAt: start}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.RunDue(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // the worker is joined before the database closes
	select {
	case <-started:
	case <-done:
		t.Fatal("the worker returned without calling the handler")
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was never called")
	}
	cancel()
	<-done
	j, err := s.Find(context.Background(), sqlDB, 1, remind, "k")
	if err != nil || j.State != StateRunning || j.LastError != "" {
		t.Fatalf("job after a stop = %+v, %v; want it running, to be taken again when its lease ends", j, err)
	}
}

func TestNextOccurrence(t *testing.T) {
	prev := start
	h := time.Hour
	for _, tc := range []struct {
		now  time.Duration
		want time.Duration
	}{
		{0, h}, {time.Minute, h}, {h - time.Nanosecond, h}, {h, 2 * h}, {h + 1, 2 * h}, {5*h + 30*time.Minute, 6 * h},
	} {
		if got, ok := nextOccurrence(prev, h, prev.Add(tc.now)); !ok || !got.Equal(prev.Add(tc.want)) {
			t.Errorf("now = prev+%v: next = prev+%v, want prev+%v", tc.now, got.Sub(prev), tc.want)
		}
	}
}

// A job whose owner's account is disabled waits: no handler is called, nothing is recorded as a
// failure, no try is used up and a recurring job isn't ended, however long the account stays off.
// Enabling the account lets it run.
func TestAJobOfADisabledOwnerWaitsForTheAccount(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	inSpace := w.job("in-space", time.Minute)
	noSpace := w.job("no-space", time.Minute)
	noSpace.SpaceID = 0
	recurring := w.job("recurring", time.Minute)
	recurring.Every = time.Hour
	bobs := w.job("bobs", time.Minute)
	bobs.UserID, bobs.SpaceID = w.bob, w.team
	for _, j := range []Job{inSpace, noSpace, recurring, bobs} {
		w.schedule(j)
	}
	exec(t, w.db, `UPDATE users SET disabled_at = '2030-01-02T08:00:00.000Z' WHERE id = ?`, w.alice)

	// Far more rounds than a job has tries, over more time than any failure's backoff.
	for range 3 * defaultMaxAttempts {
		w.clock.advance(holdDelay + time.Second)
		w.run()
	}
	for _, key := range []string{"in-space", "no-space", "recurring"} {
		j := w.find(w.alice, key)
		if j.State != StatePending || j.Attempts != 0 || j.LastError != "" {
			t.Fatalf("%s while disabled = %+v, want pending, no try used, no error", key, j)
		}
	}
	if j := w.find(w.alice, "recurring"); j.Every != time.Hour {
		t.Fatalf("recurring = %+v", j)
	}
	for _, c := range h.calls {
		if c.UserID == w.alice {
			t.Fatalf("a handler ran for a disabled owner: %+v", c)
		}
	}
	if j := w.find(w.bob, "bobs"); j.State != StateDone {
		t.Fatalf("another person's job = %+v, want done", j)
	}

	exec(t, w.db, `UPDATE users SET disabled_at = NULL WHERE id = ?`, w.alice)
	w.clock.advance(holdDelay + time.Second)
	w.run()
	for _, key := range []string{"in-space", "no-space"} {
		if j := w.find(w.alice, key); j.State != StateDone {
			t.Fatalf("%s after enabling = %+v, want done", key, j)
		}
	}
	if j := w.find(w.alice, "recurring"); j.State != StatePending || !j.RunAt.After(w.clock.now()) {
		t.Fatalf("recurring after enabling = %+v, want it to have run and moved on", j)
	}
	if got := h.count(); got != 4 {
		t.Fatalf("calls = %d, want 4 (bob's, and alice's three)", got)
	}
}

// A handler's own check, refused because the account was disabled after the worker's, holds the job
// the same way.
func TestAHandlerRefusedForADisabledAccountHoldsTheJob(t *testing.T) {
	w := newWorld(t)
	h := w.register(remind)
	h.answer = func(n int, _ Job) error {
		if n == 1 {
			return fmt.Errorf("checking the space: %w", access.ErrDisabled)
		}
		return nil
	}
	w.schedule(w.job("a", 0))
	w.run()
	if j := w.find(w.alice, "a"); j.State != StatePending || j.Attempts != 0 || j.LastError != "" {
		t.Fatalf("job = %+v, want held", j)
	}
	w.clock.advance(holdDelay + time.Second)
	w.run()
	if j := w.find(w.alice, "a"); j.State != StateDone || h.count() != 2 {
		t.Fatalf("job = %+v, calls %d", j, h.count())
	}
}
