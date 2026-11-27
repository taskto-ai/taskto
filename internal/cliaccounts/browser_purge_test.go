package cliaccounts

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/browser"
)

// In local mode the browsers' files are in the account's CLI home. A deleted account's purge of
// them finds that home retired or not, and never makes one: it removes the browser's files and
// leaves the CLIs' to their own step; once a census or the worker's Forget retired the account and
// removed its home, the purge has nothing left to do, and says so.
func TestALocalBrowserPurgeFindsARetiredAccountsHome(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, LocalUnsafe, nil)
	// The server's census at its start: deletions are taken after it.
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	owner, _ := e.setup()
	a := e.acct(owner.id)
	dirs := e.dirs(owner.id)
	p := browser.HomePaths(dirs.Root)
	for _, f := range []string{filepath.Join(p.Profile(), "Cookies"), filepath.Join(p.Run(), "xauthority")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := &browser.LocalHost{Root: e.layout.Root, Home: e.svc.LocalHome, Found: e.svc.LocalHomeFound}
	if err := host.Purge(a); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.Root, p.Short} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s remains: %v", d, err)
		}
	}
	if _, err := os.Stat(dirs.Home); err != nil {
		t.Errorf("the CLIs' files went with the browser's: %v", err)
	}

	// The worker retires the account and removes its home; the purge's step, tried again, is done.
	if err := e.worker.Forget(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := host.Purge(a); err != nil {
		t.Errorf("the purge of a retired account's browser: %v", err)
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the purge made the retired account's home again: %v", err)
	}
}
