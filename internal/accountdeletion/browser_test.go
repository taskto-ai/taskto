package accountdeletion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type fakeBrowsers struct {
	dir string

	mu      sync.Mutex
	running map[int64]bool
	calls   []int64
	fail    error
}

func newFakeBrowsers(dir string) *fakeBrowsers {
	return &fakeBrowsers{dir: dir, running: map[int64]bool{}}
}

func (f *fakeBrowsers) browserDir(user int64) string {
	return filepath.Join(f.dir, "cli", fmt.Sprintf("u%d", user), "browser")
}

func (f *fakeBrowsers) start(t *testing.T, user int64) {
	t.Helper()
	profile := filepath.Join(f.browserDir(user), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "Cookies"), []byte("a login"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.running[user] = true
	f.mu.Unlock()
}

func (f *fakeBrowsers) isRunning(user int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[user]
}

func (f *fakeBrowsers) profileExists(user int64) bool {
	_, err := os.Stat(filepath.Join(f.browserDir(user), "profile"))
	return err == nil
}

func (f *fakeBrowsers) Forget(_ context.Context, user int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, user)
	if f.fail != nil {
		return f.fail
	}
	f.running[user] = false
	return os.RemoveAll(f.browserDir(user))
}

func (f *fakeBrowsers) forgotten() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.calls...)
}

type orderedCLI struct {
	fakeCLI
	browsers *fakeBrowsers
	ranWith  []int64
}

func (c *orderedCLI) Forget(ctx context.Context, user int64) error {
	if c.browsers.isRunning(user) {
		c.ranWith = append(c.ranWith, user)
	}
	return c.fakeCLI.Forget(ctx, user)
}

func browserWorld(t *testing.T) (*world, *fakeBrowsers, *orderedCLI) {
	t.Helper()
	w := newWorld(t)
	b := newFakeBrowsers(w.dir)
	cli := &orderedCLI{fakeCLI: fakeCLI{kind: cliContainer}, browsers: b}
	w.svc.o.Browsers = b
	w.svc.o.CLI = cli
	return w, b, cli
}

func TestPurgeStopsTheBrowserAndRemovesItsProfile(t *testing.T) {
	ctx := context.Background()
	w, b, cli := browserWorld(t)
	b.start(t, w.leaver)
	b.start(t, w.carol)
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}

	if got := b.forgotten(); len(got) != 1 || got[0] != w.leaver {
		t.Fatalf("browsers forgotten for %v, want only user %d", got, w.leaver)
	}
	if b.isRunning(w.leaver) || b.profileExists(w.leaver) {
		t.Errorf("the purged account's browser runs: %v, its profile remains: %v", b.isRunning(w.leaver), b.profileExists(w.leaver))
	}
	if len(cli.ranWith) != 0 {
		t.Errorf("the CLI directory was removed while the browser of %v still ran", cli.ranWith)
	}
	if len(cli.users) != 1 || cli.users[0] != w.leaver {
		t.Errorf("the CLI worker forgot %v", cli.users)
	}
	if !b.isRunning(w.carol) || !b.profileExists(w.carol) {
		t.Errorf("another user's browser: running %v, profile %v", b.isRunning(w.carol), b.profileExists(w.carol))
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain", n)
	}
}

func TestAFailedBrowserStepIsRecordedAndRetried(t *testing.T) {
	ctx := context.Background()
	w, b, cli := browserWorld(t)
	b.start(t, w.leaver)
	cliDir := filepath.Join(w.dir, "cli", fmt.Sprintf("u%d", w.leaver))
	b.fail = errors.New("the worker at /secret/path for leaver@example.com did not answer")
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("a purge that couldn't stop the browser reported done")
	}
	var reason string
	if err := w.db.QueryRow(`SELECT last_error FROM purge_cleanups WHERE kind = ?`, cleanBrowser).Scan(&reason); err != nil {
		t.Fatalf("the browser's step isn't recorded: %v", err)
	}
	if reason != failBrowser {
		t.Errorf("last_error = %q, want %q", reason, failBrowser)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ? AND attempts > 0`, cleanBrowser); n != 1 {
		t.Errorf("the failed try isn't counted")
	}
	if len(cli.users) != 0 {
		t.Errorf("the CLI worker forgot %v before the browser was gone", cli.users)
	}
	if _, err := os.Stat(cliDir); err != nil {
		t.Errorf("their CLI directory was removed under a browser that may run: %v", err)
	}
	if w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver) != 1 {
		t.Error("the account was deleted with its browser still running")
	}

	b.fail = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if b.isRunning(w.leaver) || b.profileExists(w.leaver) {
		t.Error("the retry left the browser or its profile")
	}
	if len(cli.ranWith) != 0 || len(cli.users) != 1 {
		t.Errorf("CLI worker: forgot %v, with a browser still running for %v", cli.users, cli.ranWith)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain", n)
	}
}

func TestTheCLIDirectoryWaitsForABrowserStepRecordedAfterIt(t *testing.T) {
	ctx := context.Background()
	w, b, cli := browserWorld(t)
	b.start(t, w.leaver)
	b.fail = errors.New("not yet")
	w.deleteAccount()
	user := fmt.Sprint(w.leaver)
	if err := w.svc.record(ctx, w.db, w.leaver, cleanCLIHome, cliContainer+":"+user); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.record(ctx, w.db, w.leaver, cleanBrowser, user); err != nil {
		t.Fatal(err)
	}

	if err := w.svc.RunCleanups(ctx); err == nil {
		t.Fatal("the failed browser step wasn't reported")
	}
	if len(cli.users) != 0 || len(cli.ranWith) != 0 {
		t.Fatalf("the CLI worker forgot %v (browser running for %v) before the browser step was done", cli.users, cli.ranWith)
	}
	if w.count(`SELECT count(*) FROM purge_cleanups`) != 2 {
		t.Fatal("a step was dropped")
	}

	b.fail = nil
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cli.users) != 1 || len(cli.ranWith) != 0 {
		t.Errorf("after: the CLI worker forgot %v, with a browser running for %v", cli.users, cli.ranWith)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain", n)
	}
}

func TestPurgeWithoutAgentBrowsers(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain", n)
	}
}
