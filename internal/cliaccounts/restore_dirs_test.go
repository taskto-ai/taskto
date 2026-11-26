package cliaccounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/conversations"
)

// signedInWithFiles signs user in to Claude Code on the worker the way the server does it (svc
// introduces them first), runs a turn, and leaves a file in their working directory and their
// browser's profile: everything a new person given the number must never see.
func signedInWithFiles(t *testing.T, svc *Service, e *env, user int64) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.worker.Status(ctx, user, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	dirs := e.layout.User(user)
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := svc.worker.StartTurn(cliworker.TurnSpec{ID: fmt.Sprintf("files-%d", user), User: user, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn.(*cliworker.Turn))
	work := filepath.Join(dirs.Conversation(1), "notes.txt")
	profile := filepath.Join(e.layout.BrowserPaths(user).Root, "personal", "Cookies")
	for _, p := range []string{work, profile} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json"), work, profile}
}

// legacyDir makes user's directory as a worker from before owners were recorded did: no owner,
// signed in.
func legacyDir(t *testing.T, e *env, user int64) string {
	t.Helper()
	dirs, err := e.layout.Ensure(user)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	return filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")
}

func goneFile(t *testing.T, what, p string) {
	t.Helper()
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: %s is still there (%v)", what, p, err)
	}
}

// A restore of the server's data gives again the numbers that came after the backup, while the CLI
// worker's directory (a volume of its own in container mode) isn't part of the backup and keeps
// the directories of the people who had them: their logins, working files, browser profiles, and
// the worker, which kept running, their sign-in in progress. A person given such a number starts
// with nothing of anyone's, though they signed up before the server told the worker anything; a
// directory from before the worker recorded owners isn't handed to anyone after a restore; the
// people in the backup keep their own.
func TestARestoreNeverGivesANewPersonAnOldPersonsCLIDirectory(t *testing.T) {
	ctx := context.Background()
	dataA := t.TempDir()
	dbA := openData(t, dataA)
	e := newEnv(t, Container, nil)
	svcA := New(dbA, Container, LocalWorker(e.worker))
	t.Cleanup(svcA.Close)
	if err := svcA.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	kept := newUser(t, dbA, "kept@example.com")
	keptFiles := signedInWithFiles(t, svcA, e, kept)
	// From before the worker recorded owners: after a restore nobody can tell whose it is.
	unowned := newUser(t, dbA, "unowned@example.com")
	unownedLogin := legacyDir(t, e, unowned)
	var archive bytes.Buffer
	if _, err := backup.Create(ctx, dataA, "test", &archive, backup.Options{}); err != nil {
		t.Fatal(err)
	}
	// After the backup: one person uses the CLIs and leaves a sign-in waiting in the worker,
	// another has a directory from before owners were recorded.
	lost := newUser(t, dbA, "lost@example.com")
	lostFiles := signedInWithFiles(t, svcA, e, lost)
	if _, err := svcA.worker.StartLogin(lost, cliworker.Codex, ""); err != nil {
		t.Fatal(err)
	}
	older := newUser(t, dbA, "older@example.com")
	olderLogin := legacyDir(t, e, older)

	// The operator restores the backup into a data directory and starts a server on it, with the
	// same worker, still running. A person signs up at once.
	dataB := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, bytes.NewReader(archive.Bytes()), dataB); err != nil {
		t.Fatal(err)
	}
	dbB := openData(t, dataB)
	fresh := newUser(t, dbB, "fresh@example.com")
	if fresh != lost {
		t.Fatalf("fixture: the restored database gave %d, want %d (the number the lost history used)", fresh, lost)
	}
	svcB := New(dbB, Container, LocalWorker(e.worker))
	t.Cleanup(svcB.Close)

	// Before the worker is told, the new person sees nothing the worker holds for the number.
	if _, ok := svcB.worker.LoginInfo(fresh, cliworker.Codex, ""); ok {
		t.Error("the new person sees the old person's sign-in before the worker was told")
	}
	// Their first sign-in: the server tells the worker who exists, then who they are.
	if _, err := svcB.worker.StartLogin(fresh, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	svcB.worker.CancelLogin(fresh, cliworker.Claude, "")
	second := newUser(t, dbB, "second@example.com")
	if second != older {
		t.Fatalf("fixture: the restored database gave %d, want %d", second, older)
	}

	for _, f := range append(lostFiles, olderLogin) {
		goneFile(t, "the lost history", f)
	}
	goneFile(t, "a directory with no owner after a restore", unownedLogin)
	if _, ok := svcB.worker.LoginInfo(fresh, cliworker.Codex, ""); ok {
		t.Error("the new person sees the old person's sign-in")
	}
	for name, user := range map[string]int64{"the person given the lost number": fresh, "the person given the next": second, "the person whose directory had no owner": unowned} {
		st, err := svcB.worker.Status(ctx, user, cliworker.Claude, "")
		if err != nil || st.LoggedIn {
			t.Errorf("%s (user %d): %+v %v, want not signed in", name, user, st, err)
		}
		entries, err := os.ReadDir(e.layout.User(user).Work)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s (user %d) finds working files: %v", name, user, entries)
		}
		if _, err := os.Stat(e.layout.BrowserPaths(user).Root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s (user %d) finds a browser profile: %v", name, user, err)
		}
	}
	for _, f := range keptFiles {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("the restored person's own %s went: %v", f, err)
		}
	}
	if st, err := svcB.worker.Status(ctx, kept, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the restored person is no longer signed in: %+v %v", st, err)
	}
}

// A directory from before the worker recorded owners is never handed over in container mode, with
// no sign of a restore anywhere (a restore by an older version leaves none): its person signs in to
// their CLIs once more. In local mode, where the directories travel with the server's backups, it is
// adopted.
func TestOnlyLocalModeAdoptsADirectoryWithNoOwner(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []Mode{Container, LocalUnsafe} {
		e := newEnv(t, mode, nil)
		user := newUser(t, e.db, "person@example.com")
		login := legacyDir(t, e, user)
		st, err := e.svc.worker.Status(ctx, user, cliworker.Claude, "")
		if err != nil {
			t.Fatal(err)
		}
		_, ferr := os.Stat(login)
		switch {
		case mode == Container && (st.LoggedIn || !errors.Is(ferr, fs.ErrNotExist)):
			t.Errorf("container mode handed over a directory with no owner: signed in %v, %v", st.LoggedIn, ferr)
		case mode != Container && (!st.LoggedIn || ferr != nil):
			t.Errorf("local mode didn't adopt the account's own directory: signed in %v, %v", st.LoggedIn, ferr)
		}
	}
}

// heldWorker is a worker the server can't tell who exists until it is let go: its Reconcile fails
// while down is set, or waits while hold is open. It counts the attempts.
type heldWorker struct {
	Worker
	down      atomic.Bool
	hold      chan struct{}
	attempts  atomic.Int32
	available bool
}

func (h *heldWorker) Reconcile(ctx context.Context, c cliworker.Census) error {
	h.attempts.Add(1)
	if h.hold != nil {
		<-h.hold
	}
	if h.down.Load() {
		return errors.New("the CLI worker is unreachable")
	}
	return h.Worker.Reconcile(ctx, c)
}

func (h *heldWorker) Available(cli cliworker.CLI) bool {
	if h.down.Load() {
		return h.available
	}
	return h.Worker.Available(cli)
}

// A worker that kept running across a restore holds the old owner of a number's sign-in in
// progress, finished turns and their permission questions. While the census hasn't arrived, the
// new owner reaches none of it through any call; once it has, the worker has dropped it all.
func TestTheNewOwnerOfANumberReachesNothingOfTheOld(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Container, nil)
	old := newUser(t, e.db, "old@example.com")
	if _, err := e.svc.worker.StartLogin(old, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	dirs := e.layout.User(old)
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := e.svc.worker.StartTurn(cliworker.TurnSpec{ID: "c1-old", User: old, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000003"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn.(*cliworker.Turn))

	// A restore: in the new database the number is someone else's.
	dbB := openData(t, t.TempDir())
	if again := newUser(t, dbB, "new@example.com"); again != old {
		t.Fatalf("fixture: %d, want %d", again, old)
	}
	held := &heldWorker{Worker: LocalWorker(e.worker)}
	held.down.Store(true)
	svc := New(dbB, Container, held)
	t.Cleanup(svc.Close)

	// The census never arrived.
	if _, ok := svc.worker.LoginInfo(old, cliworker.Claude, ""); ok {
		t.Error("LoginInfo answered before the census")
	}
	if _, ok := svc.worker.FindTurn(old, "c1-old"); ok {
		t.Error("FindTurn answered before the census")
	}
	for name, err := range map[string]error{
		"a code":     svc.worker.SubmitLoginCode(old, cliworker.Claude, "", "code-1"),
		"a status":   func() error { _, err := svc.worker.Status(ctx, old, cliworker.Claude, ""); return err }(),
		"questions":  func() error { _, err := svc.worker.Permissions(ctx, old, 1); return err }(),
		"an answer":  svc.worker.Answer(ctx, old, 1, "q", true),
		"a sign-out": svc.worker.Logout(ctx, old, cliworker.Claude, ""),
	} {
		if !errors.Is(err, ErrNotReady) {
			t.Errorf("%s before the census: %v, want ErrNotReady", name, err)
		}
	}
	svc.worker.CancelLogin(old, cliworker.Claude, "")
	if l, ok := e.worker.Login(old, cliworker.Claude, ""); !ok || l.Info().State.Final() {
		t.Error("fixture: a cancel before the census reached the worker")
	}

	// The census arrives, but the number's new owner isn't introduced yet: still nothing.
	held.down.Store(false)
	if err := svc.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.worker.FindTurn(old, "c1-old"); ok {
		t.Error("FindTurn answered between the census and the owner's introduction")
	}
	svc.worker.StopTurn(old, "c1-old")
	// Then it is: the number's directory, sign-in, turns and questions are gone.
	if err := svc.ReadyUser(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.worker.LoginInfo(old, cliworker.Claude, ""); ok {
		t.Error("the new owner sees the old owner's sign-in")
	}
	if _, ok := svc.worker.FindTurn(old, "c1-old"); ok {
		t.Error("the new owner can attach to the old owner's turn")
	}
	if qs, err := svc.worker.Permissions(ctx, old, 1); err != nil || len(qs) != 0 {
		t.Errorf("the old owner's questions: %v %v", qs, err)
	}
	if st, err := svc.worker.Status(ctx, old, cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the new owner: %+v %v, want not signed in", st, err)
	}
}

// While the worker can't be told who exists, the API says it isn't ready (503,
// cli_worker_not_ready) before anything else of the worker's: not that the CLI isn't installed,
// and not as a 200 stream for a turn. A request waiting for the server to tell the worker gives up
// with its own context, while the one attempt goes on for the others.
func TestNotReadyIsAnsweredFirst(t *testing.T) {
	e := newEnv(t, Container, nil)
	owner, _ := e.setup()
	held := &heldWorker{Worker: LocalWorker(e.worker)} // unreachable: nothing is available either
	held.down.Store(true)
	e.svc.worker = readyWorker{Worker: held, s: e.svc}

	notReady := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"`+string(apierr.CodeCLIWorkerNotReady)+`"`)) {
			t.Errorf("%s = %d %s, want 503 %s", name, rec.Code, rec.Body.String(), apierr.CodeCLIWorkerNotReady)
		}
	}
	const base = "/api/me/cli-accounts/claude"
	notReady("start a sign-in", owner.do(http.MethodPost, base+"/login", map[string]any{}))
	notReady("read a sign-in", owner.do(http.MethodGet, base+"/login", nil))
	notReady("give a code", owner.do(http.MethodPost, base+"/login/code", map[string]string{"code": "c"}))
	notReady("cancel a sign-in", owner.do(http.MethodDelete, base+"/login", nil))
	notReady("check", owner.do(http.MethodPost, base+"/check", map[string]any{}))

	rec := httptest.NewRecorder()
	body := `{"turn":"t1","cli":"claude","model":"default","prompt":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/cli/turn", bytes.NewReader([]byte(body)))
	e.svc.handleTurn(rec, r, conversations.Principal{User: owner.id, Conversation: 1})
	notReady("a turn", rec)

	// A request gives up with its context; the attempt it joined goes on, one for all.
	held.hold = make(chan struct{})
	held.down.Store(false)
	before := held.attempts.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := e.svc.Ready(ctx); !errors.Is(err, ErrNotReady) {
		t.Errorf("Ready while the attempt is held = %v", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("a request waited %s for an attempt held, past its own context", waited)
	}
	done := make(chan error, 1)
	go func() { done <- e.svc.Ready(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	close(held.hold)
	if err := <-done; err != nil {
		t.Errorf("Ready once the attempt ended: %v", err)
	}
	if n := held.attempts.Load() - before; n != 1 {
		t.Errorf("%d attempts for the waiting requests, want one", n)
	}
}

// downAfter is a worker that becomes unreachable once down is set, after the server was ready.
type downAfter struct {
	Worker
	down atomic.Bool
}

func (d *downAfter) Availability(cli cliworker.CLI) (bool, error) {
	if d.down.Load() {
		return false, fmt.Errorf("%w: dial unix: connect: no such file", cliworker.ErrUnreachable)
	}
	return d.Available(cli), nil
}

// A worker that can't be reached after the server was ready, and the caller introduced, is still
// the not-ready answer with its code: not "the CLI isn't installed", and not a turn's stream that
// fails. And one user's turn is none of another's, to find or stop, knowing its ID.
func TestAnOutageAfterReadinessIsNotReady(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Container, nil)
	owner, _ := e.setup()
	d := &downAfter{Worker: LocalWorker(e.worker)}
	e.svc.worker = readyWorker{Worker: d, s: e.svc}
	if err := e.svc.ReadyUser(ctx, owner.id); err != nil {
		t.Fatal(err)
	}
	dirs, err := e.layout.Ensure(owner.id)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	clitest.WriteScenario(t, dirs.Config(cliworker.Claude), clitest.Scenario{SleepMS: 20_000})
	turn, _, err := e.svc.worker.StartTurn(cliworker.TurnSpec{ID: "c1-mine", User: owner.id, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000004"})
	if err != nil {
		t.Fatal(err)
	}
	other := newUser(t, e.db, "other@example.com")
	if err := e.svc.ReadyUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.svc.worker.FindTurn(other, "c1-mine"); ok {
		t.Error("another user found the turn")
	}
	e.svc.worker.StopTurn(other, "c1-mine")
	time.Sleep(100 * time.Millisecond)
	if turn.Over() {
		t.Error("another user's stop stopped the turn")
	}
	e.svc.worker.StopTurn(owner.id, "c1-mine")

	d.down.Store(true)
	rec := owner.do(http.MethodPost, "/api/me/cli-accounts/claude/login", map[string]any{})
	if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"`+string(apierr.CodeCLIWorkerNotReady)+`"`)) {
		t.Errorf("a sign-in in an outage = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/cli/turn", bytes.NewReader([]byte(`{"turn":"t2","cli":"claude","model":"default","prompt":"hi"}`)))
	e.svc.handleTurn(rec, r, conversations.Principal{User: owner.id, Conversation: 1})
	if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"`+string(apierr.CodeCLIWorkerNotReady)+`"`)) {
		t.Errorf("a turn in an outage = %d %s", rec.Code, rec.Body.String())
	}
}

// incompleteWorker is a worker whose census comes back incomplete (a directory couldn't be removed
// yet), and that still holds a turn under every ID.
type incompleteWorker struct {
	Worker
	stopped atomic.Int32
}

func (*incompleteWorker) Reconcile(context.Context, cliworker.Census) error {
	return fmt.Errorf("%w: user 1: %w", cliworker.ErrIncomplete, cliworker.ErrBusy)
}

func (*incompleteWorker) FindTurn(int64, string) (Turn, bool) { return nil, true }

func (w *incompleteWorker) StopTurn(int64, string) { w.stopped.Add(1) }

// While a census came back incomplete, the server goes on, but a user it hasn't introduced reaches
// no turn the worker holds under their number: it may be an old owner's, whose directory couldn't
// be removed yet.
func TestAPartialCensusGivesNoTurnBeforeTheIntroduction(t *testing.T) {
	e := newEnv(t, Container, nil)
	w := &incompleteWorker{}
	svc := New(e.db, Container, w)
	t.Cleanup(svc.Close)
	if err := svc.Ready(context.Background()); err != nil {
		t.Fatalf("an incomplete census stops the server: %v", err)
	}
	if _, ok := svc.worker.FindTurn(1, "c1-old"); ok {
		t.Error("a turn was found for a user not introduced")
	}
	svc.worker.StopTurn(1, "c1-old")
	if w.stopped.Load() != 0 {
		t.Error("a turn was stopped for a user not introduced")
	}
}
