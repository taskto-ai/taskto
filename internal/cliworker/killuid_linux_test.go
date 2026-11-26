//go:build linux

package cliworker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Only a full look at the processes says none of a uid is left, before its number's directory goes
// and the number may be given to someone else: a list of the processes that can't be made, or a
// process whose state can't be read, is an error, not "none left".
func TestKillUIDCountsOnlyAFullLookAsNoneLeft(t *testing.T) {
	old := procRoot
	t.Cleanup(func() { procRoot = old })
	const uid = 1234567

	procRoot = filepath.Join(t.TempDir(), "gone")
	if err := killUID(uid, 100*time.Millisecond); err == nil {
		t.Error("with no list of the processes, none counted as left")
	}

	procRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "4242", "status"), 0o700); err != nil { // can't be read as a file
		t.Fatal(err)
	}
	if err := killUID(uid, 100*time.Millisecond); err == nil {
		t.Error("a process whose state couldn't be read counted as none left")
	}

	if err := os.RemoveAll(filepath.Join(procRoot, "4242")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(procRoot, "4243"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "4243", "status"), []byte("Name:\tsleep\nState:\tS (sleeping)\nUid:\t1000\t1000\t1000\t1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := killUID(uid, 100*time.Millisecond); err != nil {
		t.Errorf("a full look with nothing of the uid: %v", err)
	}
}

// A uid range that would reach the 32-bit nobody is refused when the worker starts, so every
// user's uid is valid wherever it is used.
func TestUIDOfStaysInRange(t *testing.T) {
	iso := &Isolation{UIDBase: 100000}
	for _, user := range []int64{0, -1, maxIsolatedUser} {
		if _, err := iso.uidOf(user); err == nil {
			t.Errorf("user %d was given a uid", user)
		}
	}
	if uid, err := iso.uidOf(5); err != nil || uid != 100005 {
		t.Errorf("uidOf(5) = %d %v", uid, err)
	}
}
