//go:build unix

package cliworker

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
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
	if err := w.unretire(id, 1); err != nil { // a census registered after the retirement
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

// A home's directory whose record was lost, with the pin, is a home given under some base: the
// volume's base can't be rebuilt, and a worker doesn't start on it with isolation.
func TestAnOrphanHomeKeepsTheUIDBaseFromBeingGuessed(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}}, accounts: map[string]accountRecord{}}
	if err := mkdirDurable(w.accountsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.cfg.Layout.Home(firstHome).Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.checkCounter(); err != nil {
		t.Fatal(err)
	}
	if err := w.pinUIDBase(99999); err == nil {
		t.Error("a UID base was accepted on a volume with a home and no record of its base")
	}
}

// A handover says it worked only if a starting worker would trust what it wrote: here what it
// wrote is damaged before it reads it back, and it says so.
func TestAHandoverChecksWhatItWrote(t *testing.T) {
	layout := Layout{Root: t.TempDir()}
	trustHookForTest = func(l Layout) {
		_ = os.WriteFile(filepath.Join(l.Root, ".worker", "boots.json"), []byte("{"), 0o600)
	}
	t.Cleanup(func() { trustHookForTest = nil })
	if err := TrustServer(layout, TestBoot, TestInstallation); err == nil {
		t.Error("a handover whose writes a worker wouldn't trust said it worked")
	}
	trustHookForTest = nil
	if err := TrustServer(layout, TestBoot, TestInstallation); err != nil {
		t.Errorf("a sound handover: %v", err)
	}
}

// A directory from before homes (Root/u<n>) stays on the volume until a census removes it, owned by
// its old Unix user. A UID base that would give that uid to a new account's home is refused at
// start, so the new account can't reach the old one's files; one that gives it to none is taken.
func TestAUIDBaseIsNeverOneALegacyHomesOwnerWouldGetAgain(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}}, accounts: map[string]accountRecord{}}
	if err := mkdirDurable(w.accountsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.cfg.Layout.legacy(7), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := os.Getuid() // the directory's owner here: the test's own uid
	// A base under which the owner's uid is a home's (the tenth): refused, and nothing is pinned.
	if err := w.pinUIDBase(owner - firstHome - 10); err == nil {
		t.Error("a UID base that gives a legacy home's owner to a new account was accepted")
	}
	if _, err := os.Stat(w.uidBaseFile()); err == nil {
		t.Error("the refused base was pinned")
	}
	// The last home's uid is its owner: refused too.
	if err := w.pinUIDBase(owner - (maxHome - 1)); err == nil {
		t.Error("a UID base whose last home is a legacy home's owner was accepted")
	}
	// A base whose homes all lie above the owner: taken.
	if err := w.pinUIDBase(owner + 1); err != nil {
		t.Errorf("a UID base apart from the legacy home's owner: %v", err)
	}
}
