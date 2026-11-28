package cliaccounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func signedInWithFiles(t *testing.T, svc *Service, e *env, user int64) []string {
	t.Helper()
	a := accountIn(t, svc, user)
	dirs, err := e.worker.AccountDirs(a, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	turn, _, err := svc.worker.StartTurn(cliworker.TurnSpec{ID: fmt.Sprintf("files-%d", user), User: user, Identity: a.Identity, Boot: a.Boot, CLI: cliworker.Claude, Conversation: 1, Prompt: "hi", Session: "00000000-0000-4000-8000-000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn.(*cliworker.Turn))
	home, _ := e.worker.HomeOf(a.Identity)
	work := filepath.Join(dirs.Conversation(1), "notes.txt")
	profile := filepath.Join(e.layout.BrowserPaths(home).Root, "personal", "Cookies")
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

func goneFile(t *testing.T, what, p string) {
	t.Helper()
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: %s is still there (%v)", what, p, err)
	}
}

func keptFile(t *testing.T, what, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("%s: %s went (%v)", what, p, err)
	}
}

func TestARestoreNeverGivesANewPersonAnOldPersonsCLIDirectory(t *testing.T) {
	ctx := context.Background()
	dataA := t.TempDir()
	dbA := openData(t, dataA)
	e := newEnv(t, Container, nil)
	svcA := New(dbA, Container, LocalWorker(e.worker))
	t.Cleanup(svcA.Close)
	if err := svcA.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}

	kept := newUser(t, dbA, "kept@example.com")
	keptFiles := signedInWithFiles(t, svcA, e, kept)
	var archive bytes.Buffer
	if _, err := backup.Create(ctx, dataA, "test", &archive, backup.Options{}); err != nil {
		t.Fatal(err)
	}
	lost := newUser(t, dbA, "lost@example.com")
	lostFiles := signedInWithFiles(t, svcA, e, lost)
	if _, err := svcA.worker.StartLogin(lost, cliworker.Codex, ""); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(e.layout.Root, fmt.Sprintf("u%d", lost+1), "config", "claude")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", legacy)

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

	freshAcct := accountIn(t, svcB, fresh)
	if _, ok, err := svcB.worker.LoginInfo(fresh, cliworker.Codex, ""); err != nil || ok {
		t.Errorf("the new person sees the old person's sign-in: %v %v", ok, err)
	}
	if st, err := svcB.worker.Status(ctx, fresh, cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the new person before the census: %+v %v, want not signed in", st, err)
	}
	freshHome, _ := e.worker.HomeOf(freshAcct.Identity)
	lostHome, _ := e.worker.HomeOf(accountIn(t, svcA, lost).Identity)
	if freshHome == lostHome {
		t.Fatal("the new person was given the old person's home")
	}
	if entries, err := os.ReadDir(e.layout.Home(freshHome).Work); err != nil && !errors.Is(err, fs.ErrNotExist) || len(entries) > 0 {
		t.Errorf("the new person's working directory: %v %v", entries, err)
	}

	if err := svcB.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	for _, f := range lostFiles {
		goneFile(t, "the lost history", f)
	}
	goneFile(t, "a directory from before homes", legacy)
	for _, f := range keptFiles {
		keptFile(t, "the person in the backup", f)
	}
	for name, a := range map[string]cliowner.Account{"the person in the backup": accountIn(t, svcB, kept), "the new person": freshAcct} {
		st, err := e.worker.Status(ctx, a, cliworker.Claude, "")
		if err != nil || st.LoggedIn != (name == "the person in the backup") {
			t.Errorf("%s after the census: %+v %v", name, st, err)
		}
	}
	if _, err := e.worker.Status(ctx, accountIn(t, svcA, lost), cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the lost history's account after the census: %v", err)
	}
}
