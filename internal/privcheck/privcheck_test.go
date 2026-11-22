package privcheck_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/privcheck"
)

// fakeEscalator puts an executable called name in a new directory, exiting with code, and
// returns the directory.
func fakeEscalator(t *testing.T, name string, code int) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	return dir
}

func user(id int) func() int { return func() int { return id } }

func TestProblems(t *testing.T) {
	privcheck.SetStandardDirsForTest(t, nil)
	empty := t.TempDir()
	cases := []struct {
		name string
		p    privcheck.Process
		want []string // substrings, one per problem
	}{
		{"a plain user", privcheck.Process{UID: user(1000), EUID: user(1000), Path: empty}, nil},
		{"root", privcheck.Process{UID: user(0), EUID: user(0), Path: empty}, []string{"runs as root"}},
		{"setuid root", privcheck.Process{UID: user(1000), EUID: user(0), Path: empty}, []string{"runs as root"}},
		{"root doesn't ask sudo", privcheck.Process{UID: user(0), EUID: user(0), Path: fakeEscalator(t, "sudo", 0)}, []string{"runs as root"}},
		{"passwordless sudo", privcheck.Process{UID: user(1000), EUID: user(1000), Path: fakeEscalator(t, "sudo", 0)}, []string{"`sudo -n true`", "sudo -k"}},
		{"sudo that asks for a password", privcheck.Process{UID: user(1000), EUID: user(1000), Path: fakeEscalator(t, "sudo", 1)}, nil},
		{"sudo with NOPASSWD for some commands", privcheck.Process{UID: user(1000), EUID: user(1000), Path: fakeSudoListing(t)}, []string{"`sudo -n -l`", "NOPASSWD"}},
		{"passwordless doas", privcheck.Process{UID: user(1000), EUID: user(1000), Path: fakeEscalator(t, "doas", 0)}, []string{"`doas -n true`"}},
		{"relative PATH entries are ignored", privcheck.Process{UID: user(1000), EUID: user(1000), Path: "."}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.Problems(context.Background())
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("problems = %q, want none", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems = %q, want %q in them", got, w)
				}
			}
		})
	}
}

func TestSetuidFiles(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "bin", "plain")
	setuid := filepath.Join(root, "bin", "setuid")
	skipped := filepath.Join(root, "homes", "u1", "mine")
	for _, f := range []string{plain, setuid, skipped} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil { //nolint:gosec // G306: an executable
			t.Fatal(err)
		}
	}
	for _, f := range []string{setuid, skipped} {
		if err := os.Chmod(f, 0o4755); err != nil { //nolint:gosec // G302: the setuid file under test
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(setuid); err != nil || info.Mode()&os.ModeSetuid == 0 {
		t.Skip("this file system doesn't keep the setuid bit")
	}
	got, unreadable, more, err := privcheck.SetuidFiles(root, 0, filepath.Join(root, "homes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != setuid || len(unreadable) != 0 || more {
		t.Errorf("setuid files = %q (unreadable %q, more %t), want only %s", got, unreadable, more, setuid)
	}
	// A limit says there were more.
	if got, _, more, _ := privcheck.SetuidFiles(root, 1, filepath.Join(root, "homes")); len(got) != 1 || more {
		t.Errorf("limit 1 with one file: %q, more %t", got, more)
	}
	if err := os.Chmod(setuid, 0o755); err != nil { //nolint:gosec // G302: an executable
		t.Fatal(err)
	}
	if got, _, _, err := privcheck.SetuidFiles(root, 0, filepath.Join(root, "homes")); err != nil || len(got) != 0 {
		t.Errorf("after chmod: %q, %v", got, err)
	}

	// A directory it can't read is reported: it could hide one.
	if os.Geteuid() != 0 {
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a directory
		if _, unreadable, _, err := privcheck.SetuidFiles(root, 0); err != nil || len(unreadable) != 1 || unreadable[0] != locked {
			t.Errorf("unreadable = %q, %v", unreadable, err)
		}
	}
}

func TestEnforce(t *testing.T) {
	if err := privcheck.Enforce("the agent sidecar", nil, false); err != nil {
		t.Errorf("no problems: %v", err)
	}
	err := privcheck.Enforce("the agent sidecar", []string{"it runs as root"}, false)
	if err == nil || !strings.Contains(err.Error(), "it runs as root") || !strings.Contains(err.Error(), "-"+privcheck.OverrideFlag) {
		t.Errorf("err = %v", err)
	}
	if err := privcheck.Enforce("the agent sidecar", []string{"it runs as root"}, true); err != nil {
		t.Errorf("with the override: %v", err)
	}
}

// sudo outside PATH, where an agent's command could still run it by its full path, counts.
func TestProblemsLookOutsidePath(t *testing.T) {
	privcheck.SetStandardDirsForTest(t, []string{fakeEscalator(t, "sudo", 0)})
	got := privcheck.Process{UID: user(1000), EUID: user(1000), Path: t.TempDir()}.Problems(context.Background())
	if len(got) != 1 || !strings.Contains(got[0], "`sudo -n true`") {
		t.Errorf("problems = %q", got)
	}
}

// fakeSudoListing is a sudo that refuses `-n true` but lists the user's rules without a
// password: a NOPASSWD rule for some commands only.
func fakeSudoListing(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$2\" = -l ] && exit 0\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	return dir
}
