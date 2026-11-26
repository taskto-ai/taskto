//go:build linux

package cliworker_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// The container mode's OS boundary: each user's CLIs run as a Unix user of their own. These
// tests need root on Linux (CI runs them in a container, see .github/workflows/ci.yml);
// elsewhere they are skipped.

const isoBase = 200000

type isoHarness struct {
	w      *cliworker.Worker
	cfg    cliworker.Config
	layout cliworker.Layout
}

func newIsolated(t *testing.T) *isoHarness {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the isolation tests need root (Linux); CI runs them in a container")
	}
	// Everything the users' processes run must be reachable by them: a directory others can
	// pass through, a copy of this test binary as the helper and the fake CLIs.
	base := t.TempDir()
	for d := base; d != "/" && d != os.TempDir(); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(bin, "taskto-test")
	copyFile(t, helper)
	bins := map[cliworker.CLI]string{}
	for _, cli := range cliworker.CLIs {
		path := filepath.Join(bin, string(cli))
		if err := os.Symlink(helper, path); err != nil {
			t.Fatal(err)
		}
		bins[cli] = path
	}
	accounts := filepath.Join(base, "etc")
	if err := os.Mkdir(accounts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"passwd", "group"} {
		if err := os.WriteFile(filepath.Join(accounts, f), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := cliworker.Config{
		Layout:    cliworker.Layout{Root: filepath.Join(base, "homes")},
		Bins:      bins,
		Helper:    helper,
		Path:      "/usr/bin:/bin",
		StopGrace: time.Second,
		Isolation: &cliworker.Isolation{UIDBase: isoBase, UserFile: filepath.Join(accounts, "passwd"), Group: filepath.Join(accounts, "group")},
	}
	w, err := cliworker.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return &isoHarness{w: w, cfg: cfg, layout: cfg.Layout}
}

func copyFile(t *testing.T, to string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// signIn connects user's fake cli the way its login would: as the user, through the helper.
func (h *isoHarness) signIn(t *testing.T, user int64, cli cliworker.CLI) {
	t.Helper()
	l, err := h.w.StartLogin(user, cli, "")
	if err != nil {
		t.Fatal(err)
	}
	if cli == cliworker.Claude {
		if err := h.w.SubmitLoginCode(user, cli, "", clitest.GoodCode); err != nil {
			t.Fatal(err)
		}
	} else {
		approveAs(t, h.layout.User(user).Config(cli), isoBase+int(user))
	}
	select {
	case <-l.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the login didn't end")
	}
	if info := l.Info(); info.State != cliworker.LoginConnected {
		t.Fatalf("login = %+v", info)
	}
}

// approveAs approves a device login as the user would, writing the fake's file as its owner.
func approveAs(t *testing.T, configDir string, uid int) {
	t.Helper()
	clitest.Approve(t, configDir)
	if err := os.Chown(filepath.Join(configDir, "device-verdict"), uid, uid); err != nil {
		t.Fatal(err)
	}
}

// scenario writes the fake's scenario for user as the user's own file.
func (h *isoHarness) scenario(t *testing.T, user int64, cli cliworker.CLI, s clitest.Scenario) {
	t.Helper()
	dir := h.layout.User(user).Config(cli)
	clitest.WriteScenario(t, dir, s)
	if err := os.Chown(filepath.Join(dir, "scenario.json"), isoBase+int(user), isoBase+int(user)); err != nil {
		t.Fatal(err)
	}
}

func TestIsolationEachUserIsAUnixUser(t *testing.T) {
	h := newIsolated(t)
	for _, cli := range cliworker.CLIs {
		h.signIn(t, 1, cli)
		h.scenario(t, 1, cli, clitest.Scenario{Probes: []string{"uid", "caps", "nnp"}})
		events := run(t, h.w, spec("iso-"+string(cli), 1, cli, "hi"))
		done := last(events)
		if done.Status != cliworker.StatusOK {
			t.Fatalf("%s: %+v", cli, done)
		}
		if got := probeResult(done.Text, "uid"); got != fmt.Sprintf("ok %d %d", isoBase+1, isoBase+1) {
			t.Errorf("%s ran as %s", cli, got)
		}
		// No capabilities: nothing to escalate with.
		if got := probeResult(done.Text, "caps"); !strings.Contains(got, "CapPrm: 0000000000000000") || !strings.Contains(got, "CapEff: 0000000000000000") {
			t.Errorf("%s capabilities: %s", cli, got)
		}
		// no_new_privs: a setuid program couldn't give it any either.
		if got := probeResult(done.Text, "nnp"); got != "ok 1" {
			t.Errorf("%s no_new_privs: %s", cli, got)
		}
	}
	// The user exists, and their directory is theirs alone.
	passwd, err := os.ReadFile(h.cfg.Isolation.UserFile)
	if err != nil || !strings.Contains(string(passwd), fmt.Sprintf("tt1:x:%d:%d:", isoBase+1, isoBase+1)) {
		t.Fatalf("passwd = %q %v", passwd, err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(h.layout.User(1).Root, &st); err != nil || int(st.Uid) != isoBase+1 || st.Mode&0o777 != 0o700 {
		t.Fatalf("user directory: uid %d mode %o (%v)", st.Uid, st.Mode&0o777, err)
	}
	if err := syscall.Lstat(h.layout.Root, &st); err != nil || st.Uid != 0 || st.Mode&0o777 != 0o711 {
		t.Fatalf("layout root: uid %d mode %o (%v)", st.Uid, st.Mode&0o777, err)
	}
}

// What the spike saw Codex do (cat another user's file) fails here: one user's CLI can't read,
// list, write or signal anything of another user's, nor list the users.
func TestIsolationBetweenUsers(t *testing.T) {
	h := newIsolated(t)
	h.signIn(t, 1, cliworker.Claude)
	h.signIn(t, 2, cliworker.Codex)
	one := h.layout.User(1)
	secret := filepath.Join(one.Config(cliworker.Claude), ".credentials.json")
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("user 1's login: %v", err)
	}

	// User 1 runs something long, so there is a process to signal.
	h.scenario(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 20_000})
	long, _, err := h.w.Start(spec("long", 1, cliworker.Claude, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	var victim int
	eventually(t, "user 1's CLI didn't start", func() bool {
		calls := clitest.Calls(t, one.Config(cliworker.Claude))
		if len(calls) > 0 {
			victim = calls[len(calls)-1].PID
		}
		return victim != 0
	})

	h.scenario(t, 2, cliworker.Codex, clitest.Scenario{Probes: []string{
		"read:" + secret,
		"list:" + one.Root,
		"list:" + h.layout.Root,
		"write:" + filepath.Join(one.Work, "planted"),
		"kill:" + strconv.Itoa(victim),
		"read:" + h.cfg.Isolation.UserFile,
	}})
	done := last(run(t, h.w, spec("probe", 2, cliworker.Codex, "look around")))
	if done.Status != cliworker.StatusOK {
		t.Fatalf("user 2's turn: %+v", done)
	}
	for _, p := range []string{"read:" + secret, "list:" + one.Root, "list:" + h.layout.Root, "write:" + filepath.Join(one.Work, "planted")} {
		if got := probeResult(done.Text, p); !strings.Contains(got, "permission denied") {
			t.Errorf("user 2's %s: %s", p, got)
		}
	}
	if got := probeResult(done.Text, "kill:"+strconv.Itoa(victim)); !strings.Contains(got, "operation not permitted") {
		t.Errorf("user 2 signalled user 1's CLI: %s", got)
	}
	// The account files are public, as on any system: they hold no secrets.
	if got := probeResult(done.Text, "read:"+h.cfg.Isolation.UserFile); !strings.HasPrefix(got, "ok") {
		t.Errorf("reading passwd: %s", got)
	}
	// User 1's CLI is untouched.
	h.w.Stop("long")
	if d := last(waitDone(t, long, 15*time.Second)); d.Status != cliworker.StatusStopped {
		t.Errorf("user 1's turn: %+v", d)
	}
}

// A new worker kills every process of every CLI user before it does anything: a uid, unlike
// a PID, can't belong to something else.
func TestIsolationNewWorkerKillsEveryCLIProcess(t *testing.T) {
	h := newIsolated(t)
	h.signIn(t, 1, cliworker.Grok)
	h.scenario(t, 1, cliworker.Grok, clitest.Scenario{SleepMS: 60_000, Background: true})
	turn, _, err := h.w.Start(spec("orphan", 1, cliworker.Grok, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.layout.User(1).Config(cliworker.Grok)
	eventually(t, "the CLI didn't start its tool", func() bool {
		_, err := os.Stat(filepath.Join(cfg, "background.pid"))
		return err == nil
	})
	bg := backgroundPID(t, cfg)
	cli := clitest.Calls(t, cfg)[0].PID

	second, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	eventually(t, "a CLI user's process survived the new worker", func() bool { return !alive(cli) && !alive(bg) })
	if d := last(waitDone(t, turn, 15*time.Second)); d.Status == cliworker.StatusOK {
		t.Errorf("the killed turn finished: %+v", d)
	}
}

// A user's directory that isn't theirs (a symlink planted before the worker made it, or
// another user's) is refused, not followed.
func TestIsolationRefusesAForeignDirectory(t *testing.T) {
	h := newIsolated(t)
	if err := os.Symlink("/etc", h.layout.User(3).Root); err != nil {
		t.Fatal(err)
	}
	done := last(run(t, h.w, spec("planted", 3, cliworker.Claude, "hi")))
	if done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "not a directory") {
		t.Fatalf("done = %+v", done)
	}
	if err := os.Mkdir(h.layout.User(4).Root, 0o700); err != nil { // root's, not user 4's
		t.Fatal(err)
	}
	done = last(run(t, h.w, spec("foreign", 4, cliworker.Claude, "hi")))
	if done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "belongs to someone else") {
		t.Fatalf("done = %+v", done)
	}
}

// The first uid must be above every system and image uid: the start-up sweep kills every
// process from it up.
func TestIsolationRefusesALowUIDBase(t *testing.T) {
	h := newIsolated(t)
	for _, base := range []int{-1, 1, 1000, 65534} {
		cfg := h.cfg
		cfg.Isolation = &cliworker.Isolation{UIDBase: base, UserFile: h.cfg.Isolation.UserFile, Group: h.cfg.Isolation.Group}
		if _, err := cliworker.New(cfg); err == nil || !strings.Contains(err.Error(), "first uid") {
			t.Errorf("uid base %d: %v", base, err)
		}
	}
}

// An account entry that would make a user someone else (the name with another uid, or the
// uid with another name) is refused, not duplicated.
func TestIsolationRefusesAConflictingAccount(t *testing.T) {
	h := newIsolated(t)
	f, err := os.OpenFile(h.cfg.Isolation.UserFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(f, "tt5:x:4242:4242::/root:/bin/sh\nintruder:x:%d:%d::/:/bin/sh\n", isoBase+6, isoBase+6)
	_ = f.Close()
	for _, user := range []int64{5, 6} {
		done := last(run(t, h.w, spec(fmt.Sprintf("conflict-%d", user), user, cliworker.Claude, "hi")))
		if done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "for someone else") {
			t.Errorf("user %d: %+v", user, done)
		}
	}
}

// A user's commands can ask the approval socket only about their own turns: the worker
// knows who asks from the socket's peer uid, so naming another user's session reaches
// nothing of theirs.
func TestIsolationPermissionQuestionsStayWithTheirUser(t *testing.T) {
	h := newIsolated(t)
	h.signIn(t, 1, cliworker.Claude)
	h.scenario(t, 1, cliworker.Claude, clitest.Scenario{Tool: &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "touch proof.txt"}}})
	s1 := spec("victim", 1, cliworker.Claude, "go")
	turn, _, err := h.w.Start(s1)
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, h.w, 1)

	h.signIn(t, 2, cliworker.Claude)
	h.scenario(t, 2, cliworker.Claude, clitest.Scenario{Probes: []string{"ask:" + s1.Session}})
	s2 := spec("thief", 2, cliworker.Claude, "hi")
	s2.Permissions = cliworker.Ask
	done := last(run(t, h.w, s2))
	if done.Status != cliworker.StatusOK {
		t.Fatalf("user 2's turn: %+v", done)
	}
	// User 2's hook reached no turn: none of user 2's runs that session.
	if got := probeResult(done.Text, "ask:"+s1.Session); !strings.Contains(got, "no running turn") {
		t.Errorf("user 2's question about user 1's session: %s", got)
	}
	if qs := h.w.Permissions(1, 7); len(qs) != 1 || qs[0].ID != q.ID {
		t.Errorf("user 1's questions changed: %+v", qs)
	}
	if err := h.w.Answer(1, 7, q.ID, false); err != nil {
		t.Fatal(err)
	}
	waitDone(t, turn, 20*time.Second)
}

// Without a Unix user per taskto user, a worker running as root would run the CLIs as root:
// the helper refuses, unless the development override is set (docs/agent-safety.md).
func TestIsolationRefusesRootInLocalMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (Linux); CI runs it in a container")
	}
	for _, allow := range []bool{false, true} {
		h := newHarness(t, func(c *cliworker.Config) { c.UnsafePrivileged = allow })
		h.user(t, 1, cliworker.Claude, clitest.Scenario{Probes: []string{"uid"}}, true)
		done := last(run(t, h.w, spec(fmt.Sprintf("root-%t", allow), 1, cliworker.Claude, "hi")))
		switch {
		case !allow && (done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "refusing to run an agent's program as root")):
			t.Errorf("as root without the override: %+v", done)
		case allow && (done.Status != cliworker.StatusOK || probeResult(done.Text, "uid") != "ok 0 0"):
			t.Errorf("as root with the override: %+v", done)
		}
	}
}
