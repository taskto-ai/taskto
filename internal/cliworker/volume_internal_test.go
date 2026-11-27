//go:build unix

package cliworker

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// The UID base is the volume's: pinned the first time, and a worker with another is refused, as it
// is on a volume whose records say their uids were given under another base, so no home's uid is
// ever another home's.
func TestTheUIDBaseIsPinnedToTheVolume(t *testing.T) {
	newVolume := func(t *testing.T) *Worker {
		w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}}, accounts: map[string]accountRecord{}}
		if err := mkdirDurable(w.accountsDir()); err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := newVolume(t)
	if err := w.pinUIDBase(100000); err != nil {
		t.Fatal(err)
	}
	if err := w.pinUIDBase(100000); err != nil {
		t.Errorf("the same base again: %v", err)
	}
	if err := w.pinUIDBase(200000); err == nil {
		t.Error("another base was accepted on a pinned volume")
	}

	// Not pinned, but an account says its uid.
	w = newVolume(t)
	w.accountsMu.Lock()
	err := w.putRecord(IdentityForTest(1), accountRecord{Home: firstHome, User: 1, UID: 100000 + firstHome})
	w.accountsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.pinUIDBase(200000); err == nil {
		t.Error("a base its accounts' uids weren't given under was accepted")
	}
	if err := w.pinUIDBase(100000); err != nil {
		t.Errorf("the accounts' own base: %v", err)
	}
	if b, err := os.ReadFile(w.uidBaseFile()); err != nil || string(b) != strconv.Itoa(100000)+"\n" {
		t.Errorf("the pin = %q %v", b, err)
	}
}

// The worker's metadata directories are made with each one's parent synced, before an account is
// recorded in them, so a crash can't keep a counter or a home and lose the record that owns it.
func TestTheWorkersDirectoriesAreDurableBeforeUse(t *testing.T) {
	var synced []string
	syncedForTest = func(p string) { synced = append(synced, filepath.Clean(p)) }
	t.Cleanup(func() { syncedForTest = nil })
	root := filepath.Join(t.TempDir(), "cli")
	w, err := New(Config{Layout: Layout{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, parent := range []string{root, w.stateDir()} {
		if !slices.Contains(synced, filepath.Clean(parent)) {
			t.Errorf("%s wasn't synced after a directory was made in it (synced %v)", parent, synced)
		}
	}
	synced = nil
	if _, err := w.resolve(AccountForTest(1), true); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{w.stateDir(), w.accountsDir()} {
		if !slices.Contains(synced, filepath.Clean(dir)) {
			t.Errorf("%s wasn't synced after the first account was recorded (synced %v)", dir, synced)
		}
	}
}

// Every account that gets a home gets its uid with it, one a census brings back that never had a
// home too; and an isolated volume with homes whose base nothing can rebuild doesn't start.
func TestEveryHomeComesWithItsUID(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}, Isolation: &Isolation{UIDBase: 100000}}, accounts: map[string]accountRecord{}}
	if err := mkdirDurable(w.accountsDir()); err != nil {
		t.Fatal(err)
	}
	id := IdentityForTest(1)
	// Deleted before the worker gave it a home, then brought back by a restore.
	if err := w.retire(id, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.unretire(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r, ok, err := w.find(id)
	if err != nil || !ok || r.Home == 0 || r.UID != 100000+r.Home {
		t.Fatalf("the account a census brought back: %+v %v %v, want a home and its uid", r, ok, err)
	}

	// The same volume with no uid in its records and no pin: its base can't be rebuilt.
	w.accountsMu.Lock()
	r.UID = 0
	err = w.putRecord(id, r)
	w.accountsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.pinUIDBase(200000); err == nil {
		t.Error("a volume with homes and no base to rebuild was accepted")
	}
}
