package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func lines(n int, prefix string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(prefix)
		b.WriteString(" ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	return b.String()
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

func (r *repo) count(base, head string) tally {
	r.t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chdir(r.dir); err != nil {
		r.t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			r.t.Fatal(err)
		}
	}()
	changes, err := diff(base, head)
	if err != nil {
		r.t.Fatal(err)
	}
	return count(changes)
}

func TestWhatMainLandedOnItsOwnDoesNotShrinkThePullRequest(t *testing.T) {
	r := newRepo(t)
	r.write("seed.go", "package seed\n")
	r.commit("seed")
	r.git("switch", "-q", "-c", "topic")
	r.write("shared.go", lines(810, "shared"))
	r.write("own.go", "own\n")
	r.commit("topic")
	r.git("switch", "-q", "main")
	r.write("shared.go", lines(810, "shared"))
	r.write("other.go", lines(100, "other"))
	r.commit("main")

	got := r.count("main", "topic")
	if got.production != 811 {
		t.Fatalf("production = %d, want 811 (the pull request's own changes, as GitHub shows them)", got.production)
	}
}

func TestMovingProductionCodeIntoATestDirectoryCountsItsDeletedLines(t *testing.T) {
	r := newRepo(t)
	r.write("web/src/old.ts", lines(1000, "keep")+lines(801, "drop"))
	r.commit("seed")
	r.git("switch", "-q", "-c", "topic")
	r.git("rm", "-q", "web/src/old.ts")
	r.write("web/e2e/new.ts", lines(1000, "keep"))
	r.commit("move")

	got := r.count("main", "topic")
	if got.production != 801 {
		t.Fatalf("production = %d, want the 801 lines deleted from web/src/old.ts", got.production)
	}
}

func TestAnEmptyDiffCountsNothing(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "a\n")
	r.commit("seed")
	r.git("switch", "-q", "-c", "topic")
	r.commit("empty")

	got := r.count("main", "topic")
	if got.production != 0 || got.other != 0 {
		t.Fatalf("got %+v, want nothing counted", got)
	}
}

func TestTestOnlyFilesAreNotCounted(t *testing.T) {
	for _, name := range []string{
		"internal/x/x_test.go",
		"web/src/a.test.ts",
		"web/src/a.test.tsx",
		"internal/x/testdata/f.json",
		"web/e2e/app/a.e2e.mjs",
		"internal/cliworker/clitest/fake.go",
		"internal/audit/audittest/check.go",
		"internal/integrations/testprovider/stripe.go",
		"web/src/test/fakes.ts",
		"agent/src/test-entry.ts",
		"deploy/worker-network-test.sh",
		"web/src/i18n/locales/pt-BR.json",
		"web/src/api.gen.ts",
		"pnpm-lock.yaml",
		"docs/api.md",
	} {
		if counted(name) {
			t.Errorf("%s is counted as production code", name)
		}
	}
	for _, name := range []string{
		"internal/apps/store.go",
		"web/src/App.tsx",
		"agent/src/test-hooks.ts",
		"internal/agenttools/tools.json",
		"internal/apps/templates/invoicing.json",
		".github/workflows/ci.yml",
	} {
		if !counted(name) {
			t.Errorf("%s is not counted as production code", name)
		}
	}
}

func TestParseNumstatReadsRenamesBinariesAndUnusualNames(t *testing.T) {
	out := []byte("2\t0\ta space.go\x00" +
		"2\t1\ta\ttab.go\x00" +
		"1\t0\t\x00old name.go\x00renamed\tfile\nname.go\x00" +
		"-\t-\tbinary.dat\x00" +
		"-\t-\t\x00binary-rename.dat\x00moved-binary.dat\x00" +
		"10\t0\tlast.go\x00")
	got, err := parseNumstat(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{
		{oldName: "a space.go", newName: "a space.go", added: 2},
		{oldName: "a\ttab.go", newName: "a\ttab.go", added: 2, deleted: 1},
		{oldName: "old name.go", newName: "renamed\tfile\nname.go", added: 1},
		{oldName: "last.go", newName: "last.go", added: 10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestOnlyRevisionsReachGit(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "a\n")
	r.commit("seed")
	for _, bad := range []string{"--output=/tmp/x", "-p", "main..HEAD", "", "a b"} {
		if _, err := diff(bad, "main"); err == nil {
			t.Errorf("base %q was given to git", bad)
		}
		if _, err := diff("main", bad); err == nil {
			t.Errorf("head %q was given to git", bad)
		}
	}
}

func TestTheLimitAndTheException(t *testing.T) {
	at := tally{production: limit}
	over := tally{production: limit + 1}
	if v := judge(at, false); !v.pass {
		t.Errorf("%d lines failed: %s", limit, v.message)
	}
	if v := judge(over, false); v.pass || !strings.HasPrefix(v.message, "::error") {
		t.Errorf("%d lines without the label: %+v, want a failure", limit+1, v)
	}
	if v := judge(over, true); !v.pass || !strings.HasPrefix(v.message, "::warning") {
		t.Errorf("%d lines with the label: %+v, want a pass with a warning", limit+1, v)
	}
}
