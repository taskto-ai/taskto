//go:build linux

package cliworker_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
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
	// pass through, a copy of this test binary as the helper and the fake CLIs. A short one: the
	// agent browser's sockets under a home must fit a Unix socket's path.
	base, err := os.MkdirTemp("", "iso")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
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

// introduce gives the tests' accounts of users their homes, as their first requests would.
func (h *isoHarness) introduce(t *testing.T, users ...int64) {
	t.Helper()
	for _, user := range users {
		h.w.HomeForTest(acct(user))
	}
}

// home is the directories of the tests' account of user.
func (h *isoHarness) home(user int64) cliworker.Dirs {
	return h.layout.Home(h.w.HomeForTest(acct(user)))
}

// uid is the Unix user of the tests' account of user: UIDBase plus its home number.
func (h *isoHarness) uid(user int64) int { return isoBase + int(h.w.HomeForTest(acct(user))) }

// signIn connects user's fake cli the way its login would: as the user, through the helper.
func (h *isoHarness) signIn(t *testing.T, user int64, cli cliworker.CLI) {
	t.Helper()
	h.introduce(t, user)
	l, err := h.w.StartLogin(acct(user), cli, "")
	if err != nil {
		t.Fatal(err)
	}
	if cli == cliworker.Claude {
		if err := h.w.SubmitLoginCode(acct(user), cli, "", clitest.GoodCode); err != nil {
			t.Fatal(err)
		}
	} else {
		approveAs(t, h.home(user).Config(cli), h.uid(user))
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
	dir := h.home(user).Config(cli)
	clitest.WriteScenario(t, dir, s)
	if err := os.Chown(filepath.Join(dir, "scenario.json"), h.uid(user), h.uid(user)); err != nil {
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
		if got := probeResult(done.Text, "uid"); got != fmt.Sprintf("ok %d %d", h.uid(1), h.uid(1)) {
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
	if err != nil || !strings.Contains(string(passwd), fmt.Sprintf("th%d:x:%d:%d:", h.w.HomeForTest(acct(1)), h.uid(1), h.uid(1))) {
		t.Fatalf("passwd = %q %v", passwd, err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(h.home(1).Root, &st); err != nil || int(st.Uid) != h.uid(1) || st.Mode&0o777 != 0o700 {
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
	one := h.home(1)
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
	cfg := h.home(1).Config(cliworker.Grok)
	eventually(t, "the CLI didn't start its tool", func() bool {
		_, err := os.Stat(filepath.Join(cfg, "background.pid"))
		return err == nil
	})
	bg := backgroundPID(t, cfg)
	cli := clitest.Calls(t, cfg)[0].PID

	h.w.CrashForTest() // the first worker's process is gone, and its lock with it
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
	h.introduce(t, 3, 4)
	if err := os.Symlink("/etc", h.home(3).Root); err != nil {
		t.Fatal(err)
	}
	done := last(run(t, h.w, spec("planted", 3, cliworker.Claude, "hi")))
	if done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "not a directory") {
		t.Fatalf("done = %+v", done)
	}
	if err := os.Mkdir(h.home(4).Root, 0o700); err != nil { // root's, not user 4's
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
	for _, base := range []int{-1, 1, 1000, 65534, cliworker.MaxUIDForTest - (1 << 30) + 1, 1 << 62} {
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
	_, _ = fmt.Fprintf(f, "th%d:x:4242:4242::/root:/bin/sh\nintruder:x:%d:%d::/:/bin/sh\n", h.w.HomeForTest(acct(5)), h.uid(6), h.uid(6))
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
	if qs := h.w.Permissions(acct(1), 7); len(qs) != 1 || qs[0].ID != q.ID {
		t.Errorf("user 1's questions changed: %+v", qs)
	}
	if err := h.w.Answer(acct(1), 7, q.ID, false); err != nil {
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

// Before a number's directory goes, and so before the number can be given to someone else, every
// process of its Unix user ends: one a CLI detached into a session of its own too, which no turn
// or process group of the worker's lists, and which would otherwise reach the next account's
// directory under the same uid.
func TestIsolationRemovalEndsEveryProcessOfTheUser(t *testing.T) {
	h := newIsolated(t)
	h.signIn(t, 5, cliworker.Claude)
	detached := exec.Command("sleep", "60")
	detached.Dir = "/"
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: uint32(h.uid(5)), Gid: uint32(h.uid(5))}} //nolint:gosec // G115: uidOf keeps a uid below the 32-bit nobody
	if err := detached.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _ = detached.Wait(); close(ended) }()
	t.Cleanup(func() { _ = detached.Process.Kill(); <-ended })

	// A restore gave 5 to another account: the old account is retired, and tidied up.
	old := h.home(5)
	next := cliowner.Account{User: 5, Identity: strings.Repeat("b", 32), Boot: strings.Repeat("c", 32)}
	again := census(map[int64]string{5: next.Identity})
	again.Boot = next.Boot // the server's next start
	if err := h.w.Reconcile(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a process of the old account's uid outlived the removal of its home")
	}
	if _, err := os.Stat(old.Root); !os.IsNotExist(err) {
		t.Errorf("the old home: %v", err)
	}
	// The new account has a home and a uid of its own.
	if st, err := h.w.Status(context.Background(), next, cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the new account: %+v %v", st, err)
	}
	if h.w.HomeForTest(next) == h.w.HomeForTest(acct(5)) {
		t.Error("the new account was given the old one's home")
	}
}

// The worker removes a user's directory as itself, root with only CAP_CHOWN of the file powers
// in the container (CI runs this with those capabilities alone too): a read-only part the user
// made, and a link to a file of root's, don't stop it, and the link isn't followed.
func TestIsolationRemovesAReadOnlyDirectoryOfTheUser(t *testing.T) {
	h := newIsolated(t)
	h.signIn(t, 6, cliworker.Claude)
	outside := filepath.Join(filepath.Dir(h.layout.Root), "outside.txt")
	if err := os.WriteFile(outside, []byte("root's"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Made by the user, as a tool of theirs would.
	work := h.home(6).Work
	mk := exec.Command("sh", "-c", `set -e; mkdir -p "$1/ro/inner"; echo x > "$1/ro/inner/file"; ln -s "$2" "$1/ro/inner/link"; chmod 400 "$1/ro/inner/file"; chmod 500 "$1/ro/inner" "$1/ro"`, "sh", work, outside)
	mk.Dir = "/"
	mk.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(h.uid(6)), Gid: uint32(h.uid(6))}} //nolint:gosec // G115: uidOf keeps a uid below the 32-bit nobody
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("making the read-only directory as the user: %v %s", err, out)
	}
	if err := h.w.Forget(context.Background(), acct(6)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(h.home(6).Root); !os.IsNotExist(err) {
		t.Errorf("the directory: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file a link pointed to went: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.layout.Root, ".worker", "trash")); len(entries) != 0 {
		t.Errorf("left in the trash: %v", entries)
	}
}

// (From the pull request that first ended a user's detached processes.) Forgetting a user ends
// every process of their Unix user before their directory goes: also one
// a tool of the CLI detached from the process group (a daemon, a browser's leftover), which the
// stops of the turns and groups don't reach, and which would go on running as the user, writing
// into the directory, or into a new person's that is given the number. Another user's processes,
// turn and directory are not touched: a uid is one user's alone.
func TestIsolationForgetEndsTheUsersDetachedProcesses(t *testing.T) {
	h := newIsolated(t)
	for _, user := range []int64{1, 2} {
		h.signIn(t, user, cliworker.Grok)
	}
	// User 1's tool detaches a process from the turn's group; user 2's stays in its own.
	h.scenario(t, 1, cliworker.Grok, clitest.Scenario{SleepMS: 60_000, Background: true, Detached: true})
	h.scenario(t, 2, cliworker.Grok, clitest.Scenario{SleepMS: 60_000, Background: true})
	var turns [3]*cliworker.Turn
	var bg [3]int
	for _, user := range []int64{1, 2} {
		turn, _, err := h.w.Start(spec(fmt.Sprintf("turn-%d", user), user, cliworker.Grok, "slow"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := h.home(user).Config(cliworker.Grok)
		eventually(t, "the CLI didn't start its tool", func() bool {
			_, err := os.Stat(filepath.Join(cfg, "background.pid"))
			return err == nil
		})
		turns[user], bg[user] = turn, backgroundPID(t, cfg)
	}
	// The detached process is in a session of its own: not in the turn's group.
	if pgid, err := syscall.Getpgid(bg[1]); err != nil || pgid != bg[1] {
		t.Fatalf("the detached process's group = %d, %v: want a group of its own (%d)", pgid, err, bg[1])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	if alive(bg[1]) {
		t.Error("the user's detached process still runs after Forget")
	}
	if !turns[1].Over() {
		t.Error("the user's turn still runs after Forget")
	}
	gone(t, h.home(1).Root)

	// The other user: process, turn and files.
	if !alive(bg[2]) {
		t.Error("another user's process was ended")
	}
	if turns[2].Over() {
		t.Errorf("another user's turn ended: %+v", last(waitDone(t, turns[2], time.Second)))
	}
	if _, err := os.Stat(h.home(2).Config(cliworker.Grok)); err != nil {
		t.Errorf("another user's directory: %v", err)
	}
	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Errorf("forgetting again: %v", err)
	}
}

// A worker started on a volume with another UID base than the one its accounts were given their
// uids under doesn't start: every home's uid would be another's.
func TestIsolationRefusesAChangedUIDBase(t *testing.T) {
	h := newIsolated(t)
	h.introduce(t, 1)
	h.w.Close()
	cfg := h.cfg
	cfg.Isolation = &cliworker.Isolation{UIDBase: h.cfg.Isolation.UIDBase + 1000, UserFile: h.cfg.Isolation.UserFile, Group: h.cfg.Isolation.Group}
	if w, err := cliworker.New(cfg); err == nil {
		w.Close()
		t.Fatal("a worker started with another UID base on a volume with accounts")
	} else if !strings.Contains(err.Error(), "-uid-base") {
		t.Errorf("the refusal: %v", err)
	}
	cfg.Isolation = &cliworker.Isolation{UIDBase: h.cfg.Isolation.UIDBase, UserFile: h.cfg.Isolation.UserFile, Group: h.cfg.Isolation.Group}
	w, err := cliworker.New(cfg)
	if err != nil {
		t.Fatalf("the volume's own base: %v", err)
	}
	w.Close()
}
