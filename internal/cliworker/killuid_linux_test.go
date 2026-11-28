//go:build linux

package cliworker

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestKillUIDCountsOnlyAFullLookAsNoneLeft(t *testing.T) {
	old := procRoot
	t.Cleanup(func() { procRoot = old })
	const uid = 1234567

	procRoot = filepath.Join(t.TempDir(), "gone")
	if err := killUID(uid, 100*time.Millisecond); err == nil {
		t.Error("with no list of the processes, none counted as left")
	}

	procRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "4242", "status"), 0o700); err != nil {
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

func TestUIDOfStaysInRange(t *testing.T) {
	iso := &Isolation{UIDBase: 100000}
	for _, home := range []int64{0, -1, 5, firstHome - 1, maxHome} {
		if _, err := iso.uidOf(home); err == nil {
			t.Errorf("home %d was given a uid", home)
		}
	}
	if uid, err := iso.uidOf(firstHome + 5); err != nil || uid != 100000+firstHome+5 {
		t.Errorf("uidOf(firstHome+5) = %d %v", uid, err)
	}
}

func TestTheStartUpSweepCountsOnlyAFullLook(t *testing.T) {
	old := procRoot
	t.Cleanup(func() { procRoot = old })
	procRoot = filepath.Join(t.TempDir(), "gone")
	if _, err := killUIDs(1234567); err == nil {
		t.Error("with no list of the processes, the sweep counted as done")
	}
	procRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "4242", "status"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := killUIDs(1234567); err == nil {
		t.Error("a process whose state couldn't be read was left out of the sweep")
	}
}

func TestAZombieLeaderWithLiveThreadsCounts(t *testing.T) {
	old := procRoot
	t.Cleanup(func() { procRoot = old })
	procRoot = t.TempDir()
	for pid, c := range map[int]struct {
		state, threads string
		zombie         bool
	}{
		4250: {"Z (zombie)", "1", true},
		4251: {"Z (zombie)", "3", false},
		4252: {"S (sleeping)", "1", false},
	} {
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		status := "Name:\tx\nState:\t" + c.state + "\nUid:\t1000\t1000\t1000\t1000\nThreads:\t" + c.threads + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
			t.Fatal(err)
		}
		uid, zombie, err := procStatus(pid)
		if err != nil || uid != 1000 || zombie != c.zombie {
			t.Errorf("%s with %s threads: uid %d, zombie %v, %v; want zombie %v", c.state, c.threads, uid, zombie, err, c.zombie)
		}
	}
}
