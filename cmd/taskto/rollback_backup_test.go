package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rollbackScript(t *testing.T) string {
	t.Helper()
	guide := readRepoFile(t, "docs/install.md")
	start := strings.Index(guide, "HOMES=0   # 1: back")
	if start < 0 {
		t.Fatal("the rollback's script isn't in docs/install.md")
	}
	end := strings.Index(guide[start:], "\nEOF\n")
	if end < 0 {
		t.Fatal("the rollback's script has no end")
	}
	return strings.Replace(guide[start:start+end+len("\nEOF\n")], "HOMES=0", "HOMES=1", 1)
}

type rollbackRun struct {
	dir   string
	calls string
	out   string
	err   error
}

func runRollback(t *testing.T, backupCommand string, files map[string]string) rollbackRun {
	t.Helper()
	for _, prog := range []string{"bash", "gzip"} {
		if _, err := exec.LookPath(prog); err != nil {
			t.Skip("needs " + prog)
		}
	}
	script := rollbackScript(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\necho \"$*\" >> \"$FAKE_STATE/calls\"\ncase \"$*\" in\n*'taskto backup -'*)\n" + backupCommand + "\n;;\nesac\nexit 0\n"
	for _, prog := range []string{"docker", "git"} {
		if err := os.WriteFile(filepath.Join(bin, prog), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files["before-v0.2.0.tar.gz"] = ""
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_STATE="+dir)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return rollbackRun{dir: dir, calls: string(calls), out: string(out), err: err}
}

func (r rollbackRun) mustStopBeforeTeardown(t *testing.T) {
	t.Helper()
	if r.err == nil {
		t.Fatal("the script went on")
	}
	if strings.Contains(r.calls, "volume rm") {
		t.Errorf("the data volume was removed:\n%s", r.calls)
	}
	if strings.Contains(r.calls, " down") {
		t.Errorf("the deployment was stopped:\n%s", r.calls)
	}
}

func TestARollbackStopsBeforeChangingAnythingWhenTheUpgradedBackupExists(t *testing.T) {
	kept := "kept-verified-backup"
	r := runRollback(t, "echo newer-data | gzip -c", map[string]string{"after-v0.2.0.tar.gz": kept})
	r.mustStopBeforeTeardown(t)
	if strings.Contains(r.calls, "taskto backup") {
		t.Errorf("the script took a backup though one exists:\n%s", r.calls)
	}
	if got, _ := os.ReadFile(filepath.Join(r.dir, "after-v0.2.0.tar.gz")); string(got) != kept {
		t.Errorf("the existing backup changed: %q", got)
	}
	if !strings.Contains(r.out, "already exists") {
		t.Errorf("the script didn't tell the operator: %q", r.out)
	}
	if others, _ := filepath.Glob(filepath.Join(r.dir, "after-v0.2.0*")); len(others) != 1 {
		t.Errorf("the script made another file: %v", others)
	}
}

func TestARollbackStopsWhenTheBackupCommandWritesInvalidGzip(t *testing.T) {
	r := runRollback(t, "echo not-gzip", map[string]string{})
	r.mustStopBeforeTeardown(t)
	if _, err := os.Stat(filepath.Join(r.dir, "after-v0.2.0.tar.gz")); err == nil {
		t.Error("an invalid backup was put in place")
	}
}

func TestARollbackStopsWhenTheBackupCommandWritesNothing(t *testing.T) {
	r := runRollback(t, "exit 0", map[string]string{})
	r.mustStopBeforeTeardown(t)
	if _, err := os.Stat(filepath.Join(r.dir, "after-v0.2.0.tar.gz")); err == nil {
		t.Error("an empty backup was put in place")
	}
}

func TestARollbackStopsWhenTheRenameIntoPlaceDidNothing(t *testing.T) {
	r := runRollback(t, "echo newer-data | gzip -c; echo older-data | gzip -c > after-v0.2.0.tar.gz", map[string]string{})
	r.mustStopBeforeTeardown(t)
	got, err := exec.Command("gzip", "-dc", filepath.Join(r.dir, "after-v0.2.0.tar.gz")).Output()
	if err != nil || string(got) != "older-data\n" {
		t.Errorf("the valid backup that appeared meanwhile changed: %q, %v", got, err)
	}
}

func TestARollbackStopsWhenADirectoryAppearsWhereTheBackupGoes(t *testing.T) {
	r := runRollback(t, "echo newer-data | gzip -c; mkdir after-v0.2.0.tar.gz", map[string]string{})
	r.mustStopBeforeTeardown(t)
	if info, err := os.Stat(filepath.Join(r.dir, "after-v0.2.0.tar.gz")); err != nil || !info.IsDir() {
		t.Errorf("the directory that appeared meanwhile changed: %v", err)
	}
}

func TestARollbackWithAValidBackupPublishesItAndGoesOn(t *testing.T) {
	r := runRollback(t, "echo newer-data | gzip -c", map[string]string{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if err := exec.Command("gzip", "-t", filepath.Join(r.dir, "after-v0.2.0.tar.gz")).Run(); err != nil {
		t.Errorf("the backup is damaged: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*.partial")); len(left) != 0 {
		t.Errorf("a temporary file is left: %v", left)
	}
	if !strings.Contains(r.calls, "volume rm") {
		t.Errorf("the rollback didn't go on:\n%s", r.calls)
	}
}
