package cliaccounts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/db"
)

func openData(t *testing.T, dir string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func newUser(t *testing.T, sqlDB *sql.DB, email string) int64 {
	t.Helper()
	var id int64
	err := sqlDB.QueryRowContext(context.Background(), `
		INSERT INTO users (id, email, name, password_hash)
		VALUES (max(coalesce((SELECT max(id) FROM users), 0), (SELECT high FROM user_ids WHERE id = 1)) + 1, ?, 'x', 'x')
		RETURNING id`, email).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func purge(t *testing.T, svc *Service, sqlDB *sql.DB, user int64) {
	t.Helper()
	if _, err := sqlDB.Exec(`DELETE FROM users WHERE id = ?`, user); err != nil {
		t.Fatal(err)
	}
	if err := svc.Forget(context.Background(), user); err != nil {
		t.Fatal(err)
	}
}

func accountIn(t *testing.T, svc *Service, user int64) cliowner.Account {
	t.Helper()
	a, err := svc.Account(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestARetiredAccountComesBackOnlyWithACensus(t *testing.T) {
	ctx := context.Background()
	dataA := t.TempDir()
	dbA := openData(t, dataA)
	e := newEnv(t, Container, nil)
	svcA := New(dbA, Container, LocalWorker(e.worker))
	t.Cleanup(svcA.Close)
	if err := svcA.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}

	one := newUser(t, dbA, "one@example.com")
	gone := newUser(t, dbA, "gone@example.com")
	back := newUser(t, dbA, "back@example.com")
	goneAcct, backAcct := accountIn(t, svcA, gone), accountIn(t, svcA, back)
	purge(t, svcA, dbA, gone)

	var archive bytes.Buffer
	if _, err := backup.Create(ctx, dataA, "test", &archive, backup.Options{}); err != nil {
		t.Fatal(err)
	}
	later := newUser(t, dbA, "later@example.com")
	purge(t, svcA, dbA, later)
	purge(t, svcA, dbA, back)
	time.Sleep(20 * time.Millisecond)

	dataB := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, bytes.NewReader(archive.Bytes()), dataB); err != nil {
		t.Fatal(err)
	}
	dbB := openData(t, dataB)
	svcB := New(dbB, Container, LocalWorker(e.worker))
	t.Cleanup(svcB.Close)
	fresh := newUser(t, dbB, "fresh@example.com")
	if fresh != later {
		t.Fatalf("fixture: the restored database gave %d, want %d (the number the lost history used)", fresh, later)
	}

	if _, err := svcB.worker.StartLogin(back, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("a retired account before the census said it is live: %v", err)
	}
	if _, err := svcB.worker.StartLogin(fresh, cliworker.Claude, ""); err != nil {
		t.Errorf("the new person, before any census: %v", err)
	}
	_ = svcB.worker.CancelLogin(fresh, cliworker.Claude, "")

	if err := svcB.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	for name, user := range map[string]int64{"the person whose purge the restore undid": back, "a person never forgotten": one, "the new person": fresh} {
		if _, err := svcB.worker.StartLogin(user, cliworker.Claude, ""); err != nil {
			t.Errorf("%s (user %d) is refused after the census: %v", name, user, err)
		}
		_ = svcB.worker.CancelLogin(user, cliworker.Claude, "")
	}
	if _, err := e.worker.StartLogin(goneAcct, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("an account that is gone for good was let back: %v", err)
	}
	dirs, err := e.worker.AccountDirs(backAcct, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := e.worker.Start(cliworker.TurnSpec{ID: "back-1", User: back, Identity: backAcct.Identity, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn)
}

func waitTurn(t *testing.T, turn *cliworker.Turn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := 0
	for {
		events, done, err := turn.Wait(ctx, seen)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(events)
		if done {
			if last := events[len(events)-1]; last.Status != cliworker.StatusOK {
				t.Fatalf("the turn ended %+v", last)
			}
			return
		}
	}
}

type flakyWorker struct {
	Worker
	fails atomic.Int32
	told  chan struct{}
}

func (f *flakyWorker) Reconcile(ctx context.Context, c cliworker.Census) error {
	if f.fails.Add(-1) >= 0 {
		return errors.New("the CLI worker is unreachable")
	}
	err := f.Worker.Reconcile(ctx, c)
	close(f.told)
	return err
}

func TestStartReconcileRetriesAndForgottenIsAnAPIError(t *testing.T) {
	e := newEnv(t, Container, nil)
	ctx := context.Background()
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Forget(ctx, 9); err != nil {
		t.Fatal(err)
	}
	w := &flakyWorker{Worker: LocalWorker(e.worker), told: make(chan struct{})}
	w.fails.Store(1)
	svc := New(e.db, Container, w)
	t.Cleanup(svc.Close)
	svc.StartReconcile()
	select {
	case <-w.told:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker was never told")
	}

	rec := httptest.NewRecorder()
	workerError(rec, httptest.NewRequest(http.MethodPost, "/", nil), ref{cli: cliworker.Claude}, fmt.Errorf("starting: %w", cliworker.ErrForgotten))
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusGone || body.Code != string(apierr.CodeCLIAccountForgotten) {
		t.Errorf("answer = %d %q, want 410 %q", rec.Code, body.Code, apierr.CodeCLIAccountForgotten)
	}
}

func TestReconcileKeepsTheMarkOfAPurgeThatBegan(t *testing.T) {
	e := newEnv(t, Container, nil)
	ctx := context.Background()
	begun := newUser(t, e.db, "begun@example.com")
	other := newUser(t, e.db, "other@example.com")
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	for _, user := range []int64{begun, other} {
		if err := e.svc.Forget(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.Exec(`INSERT INTO account_deletions (user_id, requested_at, purge_at, purge_started_at) VALUES (?, 'x', 'x', 'x')`, begun); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	again := New(e.db, Container, LocalWorker(e.worker))
	t.Cleanup(again.Close)
	if err := again.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.worker.StartLogin(e.acct(begun), cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the account whose purge began was let back: %v", err)
	}
	if _, err := again.worker.StartLogin(other, cliworker.Claude, ""); err != nil {
		t.Errorf("an account that is live and not being purged is refused: %v", err)
	}
	_ = again.worker.CancelLogin(other, cliworker.Claude, "")
}

func TestADeletionWaitsForTheOperatorAfterTheWorkerLostItsHistory(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Container, nil)
	sw := e.swappable()
	user := newUser(t, e.db, "leaving@example.com")
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	dirs := e.dirs(user)
	e.worker.Close()
	if err := os.Remove(filepath.Join(e.layout.Root, ".worker", "boots.json")); err != nil {
		t.Fatal(err)
	}
	start := func() *cliworker.Worker {
		w, err := cliworker.New(cliworker.Config{Layout: e.layout, UnsafePrivileged: os.Geteuid() == 0})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Close)
		sw.use(LocalWorker(w))
		return w
	}
	w := start()
	for range 2 {
		if err := e.svc.Forget(ctx, user); !errors.Is(err, cliowner.ErrAuthorityLost) {
			t.Fatalf("a deletion while the worker lost its history: %v, want ErrAuthorityLost", err)
		}
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Fatalf("the refused deletion removed something: %v", err)
	}
	w.Close()
	if err := cliworker.TrustServer(e.layout, e.svc.Boot(), ""); err != nil {
		t.Fatal(err)
	}
	w = start()
	if err := e.svc.Forget(ctx, user); err != nil {
		t.Fatalf("the deletion once the worker was handed over: %v", err)
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the deleted account's home remains: %v", err)
	}
	if _, err := w.AccountDirs(accountIn(t, e.svc, user), "", ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the deleted account after its deletion: %v, want ErrForgotten", err)
	}
}

func TestAServerOfAnotherDataDirectoryCannotActOnTheWorker(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Container, nil)
	dbB := openData(t, t.TempDir())
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	user := newUser(t, e.db, "kept@example.com")
	dirs := e.dirs(user)
	svcB := New(dbB, Container, LocalWorker(e.worker))
	t.Cleanup(svcB.Close)
	instA, _ := e.svc.Installation(ctx)
	instB, _ := svcB.Installation(ctx)
	if instA == instB || instA == "" {
		t.Fatalf("fixture: the installations %q and %q", instA, instB)
	}
	if err := svcB.ReconcileForgotten(ctx); !errors.Is(err, cliowner.ErrOtherInstallation) {
		t.Errorf("the other data directory's census: %v, want ErrOtherInstallation", err)
	}
	if err := svcB.Forget(cliowner.With(ctx, accountIn(t, e.svc, user).Identity), user); !errors.Is(err, cliowner.ErrOtherInstallation) {
		t.Errorf("the other data directory's deletion: %v, want ErrOtherInstallation", err)
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("the account's home: %v", err)
	}
	rec := httptest.NewRecorder()
	svcB.handleWorkerStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	got := decode[WorkerStatus](t, rec)
	if !got.OtherInstallation || got.Installation != instB || got.Boot != svcB.Boot() {
		t.Errorf("the other data directory's operators see %+v", got)
	}
}
