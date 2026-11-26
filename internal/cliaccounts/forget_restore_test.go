package cliaccounts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/db"
)

// openData opens (and migrates) the database of data directory dir.
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

// newUser adds a user the way the server does: with a number above every number there has been
// (auth's insertUser), so a purged account's number is not given again by this database.
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

// purge deletes user from the database and has the worker forget them, as the account purge does.
func purge(t *testing.T, svc *Service, sqlDB *sql.DB, user int64) {
	t.Helper()
	if _, err := sqlDB.Exec(`DELETE FROM users WHERE id = ?`, user); err != nil {
		t.Fatal(err)
	}
	if err := svc.Forget(context.Background(), user); err != nil {
		t.Fatal(err)
	}
}

// A restore of the server's data goes back in the database's history, while the CLI worker's own
// directory (a volume of its own in container mode) is not part of the backup. What the worker
// forgot must not be refused after it, or a live person is locked out for good: the numbers
// after the backup are given again, and a person whose account was purged since may be back.
// After the server has told the worker who is here (at every start), only the accounts that are
// really gone stay refused.
func TestForgottenMarksDoNotOutliveARestore(t *testing.T) {
	ctx := context.Background()
	dataA := t.TempDir()
	dbA := openData(t, dataA)
	// The worker is on a root of its own, as the container worker is: no backup covers it.
	e := newEnv(t, Container, nil)
	svcA := New(dbA, Container, LocalWorker(e.worker))
	t.Cleanup(svcA.Close)

	one := newUser(t, dbA, "one@example.com")
	gone := newUser(t, dbA, "gone@example.com")
	back := newUser(t, dbA, "back@example.com")
	purge(t, svcA, dbA, gone) // purged before the backup: gone for good

	var archive bytes.Buffer
	if _, err := backup.Create(ctx, dataA, "test", &archive, backup.Options{}); err != nil {
		t.Fatal(err)
	}

	// After the backup: a person is added and deleted, and the account that is in the backup is
	// purged.
	later := newUser(t, dbA, "later@example.com")
	purge(t, svcA, dbA, later)
	purge(t, svcA, dbA, back)
	if next := newUser(t, dbA, "after@example.com"); next <= later {
		t.Fatalf("fixture: the database gave %d again", next)
	}
	time.Sleep(20 * time.Millisecond) // the marks are older than the next server's read

	// The operator restores the backup into a data directory and starts a server on it.
	dataB := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, bytes.NewReader(archive.Bytes()), dataB); err != nil {
		t.Fatal(err)
	}
	dbB := openData(t, dataB)
	svcB := New(dbB, Container, LocalWorker(e.worker))
	t.Cleanup(svcB.Close)
	// A person is added in the restored database and gets the number the other history gave away.
	fresh := newUser(t, dbB, "fresh@example.com")
	if fresh != later {
		t.Fatalf("fixture: the restored database gave %d, want %d (the number the lost history used)", fresh, later)
	}

	started := func(user int64) error {
		_, err := e.worker.StartLogin(user, cliworker.Claude, "")
		return err
	}
	// Until the server has told the worker, it refuses: that is the moment the fix closes.
	if err := started(fresh); !errors.Is(err, cliworker.ErrForgotten) {
		t.Fatalf("before the server tells the worker: %v (fixture)", err)
	}
	if err := svcB.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	// From now on as the server asks: it introduces each account first.
	started = func(user int64) error {
		_, err := svcB.worker.StartLogin(user, cliworker.Claude, "")
		return err
	}
	for name, user := range map[string]int64{"the person given a number again": fresh, "the person whose purge the restore undid": back, "a person never forgotten": one} {
		if err := started(user); err != nil {
			t.Errorf("%s (user %d) is refused after the restore: %v", name, user, err)
		}
		svcB.worker.CancelLogin(user, cliworker.Claude, "")
	}
	// The account that was gone before the backup is still gone: the worker itself still refuses
	// it (the service would refuse it anyway, with no account in its database).
	if _, err := e.worker.StartLogin(gone, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("an account that is gone for good was let back: %v", err)
	}
	// And a turn runs on the restored person's own login, as it did.
	if err := svcB.ReadyUser(ctx, back); err != nil {
		t.Fatal(err)
	}
	dirs, err := e.layout.Ensure(back)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := e.worker.Start(cliworker.TurnSpec{ID: "back-1", User: back, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000001"})
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

// flakyWorker is a worker that can't be reached for its first few Reconcile calls.
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

// A worker that can't be reached when the server starts (in container mode it starts beside it)
// is told as soon as it can be, and its refusal of a forgotten user is an answer the API gives
// with a code the web app translates.
func TestStartReconcileRetriesAndForgottenIsAnAPIError(t *testing.T) {
	e := newEnv(t, Container, nil)
	ctx := context.Background()
	if err := e.svc.Forget(ctx, 9); err != nil {
		t.Fatal(err)
	}
	// The database has no user 9 and the next number is 1: the mark is for a number to come.
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
	if _, err := e.worker.Status(ctx, 9, cliworker.Claude, ""); errors.Is(err, cliworker.ErrForgotten) {
		t.Error("the mark of a number that comes next was kept")
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

// An account whose purge began keeps its mark across a start of the server, until the purge has
// deleted it: the purge forgets the worker's files before it deletes the account, and one that
// failed in between is made again from its start. A person whose deletion is only requested is
// not forgotten yet, and nothing marks them.
func TestReconcileKeepsTheMarkOfAPurgeThatBegan(t *testing.T) {
	e := newEnv(t, Container, nil)
	ctx := context.Background()
	begun := newUser(t, e.db, "begun@example.com")
	other := newUser(t, e.db, "other@example.com")
	for _, user := range []int64{begun, other} {
		if err := e.svc.Forget(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.Exec(`INSERT INTO account_deletions (user_id, requested_at, purge_at, purge_started_at) VALUES (?, 'x', 'x', 'x')`, begun); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the marks are older than the read
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.worker.StartLogin(begun, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the account whose purge began was let back: %v", err)
	}
	if _, err := e.svc.worker.StartLogin(other, cliworker.Claude, ""); err != nil {
		t.Errorf("an account that is live and not being purged is refused: %v", err)
	}
	e.svc.worker.CancelLogin(other, cliworker.Claude, "")
}
