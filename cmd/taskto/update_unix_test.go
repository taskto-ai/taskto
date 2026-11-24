//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An update killed after it has replaced .env (a crash, a reboot, a full disk) is found by the
// next run, not mistaken for an install that is up to date: nothing runs again until a
// person has looked, whatever it is asked, and the record says what was happening.
func TestUpdateKilledHalfWayIsFoundAndBlocks(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/block-up", "") // `compose up` of the new version waits, for ever
	cmd := e.command()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	for deadline := time.Now().Add(30 * time.Second); e.count(`^compose up`) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatal("the update never got to `compose up`")
		}
	}
	// .env is the new version's and the old server still runs: the state to be found.
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
	// A person who removes the record and leaves the .env as it is is stopped by the second
	// defense: the server isn't the version the .env says.
	if err := os.Remove(filepath.Join(e.state, "transaction")); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("blocked", "the server runs '"+updOld+"' but")
}
