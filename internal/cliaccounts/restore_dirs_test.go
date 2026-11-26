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
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// signedInWithFiles signs user in to Claude Code on the worker the way a turn uses it (the worker
// makes the directory), runs a turn, and leaves a file in their working directory and their
// browser's profile: everything a new person given the number must never see.
func signedInWithFiles(t *testing.T, e *env, user int64) []string {
	t.Helper()
	dirs := e.layout.User(user)
	// The worker's first act for the user makes their directory; then the login is written as the CLI would.
	if _, err := e.worker.Status(context.Background(), user, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := e.worker.Start(cliworker.TurnSpec{ID: fmt.Sprintf("files-%d", user), User: user, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn)
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

// A restore of the server's data gives again the numbers that came after the backup, while the CLI
// worker's directory (a volume of its own in container mode) isn't part of the backup and keeps
// the directories of the people who had them: their logins, working files and browser profiles. A
// person given such a number must start with an empty directory, and the files of the people of
// the lost history must be gone; the people in the backup keep theirs.
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
	keptFiles := signedInWithFiles(t, e, kept)
	var archive bytes.Buffer
	if _, err := backup.Create(ctx, dataA, "test", &archive, backup.Options{}); err != nil {
		t.Fatal(err)
	}
	// After the backup: two people use the CLIs. One's directory the worker dated; the other's is
	// from before it dated directories.
	lost := newUser(t, dbA, "lost@example.com")
	lostFiles := signedInWithFiles(t, e, lost)
	older := newUser(t, dbA, "older@example.com")
	olderDirs, err := e.layout.Ensure(older)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", olderDirs.Config(cliworker.Claude))
	olderFiles := []string{filepath.Join(olderDirs.Config(cliworker.Claude), ".credentials.json")}

	// The operator restores the backup into a data directory and starts a server on it, with the
	// same worker. A person signs up at once, before the server has told the worker anything.
	dataB := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, bytes.NewReader(archive.Bytes()), dataB); err != nil {
		t.Fatal(err)
	}
	dbB := openData(t, dataB)
	time.Sleep(20 * time.Millisecond) // the new account is made after the lost one's directory
	fresh := newUser(t, dbB, "fresh@example.com")
	if fresh != lost {
		t.Fatalf("fixture: the restored database gave %d, want %d (the number the lost history used)", fresh, lost)
	}
	svcB := New(dbB, Container, LocalWorker(e.worker))
	t.Cleanup(svcB.Close)

	// The new person's first sign-in: the server tells the worker who exists first.
	if _, err := svcB.worker.StartLogin(fresh, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	svcB.worker.CancelLogin(fresh, cliworker.Claude, "")
	// And another person gets the number after it, whose directory the worker never dated.
	second := newUser(t, dbB, "second@example.com")
	if second != older {
		t.Fatalf("fixture: the restored database gave %d, want %d", second, older)
	}

	for _, f := range append(lostFiles, olderFiles...) {
		if _, err := os.Stat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of the lost history is still there: %v", f, err)
		}
	}
	for name, user := range map[string]int64{"the person given the lost number": fresh, "the person given the next": second} {
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

// Until the server has told the worker who exists since it started, nothing is asked of the
// worker for a user, and the API says the worker isn't ready, with a code the web app translates.
// Once the worker can be reached, the first request tells it and goes on.
func TestNothingIsAskedOfTheWorkerBeforeItIsTold(t *testing.T) {
	e := newEnv(t, Container, nil)
	ctx := context.Background()
	user := newUser(t, e.db, "one@example.com")
	w := &flakyWorker{Worker: LocalWorker(e.worker), told: make(chan struct{})}
	w.fails.Store(1)
	svc := New(e.db, Container, w)
	t.Cleanup(svc.Close)

	_, refused := svc.worker.Status(ctx, user, cliworker.Claude, "")
	if !errors.Is(refused, ErrNotReady) {
		t.Fatalf("a status check before the worker was told: %v, want ErrNotReady", refused)
	}
	if _, err := svc.worker.StartLogin(user, cliworker.Claude, ""); err != nil {
		t.Fatalf("a sign-in once the worker can be told: %v", err)
	}
	svc.worker.CancelLogin(user, cliworker.Claude, "")
	select {
	case <-w.told:
	default:
		t.Fatal("the sign-in went to the worker before it was told")
	}
	if err := svc.Ready(ctx); err != nil {
		t.Errorf("after the worker was told: %v", err)
	}

	rec := httptest.NewRecorder()
	workerError(rec, httptest.NewRequest(http.MethodPost, "/", nil), ref{cli: cliworker.Claude}, refused)
	if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"`+string(apierr.CodeCLIWorkerNotReady)+`"`)) {
		t.Errorf("answer = %d %s, want 503 %s", rec.Code, rec.Body.String(), apierr.CodeCLIWorkerNotReady)
	}
}
