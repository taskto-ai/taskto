//go:build unix

package lockfile

// Tests with other processes run this test binary again: with TASKTO_LOCKFILE_HOLD set it
// takes that lock, says so on stdout and holds it until its stdin closes; with
// TASKTO_LOCKFILE_WAIT set it only waits for its stdin to close.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if path := os.Getenv("TASKTO_LOCKFILE_HOLD"); path != "" {
		f, err := Lock(path)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer f.Close()
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if os.Getenv("TASKTO_LOCKFILE_WAIT") != "" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	os.Exit(m.Run())
}

// child starts this test binary with env and returns it once it has started; closing its
// stdin ends it.
func child(t *testing.T, env string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe) //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, stdin, bufio.NewReader(stdout)
}

// lockOK takes the lock at path and fails the test if it can't; the test releases it.
func lockOK(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The holder's PID is in the file, and in the error another taker gets. The lock is per
// open file, so even a second Lock in the same process is refused.
func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	f := lockOK(t, path)
	if b, err := os.ReadFile(path); err != nil || string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock file = %q, %v; want this process's PID", b, err)
	}
	_, err := Lock(path)
	var held *HeldError
	if !errors.As(err, &held) || held.PID != os.Getpid() || !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock = %v, want a HeldError with this PID", err)
	}
	// Released on close.
	f.Close()
	lockOK(t, path)
}

// A second process is refused, with the holder's PID. Once the holder dies without
// releasing anything, its lock is gone though its file, and its PID in it, are still there.
func TestAnotherProcessIsRefusedUntilItDies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	holder, _, out := child(t, "TASKTO_LOCKFILE_HOLD="+path)
	if line, err := out.ReadString('\n'); line != "locked\n" {
		t.Fatalf("holder said %q, %v", line, err)
	}

	_, err := Lock(path)
	var held *HeldError
	if !errors.As(err, &held) || held.PID != holder.Process.Pid {
		t.Fatalf("Lock while another process holds it = %v, want a HeldError with PID %d", err, holder.Process.Pid)
	}
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(holder.Process.Pid)) {
		t.Fatalf("error %q doesn't name the holder's PID", err)
	}

	// A crash: SIGKILL, no cleanup. The stale file is reused.
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the dead holder's lock file: %v", err)
	}
	lockOK(t, path)
	if b, _ := os.ReadFile(path); string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock file = %q, want this process's PID only", b)
	}
}

// A child process started while the lock is held doesn't inherit it: once the parent
// lets go, the lock is free although the child still runs.
func TestLockIsNotInherited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	f := lockOK(t, path)
	waiter, stdin, _ := child(t, "TASKTO_LOCKFILE_WAIT=1")
	f.Close()
	lockOK(t, path)
	_ = stdin.Close()
	if err := waiter.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
}

func TestLockDoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if f, err := Lock(path); err == nil {
		f.Close()
		t.Fatal("Lock followed a symlink")
	}
}

func TestDataDirSaysWhoHoldsIt(t *testing.T) {
	dir := t.TempDir()
	f, err := DataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Stat(filepath.Join(dir, DataDirName)); err != nil {
		t.Fatal(err)
	}
	_, err = DataDir(dir)
	want := fmt.Sprintf("data directory %s is in use by another taskto process (pid %d)", dir, os.Getpid())
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("second DataDir = %v, want %q", err, want)
	}
	// Matchable, not only readable.
	if held := (*HeldError)(nil); !errors.Is(err, ErrLocked) || !errors.As(err, &held) || held.PID != os.Getpid() {
		t.Fatalf("second DataDir = %v: want errors.Is ErrLocked and a *HeldError naming pid %d", err, os.Getpid())
	}
}

func TestAFileReplacedWhileLockingIsLockedAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	// Another process removes the file and makes a new one between our open and our lock.
	replaced := false
	afterOpen = func() {
		if replaced {
			return
		}
		replaced = true
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { afterOpen = nil }()
	f, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	afterOpen = nil
	// The lock is on the file the path names, so nobody else gets it.
	if g, err := Lock(path); !errors.Is(err, ErrLocked) {
		if g != nil {
			g.Close()
		}
		t.Fatalf("second Lock after a replacement = %v, want ErrLocked", err)
	}
}
