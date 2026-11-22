//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestMain(m *testing.M) {
	clitest.Main()
	os.Exit(m.Run())
}

type harness struct {
	w      *cliworker.Worker
	layout cliworker.Layout
	cfg    cliworker.Config
}

func newHarness(t *testing.T, tune func(*cliworker.Config)) *harness {
	t.Helper()
	root := t.TempDir()
	fakes := clitest.Install(t, t.TempDir())
	cfg := cliworker.Config{
		Layout:    cliworker.Layout{Root: filepath.Join(root, "cli")},
		Bins:      map[cliworker.CLI]string{},
		Path:      "/usr/bin:/bin",
		StopGrace: 2 * time.Second,
		// The helper refuses to run a CLI as root; a test job that runs as root allows it.
		UnsafePrivileged: os.Geteuid() == 0,
	}
	for name, path := range fakes {
		cfg.Bins[cliworker.CLI(name)] = path
	}
	if tune != nil {
		tune(&cfg)
	}
	w, err := cliworker.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return &harness{w: w, layout: cfg.Layout, cfg: cfg}
}

// user prepares user's directories with cli signed in and the scenario s.
func (h *harness) user(t *testing.T, user int64, cli cliworker.CLI, s clitest.Scenario, signedIn bool) cliworker.Dirs {
	t.Helper()
	dirs, err := h.layout.Ensure(user)
	if err != nil {
		t.Fatal(err)
	}
	if signedIn {
		clitest.SignIn(t, string(cli), dirs.Config(cli))
	}
	clitest.WriteScenario(t, dirs.Config(cli), s)
	return dirs
}

func spec(id string, user int64, cli cliworker.CLI, prompt string) cliworker.TurnSpec {
	s := cliworker.TurnSpec{ID: id, User: user, CLI: cli, Conversation: 7, Prompt: prompt}
	if cli != cliworker.Codex {
		s.Session = fmt.Sprintf("00000000-0000-4000-8000-%012d", user*1000+int64(len(id)))
	}
	return s
}

// run starts a turn and waits for it to end, returning all its events.
func run(t *testing.T, w *cliworker.Worker, s cliworker.TurnSpec) []cliworker.Event {
	t.Helper()
	turn, attached, err := w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		t.Fatal("a new turn was attached to an old one")
	}
	return waitDone(t, turn, 30*time.Second)
}

func waitDone(t *testing.T, turn *cliworker.Turn, limit time.Duration) []cliworker.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var all []cliworker.Event
	for {
		events, done, err := turn.Wait(ctx, len(all))
		if err != nil {
			t.Fatalf("the turn didn't end: %v; events so far %+v", err, all)
		}
		all = append(all, events...)
		if done {
			return all
		}
	}
}

func last(events []cliworker.Event) cliworker.Event { return events[len(events)-1] }

func find(events []cliworker.Event, typ cliworker.EventType) (cliworker.Event, bool) {
	for _, e := range events {
		if e.Type == typ {
			return e, true
		}
	}
	return cliworker.Event{}, false
}

func TestTurnOnEachCLI(t *testing.T) {
	t.Setenv("TASKTO_CANARY_SECRET", "must-not-reach-the-cli")
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			dirs := h.user(t, 1, cli, clitest.Scenario{Probes: []string{"nnp"}}, true)
			prompt := "--model evil\nhello there"
			events := run(t, h.w, spec("turn-"+string(cli), 1, cli, prompt))
			done := last(events)
			answer, _, _ := strings.Cut(done.Text, "\nprobe ")
			if done.Type != cliworker.EventDone || done.Status != cliworker.StatusOK || answer != "echo: "+prompt {
				t.Fatalf("done = %+v; events %+v", done, events)
			}
			// On Linux the CLI runs with no_new_privs: no setuid program (sudo) raises it.
			if nnp := probeResult(done.Text, "nnp"); runtime.GOOS == "linux" && nnp != "ok 1" {
				t.Errorf("no_new_privs = %s", nnp)
			}
			if done.Exit == nil || *done.Exit != 0 {
				t.Errorf("exit = %v", done.Exit)
			}
			for i, e := range events {
				if e.Seq != i+1 {
					t.Errorf("event %d has seq %d", i, e.Seq)
				}
			}
			session, ok := find(events, cliworker.EventSession)
			if !ok || session.Session == "" {
				t.Errorf("no session event: %+v", events)
			}
			if _, ok := find(events, cliworker.EventThinking); !ok {
				t.Errorf("no thinking event: %+v", events)
			}

			calls := clitest.Calls(t, dirs.Config(cli))
			if len(calls) != 1 {
				t.Fatalf("calls = %d", len(calls))
			}
			c := calls[0]
			if c.Prompt != prompt {
				t.Errorf("prompt = %q", c.Prompt)
			}
			for _, a := range c.Args {
				if strings.Contains(a, "hello there") {
					t.Errorf("the prompt is in argv: %q", c.Args)
				}
			}
			if real, _ := filepath.EvalSymlinks(dirs.Conversation(7)); c.Dir != real && c.Dir != dirs.Conversation(7) {
				t.Errorf("working directory = %q, want %q", c.Dir, dirs.Conversation(7))
			}
			env := map[string]string{}
			for _, kv := range c.Env {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			if env["HOME"] != dirs.Home || env["TMPDIR"] != dirs.Tmp || env["PATH"] != "/usr/bin:/bin" {
				t.Errorf("env = %q", c.Env)
			}
			configVar := map[cliworker.CLI]string{cliworker.Claude: "CLAUDE_CONFIG_DIR", cliworker.Codex: "CODEX_HOME", cliworker.Grok: "GROK_HOME"}[cli]
			if env[configVar] != dirs.Config(cli) {
				t.Errorf("%s = %q", configVar, env[configVar])
			}
			// A clean environment: only what the worker sets (the OS may add a few of its own,
			// such as __CF_USER_TEXT_ENCODING on macOS).
			allowed := []string{"PATH", "HOME", "LANG", "TMPDIR", configVar, "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "GROK_DISABLE_AUTOUPDATER", "__CF_USER_TEXT_ENCODING", "PWD"}
			for k := range env {
				if !slices.Contains(allowed, k) {
					t.Errorf("unexpected variable %s in the CLI's environment", k)
				}
			}
		})
	}
}

func TestResumeKeepsTheSession(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			h.user(t, 1, cli, clitest.Scenario{}, true)
			first := run(t, h.w, spec("first-"+string(cli), 1, cli, "one"))
			session, _ := find(first, cliworker.EventSession)
			s := spec("second-"+string(cli), 1, cli, "two")
			s.Session, s.Resume = session.Session, true
			second := run(t, h.w, s)
			if done := last(second); done.Status != cliworker.StatusOK || !strings.Contains(done.Text, "turn 2 of this session") {
				t.Fatalf("done = %+v", done)
			}
			again, _ := find(second, cliworker.EventSession)
			if again.Session != session.Session {
				t.Errorf("resumed session %q, want %q", again.Session, session.Session)
			}
		})
	}
}

// An expired login is reported at once, and the CLI stopped, instead of waiting out its retries
// (Claude Code's take minutes).
func TestExpiredLogin(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			h.user(t, 1, cli, clitest.Scenario{Expired: true}, true)
			start := time.Now()
			events := run(t, h.w, spec("expired-"+string(cli), 1, cli, "hi"))
			if done := last(events); done.Status != cliworker.StatusLoginRequired {
				t.Fatalf("done = %+v; events %+v", done, events)
			}
			if _, ok := find(events, cliworker.EventLoginRequired); !ok {
				t.Errorf("no login_required event: %+v", events)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("took %s: the worker waited for the CLI's retries", took)
			}
		})
	}
}

func TestUsageLimits(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range []cliworker.CLI{cliworker.Claude, cliworker.Codex} {
		t.Run(string(cli), func(t *testing.T) {
			h.user(t, 1, cli, clitest.Scenario{RateLimits: true}, true)
			events := run(t, h.w, spec("limits-"+string(cli), 1, cli, "hi"))
			var limits []cliworker.Limit
			for _, e := range events {
				limits = append(limits, e.Limits...)
			}
			if len(limits) == 0 {
				t.Fatalf("no limits: %+v", events)
			}
			for _, l := range limits {
				if l.UsedPercent == nil || l.ResetsAt == nil || l.Window == "" {
					t.Errorf("limit = %+v", l)
				}
			}
		})
	}
}

// alive reports whether process pid is running. A zombie (killed, not yet reaped) is not: where
// the tests' parent doesn't reap orphans (a CI job's container whose PID 1 is not an init), a
// killed grandchild stays a zombie, and kill(pid, 0) still succeeds on it.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil && !zombie(pid) }

func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil { // not Linux: ask ps
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && len(s) > i+2 && s[i+2] == 'Z'
}

func backgroundPID(t *testing.T, configDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(configDir, "background.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	return pid
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// What a CLI starts and leaves behind (a background shell) ends with its turn.
func TestBackgroundWorkEndsWithTheTurn(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{Background: true}, true)
	events := run(t, h.w, spec("bg", 1, cliworker.Claude, "hi"))
	if done := last(events); done.Status != cliworker.StatusOK {
		t.Fatalf("done = %+v", done)
	}
	pid := backgroundPID(t, dirs.Config(cliworker.Claude))
	eventually(t, "the background process outlived its turn", func() bool { return !alive(pid) })
}

func TestStop(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Codex, clitest.Scenario{SleepMS: 60_000, Background: true}, true)
	turn, _, err := h.w.Start(spec("stop", 1, cliworker.Codex, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the tool to start its processes, then stop.
	eventually(t, "the CLI didn't start its tool", func() bool {
		_, err := os.Stat(filepath.Join(dirs.Config(cliworker.Codex), "background.pid"))
		return err == nil
	})
	start := time.Now()
	h.w.Stop("stop")
	events := waitDone(t, turn, 10*time.Second)
	if done := last(events); done.Status != cliworker.StatusStopped {
		t.Fatalf("done = %+v", done)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping took %s", took)
	}
	pid := backgroundPID(t, dirs.Config(cliworker.Codex))
	eventually(t, "the turn's processes outlived the stop", func() bool { return !alive(pid) })
}

// A caller that lost the stream starts the turn again with the same ID and reads it from
// the start: the CLI runs once.
func TestReattach(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 500}, true)
	s := spec("same-id", 1, cliworker.Claude, "hi")
	first, attached, err := h.w.Start(s)
	if err != nil || attached {
		t.Fatal(err, attached)
	}
	again, attached, err := h.w.Start(s)
	if err != nil || !attached || again != first {
		t.Fatalf("second start: %v attached=%v same=%v", err, attached, again == first)
	}
	a, b := waitDone(t, first, 30*time.Second), waitDone(t, again, 30*time.Second)
	if len(a) != len(b) || last(b).Status != cliworker.StatusOK {
		t.Fatalf("events differ: %+v vs %+v", a, b)
	}
	if n := len(clitest.Calls(t, dirs.Config(cliworker.Claude))); n != 1 {
		t.Errorf("the CLI ran %d times", n)
	}
	// Finished, it can still be re-attached.
	if late, attached, err := h.w.Start(s); err != nil || !attached || late != first {
		t.Errorf("late re-attach: %v %v", err, attached)
	}
}

// One user can never attach to another's turn, even knowing its ID.
func TestTurnIDOfAnotherUser(t *testing.T) {
	h := newHarness(t, nil)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 300}, true)
	if _, _, err := h.w.Start(spec("mine", 1, cliworker.Claude, "hi")); err != nil {
		t.Fatal(err)
	}
	other := spec("mine", 2, cliworker.Claude, "hi")
	if _, _, err := h.w.Start(other); !errors.Is(err, cliworker.ErrTurnClash) {
		t.Fatalf("another user's start with the same ID: %v", err)
	}
	other = spec("mine", 1, cliworker.Claude, "hi")
	other.Conversation = 8
	if _, _, err := h.w.Start(other); !errors.Is(err, cliworker.ErrTurnClash) {
		t.Fatalf("another conversation's start with the same ID: %v", err)
	}
}

// Each user's CLIs work in that user's own directories, owner-only.
func TestUsersHaveTheirOwnDirectories(t *testing.T) {
	h := newHarness(t, nil)
	a := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	b := h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true)
	run(t, h.w, spec("a", 1, cliworker.Claude, "hi"))
	run(t, h.w, spec("b", 2, cliworker.Claude, "hi"))
	ca, cb := clitest.Calls(t, a.Config(cliworker.Claude)), clitest.Calls(t, b.Config(cliworker.Claude))
	if len(ca) != 1 || len(cb) != 1 {
		t.Fatalf("calls %d and %d", len(ca), len(cb))
	}
	if slices.Equal(ca[0].Env, cb[0].Env) || ca[0].Dir == cb[0].Dir {
		t.Error("two users' turns ran with the same environment or directory")
	}
	for _, d := range []string{a.Root, a.Home, a.Work, a.Config(cliworker.Claude), b.Root} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v", d, info.Mode().Perm())
		}
	}
}

func TestFailedTurn(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			h.user(t, 1, cli, clitest.Scenario{Fail: "the disk is full", Garbage: true}, true)
			events := run(t, h.w, spec("fail-"+string(cli), 1, cli, "hi"))
			if done := last(events); done.Status != cliworker.StatusFailed || !strings.Contains(done.Text, "the disk is full") {
				t.Fatalf("done = %+v", done)
			}
		})
	}
}

// A session to resume that doesn't exist (its CLI's files were reset, or its first request
// failed before it saved anything) is replaced by a new one, in the same turn, with the
// fresh prompt.
func TestResumeOfAMissingSessionStartsANewOne(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			dirs := h.user(t, 1, cli, clitest.Scenario{}, true)
			s := spec("missing-"+string(cli), 1, cli, "hi")
			s.Session, s.Resume, s.FreshPrompt = "00000000-0000-4000-8000-00000000dead", true, "the story so far, then: hi"
			events := run(t, h.w, s)
			if done := last(events); done.Status != cliworker.StatusOK || done.Text != "echo: the story so far, then: hi" {
				t.Fatalf("done = %+v; events %+v", done, events)
			}
			notice, _ := find(events, cliworker.EventNotice)
			session, _ := find(events, cliworker.EventSession)
			if !strings.Contains(notice.Text, "starting a new one") || session.Session == "" || session.Session == s.Session {
				t.Fatalf("events = %+v", events)
			}
			if calls := clitest.Calls(t, dirs.Config(cli)); len(calls) != 2 {
				t.Fatalf("calls = %d", len(calls))
			}
		})
	}
}

// A resume that fails for another reason isn't retried.
func TestResumeFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	first := run(t, h.w, spec("first", 1, cliworker.Claude, "one"))
	session, _ := find(first, cliworker.EventSession)
	clitest.WriteScenario(t, dirs.Config(cliworker.Claude), clitest.Scenario{Fail: "the disk is full"})
	s := spec("second", 1, cliworker.Claude, "two")
	s.Session, s.Resume = session.Session, true
	if done := last(run(t, h.w, s)); done.Status != cliworker.StatusFailed {
		t.Fatalf("done = %+v", done)
	}
	if calls := clitest.Calls(t, dirs.Config(cliworker.Claude)); len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
}

// Over the per-user limit, a turn waits for a free place instead of failing.
func TestTurnsWaitForAFreePlace(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.MaxTurnsPerUser = 1 })
	h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 700}, true)
	first, _, err := h.w.Start(spec("one", 1, cliworker.Claude, "a"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := first.Wait(ctx, 0); err != nil { // the first is running
		t.Fatal(err)
	}
	second, _, err := h.w.Start(spec("two-", 1, cliworker.Claude, "b"))
	if err != nil {
		t.Fatal(err)
	}
	events := waitDone(t, second, 30*time.Second)
	if events[0].Type != cliworker.EventNotice || last(events).Status != cliworker.StatusOK {
		t.Fatalf("second turn: %+v", events)
	}
	if last(waitDone(t, first, 30*time.Second)).Status != cliworker.StatusOK {
		t.Fatal("first turn failed")
	}
}

func TestUnavailableCLI(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { delete(c.Bins, cliworker.Grok) })
	if h.w.Available(cliworker.Grok) {
		t.Error("grok is available")
	}
	if _, _, err := h.w.Start(spec("g", 1, cliworker.Grok, "hi")); !errors.Is(err, cliworker.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// A worker that starts after a crash stops the CLIs the previous one left running, so a
// resumed session never has two writers.
func TestNewWorkerStopsOrphans(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000, Background: true}, true)
	turn, _, err := h.w.Start(spec("orphan", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the CLI didn't start", func() bool {
		_, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), "background.pid"))
		return err == nil
	})
	cliPID := clitest.Calls(t, dirs.Config(cliworker.Claude))[0].PID
	bg := backgroundPID(t, dirs.Config(cliworker.Claude))

	// As if the first worker had crashed: a second one starts on the same directories.
	second, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	eventually(t, "the orphaned CLI is still running", func() bool { return !alive(cliPID) && !alive(bg) })
	if done := last(waitDone(t, turn, 10*time.Second)); done.Status == cliworker.StatusOK {
		t.Errorf("the orphaned turn finished: %+v", done)
	}
	if recs, _ := filepath.Glob(filepath.Join(h.layout.Root, ".worker", "*.pid")); len(recs) != 0 {
		t.Errorf("records left: %v", recs)
	}
}

// A stop that reaches a turn as its CLI finishes doesn't lose the answer: a CLI that exited 0
// with its result is reported done, whichever came first.
func TestStopRacingTheEnd(t *testing.T) {
	h := newHarness(t, nil)
	h.user(t, 1, cliworker.Codex, clitest.Scenario{}, true)
	for i := range 20 {
		s := spec(fmt.Sprintf("race-%d", i), 1, cliworker.Codex, "hi")
		turn, _, err := h.w.Start(s)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i) * time.Millisecond)
		h.w.Stop(s.ID)
		done := last(waitDone(t, turn, 10*time.Second))
		if done.Exit != nil && *done.Exit == 0 && (done.Status != cliworker.StatusOK || done.Text != "echo: hi") {
			t.Fatalf("attempt %d: a CLI that finished was reported %+v", i, done)
		}
		if done.Status != cliworker.StatusOK && done.Status != cliworker.StatusStopped {
			t.Fatalf("attempt %d: %+v", i, done)
		}
	}
}

// A background process that keeps the CLI's stdout open doesn't hold the turn up: it is
// killed with the rest of the group once the CLI has exited.
func TestBackgroundHoldingStdout(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{Background: true, HoldStdout: true}, true)
	start := time.Now()
	events := run(t, h.w, spec("held", 1, cliworker.Claude, "hi"))
	if done := last(events); done.Status != cliworker.StatusOK || done.Text != "echo: hi" {
		t.Fatalf("done = %+v", done)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the turn took %s", took)
	}
	pid := backgroundPID(t, dirs.Config(cliworker.Claude))
	eventually(t, "the background process outlived its turn", func() bool { return !alive(pid) })
}

// probeResult returns what a fake's answer reports for probe.
func probeResult(answer, probe string) string {
	for _, line := range strings.Split(answer, "\n") {
		if rest, ok := strings.CutPrefix(line, "probe "+probe+": "); ok {
			return rest
		}
	}
	return "(no result)"
}
