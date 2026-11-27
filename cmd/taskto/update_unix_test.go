//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startGroup(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return done
}

func TestStartGroupReapsTheGroupOfATestThatStopsEarly(t *testing.T) {
	t.Parallel()
	var pid int
	t.Run("stops at once", func(t *testing.T) {
		cmd := exec.Command("sleep", "60")
		startGroup(t, cmd)
		pid = cmd.Process.Pid
	})
	if err := syscall.Kill(-pid, 0); err == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Error("the process group of the test is still there")
	}
}

func TestStartGroupReapsAChildTheLeaderLeftBehind(t *testing.T) {
	t.Parallel()
	var child int
	t.Run("leader exits first", func(t *testing.T) {
		var out bytes.Buffer
		cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
		cmd.Stdout = &out
		<-startGroup(t, cmd)
		n, err := strconv.Atoi(strings.TrimSpace(out.String()))
		if err != nil {
			t.Fatalf("the leader's child: %q: %v", out.String(), err)
		}
		child = n
		if err := syscall.Kill(child, 0); err != nil {
			t.Fatalf("the child isn't running before the cleanup: %v", err)
		}
	})
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(child, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(child, syscall.SIGKILL)
			t.Fatal("the child the leader left in its group is still there")
		}
	}
}

func TestUpdateKilledHalfWayIsFoundAndBlocks(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/block-up", "")
	cmd := e.command()
	done := startGroup(t, cmd)
	for deadline := time.Now().Add(30 * time.Second); e.count(`^compose start`) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatal("the update never got to `compose start`")
		}
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updNew+"\n") {
		t.Fatal(".env wasn't replaced before the start")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-done
	_ = os.Remove(filepath.Join(e.fake, "block-up"))
	if err := os.RemoveAll(e.tmp); err != nil {
		t.Fatal(err)
	}
	e.sub("tmp")

	var tx map[string]any
	if err := jsonUnmarshal(e.read("state/transaction"), &tx); err != nil {
		t.Fatal(err)
	}
	if tx["from"] != updOld || tx["to"] != updNew || tx["phase"] != "starting" || tx["backup"] == "" || tx["previous"] == "" {
		t.Fatalf("the record = %v", tx)
	}
	calls := len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}, {"--dry-run"}} {
		code, out := e.run(args...)
		if code != 1 || !strings.Contains(out, "result=interrupted") {
			t.Fatalf("the run after the kill (%v): exit %d\n%s", args, code, out)
		}
		if len(args) == 0 || args[0] != "--dry-run" {
			if st := e.wantStatus("interrupted", "was interrupted in phase starting"); st["result"] == "up-to-date" {
				t.Fatal("reported up to date")
			}
		}
		if len(e.calls()) != calls {
			t.Fatalf("docker was called by a run that is blocked\n%s", strings.Join(e.calls(), "\n"))
		}
	}
	e.setRunning(updOld)
	if err := os.Remove(filepath.Join(e.state, "transaction")); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("blocked", "the server runs '"+updOld+"' but")
}

func TestUpdateKilledAfterTheRevertIsBlockedAndNotRetried(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.failCalls(`^compose start .* @` + updNew + `$`)
	e.write("fake/kill-on", "^compose exec -T cli-worker true @"+updOld+"$\n")
	cmd := e.command()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the update wasn't killed")
		}
	case <-time.After(60 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("the update never got to the old version's checks")
	}
	if err := os.RemoveAll(e.tmp); err != nil {
		t.Fatal(err)
	}
	e.sub("tmp")
	if !e.exists("state/transaction") || e.exists("state/failed") {
		t.Fatal("the run was killed after its bookkeeping, not before")
	}
	e.wantOldFilesBack()
	calls := len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}} {
		code, out := e.run(args...)
		if code != 1 || !strings.Contains(out, "result=interrupted") || len(e.calls()) != calls {
			t.Fatalf("the run after the kill (%v): exit %d, calls %d -> %d\n%s", args, code, calls, len(e.calls()), out)
		}
	}
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block: exit %d\n%s", code, out)
	}
	ups, backups := e.count(`^compose start`), e.count(`^compose exec -T taskto taskto backup`)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("the run after clearing: exit %d", code)
	}
	e.wantStatus("skipped", "failed here before")
	if e.count(`^compose start`) != ups || e.count(`^compose exec -T taskto taskto backup`) != backups {
		t.Error("the release that failed was tried again")
	}
}
