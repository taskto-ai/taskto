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

func TestARolledBackUpgradeNeverTruncatesItsVerifiedBackupOnARerun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("needs gzip")
	}
	script := rollbackScript(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
case "$*" in
*'taskto backup -'*)
  if [ -e "$FAKE_STATE/down" ]; then exit 2; fi
  echo upgraded-data | gzip -c
  ;;
*' down'*) touch "$FAKE_STATE/down" ;;
*' build'*) exit 3 ;;
esac
exit 0
`
	for _, prog := range []string{"docker", "git"} {
		if err := os.WriteFile(filepath.Join(bin, prog), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "before-v0.2.0.tar.gz"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() error {
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_STATE="+dir)
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		return err
	}
	backup := filepath.Join(dir, "after-v0.2.0.tar.gz")
	if err := run(); err == nil {
		t.Fatal("the first run succeeded though the build failed")
	}
	first, err := os.ReadFile(backup)
	if err != nil || len(first) == 0 {
		t.Fatalf("the first run left no verified backup (%v, %d bytes)", err, len(first))
	}
	if err := run(); err == nil {
		t.Fatal("the rerun succeeded against a stopped deployment")
	}
	again, err := os.ReadFile(backup)
	if err != nil || string(again) != string(first) {
		t.Fatalf("the rerun changed the verified backup (%v): %d bytes, was %d", err, len(again), len(first))
	}
	if err := exec.Command("gzip", "-t", backup).Run(); err != nil {
		t.Errorf("the kept backup is damaged: %v", err)
	}
}

func TestARolledBackUpgradeRerunWithTheServerUpWritesBesideTheKeptBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("needs gzip")
	}
	script := rollbackScript(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\ncase \"$*\" in *'taskto backup -'*) echo newer-data | gzip -c ;; esac\nexit 0\n"
	for _, prog := range []string{"docker", "git"} {
		if err := os.WriteFile(filepath.Join(bin, prog), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kept := []byte("kept-verified-backup")
	for name, body := range map[string][]byte{"after-v0.2.0.tar.gz": kept, "before-v0.2.0.tar.gz": nil} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "after-v0.2.0.tar.gz")); string(got) != string(kept) {
		t.Errorf("the kept backup was written over: %q", got)
	}
	beside, _ := filepath.Glob(filepath.Join(dir, "after-v0.2.0-*.tar.gz"))
	if len(beside) != 1 {
		t.Fatalf("the new backup beside the kept one = %v", beside)
	}
	if err := exec.Command("gzip", "-t", beside[0]).Run(); err != nil {
		t.Errorf("the new backup is damaged: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(left) != 0 {
		t.Errorf("a temporary file is left: %v", left)
	}
}
