//go:build unix

package cliworker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

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

func TestEveryHomeComesWithItsUID(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}, Isolation: &Isolation{UIDBase: 100000}}, accounts: map[string]accountRecord{}}
	if err := mkdirDurable(w.accountsDir()); err != nil {
		t.Fatal(err)
	}
	id := IdentityForTest(1)
	if err := w.retire(id, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.unretire(id, 1); err != nil {
		t.Fatal(err)
	}
	r, ok, err := w.find(id)
	if err != nil || !ok || r.Home == 0 || r.UID != 100000+r.Home {
		t.Fatalf("the account a census brought back: %+v %v %v, want a home and its uid", r, ok, err)
	}

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

func TestAUIDBaseIsNeverOneALegacyHomesOwnerWouldGetAgain(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}}, accounts: map[string]accountRecord{}}
	if err := mkdirDurable(w.accountsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.cfg.Layout.legacy(7), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := os.Getuid()
	if err := w.pinUIDBase(owner - firstHome - 10); err == nil {
		t.Error("a UID base that gives a legacy home's owner to a new account was accepted")
	}
	if _, err := os.Stat(w.uidBaseFile()); err == nil {
		t.Error("the refused base was pinned")
	}
	if err := w.pinUIDBase(owner - firstHome); err == nil {
		t.Error("a UID base whose first home is a legacy home's owner was accepted")
	}
	if err := w.pinUIDBase(owner - (maxHome - 1)); err == nil {
		t.Error("a UID base whose last home is a legacy home's owner was accepted")
	}
	if err := w.pinUIDBase(owner + 1); err != nil {
		t.Errorf("a UID base apart from the legacy home's owner: %v", err)
	}
}

func TestAVolumeThatLostItsAuthorityIsPinnedByNoRequest(t *testing.T) {
	w := &Worker{cfg: Config{Layout: Layout{Root: t.TempDir()}}, authorityLost: "its installation is gone"}
	if err := mkdirDurable(w.stateDir()); err != nil {
		t.Fatal(err)
	}
	if err := w.sameInstallation(TestInstallation, true); !errors.Is(err, ErrAuthorityLost) {
		t.Errorf("a request to pin the volume: %v, want ErrAuthorityLost", err)
	}
	if _, err := os.Stat(w.installationFile()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused request wrote the installation: %v", err)
	}
}

func newLegacyWorker(t *testing.T) *Worker {
	t.Helper()
	w, err := New(Config{Layout: Layout{Root: filepath.Join(t.TempDir(), "cli")}, AdoptLegacy: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func failOn(t *testing.T, op, path string, err error) {
	t.Helper()
	faultForTest = func(o, p string) error {
		if o == op && filepath.Clean(p) == filepath.Clean(path) {
			return err
		}
		return nil
	}
	t.Cleanup(func() { faultForTest = nil })
}

func TestAHomeScanThatCannotLookReturnsTheErrorInsteadOfGivingEveryNumberAway(t *testing.T) {
	w := newLegacyWorker(t)
	w.nextFree = maxHome - 3
	broken := errors.New("input/output error")
	faultForTest = func(op, _ string) error {
		if op == "lstat" {
			return broken
		}
		return nil
	}
	t.Cleanup(func() { faultForTest = nil })
	if _, err := w.nextHome(); !errors.Is(err, broken) {
		t.Errorf("a scan whose lookups failed: %v, want the lookup's error", err)
	}
}

func TestALegacyDirectoryThatCannotBeLookedUpIsStillAdoptedOnceItCan(t *testing.T) {
	w := newLegacyWorker(t)
	a := AccountForTest(1)
	legacy := w.cfg.Layout.legacy(1)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	failOn(t, "lstat", legacy, errors.New("input/output error"))
	if _, err := w.resolve(a, true); err == nil {
		t.Fatal("an account was made while its legacy directory couldn't be looked up")
	}
	faultForTest = nil
	acct, err := w.resolve(a, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.Layout.Home(acct.home).Root, "keep")); err != nil {
		t.Errorf("the legacy directory's file after the lookup recovered: %v", err)
	}
}

func TestAnAdoptionWhoseLookupFailedKeepsItsMarker(t *testing.T) {
	w := newLegacyWorker(t)
	a := AccountForTest(1)
	legacy := w.cfg.Layout.legacy(1)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.adoptHookForTest = func() error { return errors.New("stopped before the marker was cleared") }
	if _, err := w.resolve(a, true); err == nil {
		t.Fatal("fixture: the adoption didn't stop")
	}
	w.adoptHookForTest = nil
	r, _, _ := w.find(a.Identity)
	if r.Adopting != 1 {
		t.Fatalf("fixture: the marker = %d", r.Adopting)
	}
	if err := os.Rename(w.cfg.Layout.Home(r.Home).Root, legacy+"-aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(legacy+"-aside", legacy); err != nil {
		t.Fatal(err)
	}
	failOn(t, "lstat", legacy, errors.New("input/output error"))
	if _, err := w.resolve(a, true); err == nil {
		t.Fatal("the adoption went on with a legacy directory it couldn't look up")
	}
	if r, _, _ := w.find(a.Identity); r.Adopting != 1 {
		t.Errorf("the marker after a failed lookup = %d, want 1", r.Adopting)
	}
}

func TestAnAdoptionSyncsTheMoveBeforeItClearsItsMarkerEvenWhenItWasVisibleAlready(t *testing.T) {
	w := newLegacyWorker(t)
	a := AccountForTest(1)
	legacy := w.cfg.Layout.legacy(1)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	failOn(t, "sync", w.cfg.Layout.Root, errors.New("input/output error"))
	if _, err := w.resolve(a, true); err == nil {
		t.Fatal("fixture: the sync of the move didn't fail")
	}
	faultForTest = nil
	var synced []string
	syncedForTest = func(p string) { synced = append(synced, filepath.Clean(p)) }
	t.Cleanup(func() { syncedForTest = nil })
	if _, err := w.resolve(a, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, filepath.Clean(w.cfg.Layout.Root)) {
		t.Errorf("the retry cleared the marker without syncing %s (synced %v)", w.cfg.Layout.Root, synced)
	}
	if r, _, _ := w.find(a.Identity); r.Adopting != 0 {
		t.Errorf("the marker after the retry = %d", r.Adopting)
	}
}

func TestAnAccountRecordWhoseSyncFailedIsNotUsedUntilASyncSucceeds(t *testing.T) {
	w := newLegacyWorker(t)
	a := AccountForTest(1)
	broken := errors.New("input/output error")
	failOn(t, "sync", w.accountsDir(), broken)
	if _, err := w.resolve(a, true); !errors.Is(err, broken) {
		t.Fatalf("fixture: the first resolve = %v, want the sync's error", err)
	}
	if _, err := os.Stat(w.accountFile(a.Identity)); err != nil {
		t.Fatalf("fixture: the record isn't visible after the failed sync: %v", err)
	}
	if got, err := w.resolve(a, true); err == nil {
		t.Fatalf("the retry used home %d though no sync of the record's directory succeeded", got.home)
	} else if !errors.Is(err, broken) {
		t.Errorf("the retry = %v, want the sync's error", err)
	}
	if _, _, err := w.find(a.Identity); !errors.Is(err, broken) {
		t.Errorf("a lookup of the record = %v, want the sync's error", err)
	}
	faultForTest = nil
	var synced []string
	syncedForTest = func(p string) { synced = append(synced, filepath.Clean(p)) }
	t.Cleanup(func() { syncedForTest = nil })
	got, err := w.resolve(a, true)
	if err != nil {
		t.Fatalf("the resolve once the sync works: %v", err)
	}
	if !slices.Contains(synced, filepath.Clean(w.accountsDir())) {
		t.Errorf("the record was admitted without a sync of %s (synced %v)", w.accountsDir(), synced)
	}
	if got.home < firstHome {
		t.Errorf("home = %d", got.home)
	}
}
