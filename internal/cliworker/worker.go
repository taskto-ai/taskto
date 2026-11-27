package cliworker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// Config configures a Worker.
type Config struct {
	// Layout is where the users' directories are.
	Layout Layout
	// Bins are the CLIs' executables (absolute paths). A CLI without one is unavailable.
	Bins map[CLI]string
	// Path is $PATH for the CLIs and the commands they run.
	Path string
	// Overrides are test settings per CLI (see Overrides); none in production.
	Overrides map[CLI]Overrides
	// Helper is the taskto binary, run as `taskto cli-helper` to start every CLI (see
	// RunHelper). Empty means this process's own executable.
	Helper string
	// Isolation, if set, runs each account's CLIs as a Unix user of its own (container mode).
	Isolation *Isolation
	// AdoptLegacy moves a directory from before homes (Root/u<n>) to the home of the first account
	// the worker sees with that number (local mode, where the server's backups hold the directories
	// with the database, so u<n> is the account's of number n). Otherwise such a directory is
	// removed (Reconcile).
	AdoptLegacy bool
	// UnsafePrivileged lets the helper start a CLI as root, which it otherwise refuses
	// (docs/agent-safety.md). For development and tests that run as root only.
	UnsafePrivileged bool

	// MaxTurns bounds the turns running at once, and MaxTurnsPerUser one user's; a turn over
	// either waits for a free place. Defaults 8 and 3.
	MaxTurns, MaxTurnsPerUser int
	// TurnTimeout bounds a CLI run (default 30 minutes).
	TurnTimeout time.Duration
	// StopGrace is how long a stopped CLI has between SIGTERM and SIGKILL (default 5 s).
	StopGrace time.Duration
	// Keep is how long a finished turn's events stay for re-attaching (default 15 minutes).
	Keep time.Duration
	// AskTimeout is how long a permission question waits for the owner's answer before it is
	// denied (default 20 minutes).
	AskTimeout time.Duration
}

// Worker runs CLI turns. Its methods are safe for concurrent use.
type Worker struct {
	cfg Config

	mu      sync.Mutex
	turns   map[string]*Turn
	logins  map[loginKey]*Login
	running int
	perUser map[string]int // running turns per account identity
	freed   chan struct{}  // closed and replaced when a running turn ends
	named   chan struct{}  // closed and replaced when a CLI names its session (waitTurnForHook)
	closed  bool
	wg      sync.WaitGroup
	// What a removal covers and waits for (removal.go): the accounts being disconnected, the
	// helper requests running, the process groups started for accounts, and every sign-in's
	// execution from its start until it returns, apart from logins (a new sign-in replaces a
	// cancelled one there while the old one's CLI may still run).
	gates     []*gate
	helpers   map[*helperRun]struct{}
	groups    map[int]groupRec
	groupWait time.Duration
	groupSeq  uint64
	// helperLaunches counts the helpers built to run, for tests, and helpersBuilt the ones that
	// passed every check.
	helperLaunches atomic.Int64
	helpersBuilt   atomic.Int64
	// dialHookForTest, in tests, runs in a live view's dial between its admission and its helper.
	dialHookForTest func()
	// retiredHookForTest, in tests, runs as a census has recorded an account's retirement, before
	// it stops what runs for it.
	retiredHookForTest func(identity string)
	// arrivedHookForTest, in tests, runs as a census has taken its number, before it waits for
	// the census lock.
	arrivedHookForTest func(boot string)
	// provisionHookForTest, in tests, runs as an isolated home is made, once it is owned and
	// before it is renamed into place; an error stops there, as a crash would.
	provisionHookForTest func() error
	// handOffHookForTest, in tests, runs in a live view's dial once its helper said it is connected,
	// before the connection's watch for a removal is set.
	handOffHookForTest func()
	loginRuns          map[*Login]struct{}
	// accountsMu guards the account records (accounts.go) and accounts holds those read; it is
	// never taken before mu.
	accountsMu sync.Mutex
	// volumeLock is the volume's lock, held while the worker runs (volume.go); boots is the server
	// boots seen, oldest first, under bootsMu; censusMu runs censuses and deletions one at a time.
	volumeLock *os.File
	bootsMu    sync.Mutex
	boots      []bootEntry
	bootSeq    int64
	// reserved is the numbers the censuses waiting for the census lock took as they arrived, by
	// boot, and reservedTop the highest given (volume.go), under bootsMu.
	reserved    map[string]reservation
	reservedTop int64
	// nextFree and usedHomes are the allocator's high-water mark and the homes the volume has
	// (records, retired ones too, and directories), under accountsMu (checkCounter).
	nextFree  int64
	usedHomes map[int64]bool
	censusMu  sync.Mutex
	accounts  map[string]accountRecord
	// allocateHookForTest, in tests, runs once a home number is taken, before it is recorded, and
	// adoptHookForTest once a directory from before homes is moved, before its record says so.
	allocateHookForTest func() error
	adoptHookForTest    func() error
	// browsers is the agent browsers that run in this worker (SetBrowsers), or nil.
	browsers Browsers

	// The approval socket (approve.go): its listener, its path and, when it had to go
	// elsewhere than the layout's root, the directory made for it.
	approvals     net.Listener
	approveSocket string
	approvalsDir  string
}

// Errors of Start.
var (
	ErrUnavailable = errors.New("this CLI is not installed on the worker")
	ErrTurnClash   = errors.New("a different turn has this ID")
	ErrClosed      = errors.New("the worker is shutting down")
)

// New returns a Worker. It first stops every CLI process group a previous worker on the same
// Layout recorded and left running (after a crash), so a resumed session never has two
// writers.
func New(cfg Config) (*Worker, error) {
	if !supported {
		return nil, errUnsupported
	}
	if cfg.Layout.Root == "" || !filepath.IsAbs(cfg.Layout.Root) {
		return nil, errors.New("cliworker: the layout's root must be an absolute path")
	}
	for cli, bin := range cfg.Bins {
		if !cli.Valid() || !filepath.IsAbs(bin) {
			return nil, fmt.Errorf("cliworker: %s's executable must be an absolute path", cli)
		}
	}
	if cfg.Path == "" {
		cfg.Path = "/usr/local/bin:/usr/bin:/bin"
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 8
	}
	if cfg.MaxTurnsPerUser <= 0 {
		cfg.MaxTurnsPerUser = 3
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 15 * time.Minute
	}
	if cfg.AskTimeout <= 0 {
		cfg.AskTimeout = 20 * time.Minute
	}
	// The hook program waits replyWait for an answer: a question must be settled before that.
	cfg.AskTimeout = min(cfg.AskTimeout, replyWait-5*time.Minute)
	if cfg.Helper == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cliworker: finding the helper: %w", err)
		}
		cfg.Helper = exe
	}
	// The layout's root is the worker's: owner-only in local mode; with isolation, users may
	// pass through it to their own directory (711), and list nothing.
	rootMode := os.FileMode(0o700)
	if iso := cfg.Isolation; iso != nil {
		if !isolationSupported {
			return nil, errors.New("cliworker: a Unix user per taskto user needs Linux")
		}
		if os.Geteuid() != 0 {
			return nil, errors.New("cliworker: a Unix user per taskto user needs the worker to run as root")
		}
		if iso.UIDBase == 0 {
			iso.UIDBase = 100000
		}
		// Above every system and image uid (nobody is 65534): the start-up sweep kills every
		// process from it up, so it must never reach anything but the CLI users.
		if err := checkUIDBase(iso.UIDBase); err != nil {
			return nil, err
		}
		if err := checkPidfds(); err != nil {
			return nil, fmt.Errorf("cliworker: %w", err)
		}
		if iso.UserFile == "" {
			iso.UserFile = "/etc/passwd"
		}
		if iso.Group == "" {
			iso.Group = "/etc/group"
		}
		rootMode = 0o711
	}
	if err := os.MkdirAll(cfg.Layout.Root, rootMode); err != nil {
		return nil, err
	}
	w := &Worker{cfg: cfg, turns: map[string]*Turn{}, logins: map[loginKey]*Login{}, perUser: map[string]int{}, accounts: map[string]accountRecord{}, freed: make(chan struct{}), named: make(chan struct{}), helpers: map[*helperRun]struct{}{}, groups: map[int]groupRec{}, groupWait: defaultGroupWait, loginRuns: map[*Login]struct{}{}}
	// The volume first: its lock before anything of a worker's is swept, listened on or written.
	if err := w.openVolume(); err != nil {
		w.closeVolume()
		return nil, err
	}
	// Only the worker that holds the volume sets its root's mode: a second one, refused, changes
	// nothing of the first's (whose users pass through the root to their homes).
	if err := os.Chmod(cfg.Layout.Root, rootMode); err != nil {
		w.closeVolume()
		return nil, err
	}
	if err := w.sweep(); err != nil {
		w.closeVolume()
		return nil, err
	}
	if err := w.listenApprovals(); err != nil {
		w.closeVolume()
		return nil, err
	}
	return w, nil
}

// stateDir holds the worker's own records, in the layout's root, where no user can write.
func (w *Worker) stateDir() string { return filepath.Join(w.cfg.Layout.Root, ".worker") }

// Browsers is the agent browsers that run in the worker (browser.Manager), with their profiles in
// the users' directories: removing a user's whole directory stops theirs first, and Reconcile takes
// back their refusals as it takes back the marks.
type Browsers interface {
	StopAll(ctx context.Context, a cliowner.Account) error
	Unforget(again func(identity string) bool, before time.Time)
}

// SetBrowsers tells the worker about the agent browsers that run in it, through its BrowserHost.
func (w *Worker) SetBrowsers(b Browsers) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.browsers = b
}

func (w *Worker) browserStopper() Browsers {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.browsers
}

// Available reports whether cli can run here.
func (w *Worker) Available(cli CLI) bool { return w.cfg.Bins[cli] != "" }

// sweep stops what an earlier worker left running. With isolation, every process of a CLI
// user. Without, the process groups its turns recorded, each only if its leader is still the
// process recorded (same PID, same start time), so a PID reused by an unrelated process is
// left alone. The records are removed.
func (w *Worker) sweep() error {
	if iso := w.cfg.Isolation; iso != nil {
		n, err := killUIDs(iso.UIDBase)
		if n > 0 {
			slog.Warn("cliworker: stopped the CLI processes an earlier worker left running", "processes", n)
		}
		if err != nil {
			// Not every process was looked at: one of an earlier history's may still run.
			return fmt.Errorf("cliworker: stopping what an earlier worker left running: %w", err)
		}
	}
	// Homes being made when an earlier worker stopped: never renamed into place, so nobody's.
	partial, _ := filepath.Glob(filepath.Join(w.cfg.Layout.Root, provisionPrefix+"*"))
	for _, p := range partial {
		if err := removeTree(p, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("cliworker: removing a home an earlier worker was making: %w", err)
		}
	}
	records, _ := filepath.Glob(filepath.Join(w.stateDir(), "*.pid"))
	for _, rec := range records {
		b, err := os.ReadFile(rec) //nolint:gosec // G304: a record the worker wrote in its own directory
		if err == nil {
			pid, start, ok := parseRecord(string(b))
			if ok {
				if now, err := startTime(pid); err == nil && now == start {
					slog.Warn("cliworker: stopping a CLI left running by an earlier worker", "pgid", pid, "record", rec)
					_ = signalGroup(pid, sigKill)
				}
			}
		}
		_ = os.Remove(rec)
	}
	return nil
}

func parseRecord(s string) (pid int, start string, ok bool) {
	p, start, found := strings.Cut(strings.TrimSpace(s), " ")
	n, err := strconv.Atoi(p)
	if !found || err != nil || n <= 1 || start == "" {
		return 0, "", false
	}
	return n, start, true
}

// Start starts spec's turn and returns it. If a turn with spec.ID is running or finished
// recently, it returns that one instead (attached true): the caller reads its events on
// rather than running the CLI again. A turn with that ID for another user, CLI or
// conversation is ErrTurnClash.
func (w *Worker) Start(spec TurnSpec) (t *Turn, attached bool, err error) {
	if err := spec.check(); err != nil {
		return nil, false, err
	}
	bin := w.cfg.Bins[spec.CLI]
	if bin == "" {
		return nil, false, ErrUnavailable
	}
	acct, err := w.resolve(spec.Account(), true)
	if err != nil {
		return nil, false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, false, ErrClosed
	}
	// Checked under the lock a removal takes after closing the account's gate: a turn is either
	// refused here or among the ones the removal stops and waits for.
	if err := w.admissible(acct, spec.CLI, spec.Slot); err != nil {
		return nil, false, err
	}
	if old, ok := w.turns[spec.ID]; ok {
		if old.acct.id != acct.id || old.spec.User != spec.User || old.spec.CLI != spec.CLI || old.spec.Slot != spec.Slot || old.spec.Conversation != spec.Conversation {
			return nil, false, ErrTurnClash
		}
		return old, true, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t = &Turn{spec: spec, acct: acct, cancel: cancel, changed: make(chan struct{}), ended: make(chan struct{})}
	w.turns[spec.ID] = t
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx, t, bin)
		time.AfterFunc(w.cfg.Keep, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.turns[spec.ID] == t {
				delete(w.turns, spec.ID)
			}
		})
	}()
	return t, false, nil
}

// Turn returns the turn with id, running or finished recently.
func (w *Worker) Turn(id string) (*Turn, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.turns[id]
	return t, ok
}

// TurnOf returns a's turn id: a turn of another account's, the number's earlier owner's too, is
// none.
func (w *Worker) TurnOf(a cliowner.Account, id string) (*Turn, bool) {
	// A retired account reaches none of what it had, even while its removal hasn't dropped it.
	if w.retired(a.Identity) {
		return nil, false
	}
	t, ok := w.Turn(id)
	if !ok || t.acct.id != a.Identity || t.spec.User != a.User {
		return nil, false
	}
	return t, true
}

// Stop stops turn id: its CLI gets SIGTERM, then SIGKILL after the grace time. A turn that
// isn't running is no error.
func (w *Worker) Stop(id string) {
	if t, ok := w.Turn(id); ok {
		t.stop()
	}
}

// StopTurnOf stops a's turn id, and none of another account's.
func (w *Worker) StopTurnOf(a cliowner.Account, id string) {
	if t, ok := w.TurnOf(a, id); ok {
		t.stop()
	}
}

// Close stops every turn and waits for them to end.
func (w *Worker) Close() {
	w.mu.Lock()
	w.closed = true
	turns := make([]*Turn, 0, len(w.turns))
	for _, t := range w.turns {
		turns = append(turns, t)
	}
	for _, l := range w.logins {
		l.cancel()
	}
	w.mu.Unlock()
	for _, t := range turns {
		t.stop()
	}
	w.closeApprovals()
	w.wg.Wait()
	w.closeVolume()
}

// closeVolume lets the volume go: another worker may use it.
func (w *Worker) closeVolume() {
	if w.volumeLock != nil {
		_ = w.volumeLock.Close()
		w.volumeLock = nil
	}
}

// acquire waits for a place for user's turn, or for ctx to end.
func (w *Worker) acquire(ctx context.Context, t *Turn) error {
	waiting := false
	for {
		w.mu.Lock()
		if w.running < w.cfg.MaxTurns && w.perUser[t.acct.id] < w.cfg.MaxTurnsPerUser {
			w.running++
			w.perUser[t.acct.id]++
			w.mu.Unlock()
			return nil
		}
		freed := w.freed
		w.mu.Unlock()
		if !waiting {
			waiting = true
			t.emit(Event{Type: EventNotice, Text: "waiting for another turn to finish"})
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *Worker) release(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running--
	if w.perUser[id]--; w.perUser[id] <= 0 {
		delete(w.perUser, id)
	}
	close(w.freed)
	w.freed = make(chan struct{})
}

// sessionNamed wakes the hooks' questions waiting for a CLI to name its session.
func (w *Worker) sessionNamed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.named)
	w.named = make(chan struct{})
}

// run runs t's CLI to the end and always finishes with an EventDone.
func (w *Worker) run(ctx context.Context, t *Turn, bin string) {
	if err := w.acquire(ctx, t); err != nil {
		t.finish(Event{Type: EventDone, Status: StatusStopped, Text: "stopped before it started"})
		return
	}
	defer w.release(t.acct.id)
	spec := t.spec
	done, missing := w.runCLI(ctx, t, bin, spec)
	if missing && ctx.Err() == nil {
		// The session to resume is gone (the CLI's files were reset, or its first request
		// failed before it saved anything): start a new one in its place, in the same turn.
		t.emit(Event{Type: EventNotice, Text: "the CLI's session for this conversation was gone: starting a new one"})
		spec.Resume, spec.Session = false, ""
		if spec.CLI != Codex {
			spec.Session = newUUID()
		}
		if spec.FreshPrompt != "" {
			spec.Prompt = spec.FreshPrompt
		}
		done, _ = w.runCLI(ctx, t, bin, spec)
	}
	t.finish(done)
}

// runCLI runs spec's CLI once and returns its done event, and whether it failed because the
// session it was to resume doesn't exist, before it did anything else.
func (w *Worker) runCLI(ctx context.Context, t *Turn, bin string, spec TurnSpec) (Event, bool) {
	failed := func(format string, args ...any) (Event, bool) {
		msg := fmt.Sprintf(format, args...)
		slog.Error("cliworker: a turn could not start", "turn", spec.ID, "cli", spec.CLI, "err", msg)
		return Event{Type: EventDone, Status: StatusFailed, Text: "the CLI could not start: " + msg}, false
	}
	c := turnCommand(spec)
	cmd, header, err := w.helperCommand(t.acct, helperRequest{
		Op: "exec", CLI: spec.CLI, Slot: spec.Slot, Proxy: spec.Proxy, Tools: spec.Tools, Bin: bin, Args: c.args, Conversation: spec.Conversation, PromptFile: c.promptFile,
	})
	if err != nil {
		return failed("%v", err)
	}
	// The request, then the prompt: the CLI's stdin, or (Grok) the helper's file. Then EOF,
	// so a CLI waiting on stdin sees it closed.
	cmd.Stdin = io.MultiReader(bytes.NewReader(header), strings.NewReader(spec.Prompt))
	// Before the CLI starts: its first hook may ask as soon as it runs.
	t.startSession(spec.Session)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed("%v", err)
	}
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return failed("%v", err)
	}
	pgid := cmd.Process.Pid
	defer w.trackGroup(pgid, t.acct.id, spec.CLI, spec.Slot)()
	// The ID was checked with the spec; checked again where it names a file.
	if !turnIDPattern.MatchString(spec.ID) {
		return failed("invalid turn ID")
	}
	record := filepath.Join(w.stateDir(), spec.ID+".pid")
	if start, err := startTime(pgid); err == nil {
		if err := os.WriteFile(record, []byte(strconv.Itoa(pgid)+" "+start+"\n"), 0o600); err != nil {
			slog.Warn("cliworker: recording a turn's process group", "turn", spec.ID, "err", err)
		}
	}
	defer os.Remove(record)

	// Stop the group when the turn is stopped, times out, or the login fails.
	var stopped stopReason
	stopCh := make(chan stopReason, 1)
	exited := make(chan struct{})
	stopperDone := make(chan struct{})
	go func() {
		defer close(stopperDone)
		timeout := time.NewTimer(w.cfg.TurnTimeout)
		defer timeout.Stop()
		var why stopReason
		select {
		case <-exited:
			return
		case <-ctx.Done():
			why = stopByUser
		case <-timeout.C:
			why = stopByTimeout
		case why = <-stopCh:
		}
		// The CLI may have exited at the same moment: then it ended on its own.
		select {
		case <-exited:
			return
		default:
		}
		stopped = why // read only after stopperDone
		_ = signalGroup(pgid, sigTerm)
		grace := time.NewTimer(w.cfg.StopGrace)
		defer grace.Stop()
		select {
		case <-exited:
		case <-grace.C:
			_ = signalGroup(pgid, sigKill)
		}
	}()

	var session string
	p := newParser(spec.CLI, func(e Event) {
		if e.Type == EventSession {
			if session == "" {
				session = e.Session
			}
			t.addSession(e.Session)
			w.sessionNamed()
		}
		t.emit(e)
	})
	linesDone := make(chan struct{})
	go func() {
		defer close(linesDone)
		readLines(stdout, func(line []byte) {
			wasLogin := p.login
			p.line(line)
			if p.login && !wasLogin {
				select {
				case stopCh <- stopLogin:
				default:
				}
			}
		})
	}()
	// The CLI's exit, then whatever it started and left behind (a background shell) ends
	// with the turn. Where the system allows (Linux), the leftovers are killed while the CLI
	// is still unreaped: its PID, and so its group's ID, can't have been reused by then.
	// The stopper has then either recorded why it stopped the CLI, or returned without
	// stopping it: either way, stopped is settled before it is read below.
	unreaped := waitLeader(pgid)
	if unreaped {
		close(exited)
		<-stopperDone
		_ = signalGroup(pgid, sigKill)
		select {
		case <-linesDone:
		case <-time.After(5 * time.Second): // a process outside the group holds the pipe
			_ = stdout.Close()
			<-linesDone
		}
	} else {
		<-linesDone
	}
	waitErr := cmd.Wait()
	if !unreaped {
		close(exited)
		<-stopperDone
		_ = signalGroup(pgid, sigKill)
	}

	if spec.CLI == Codex {
		thread := session
		if thread == "" {
			thread = spec.Session
		}
		if limits := w.codexLimits(t.acct, spec.Slot, thread); len(limits) > 0 {
			t.emit(Event{Type: EventUsage, Limits: limits})
		}
	}

	why := stopped
	code := exitCode(waitErr)
	done := Event{Type: EventDone, Exit: code}
	// A CLI that finished its turn before a stop reached it ended on its own: keep its answer.
	completed := code != nil && *code == 0 && p.failed == "" && (p.result || (spec.CLI == Codex && p.answer != ""))
	switch {
	case p.login || why == stopLogin:
		done.Status, done.Text = StatusLoginRequired, spec.CLI.Name()+" needs you to sign in again"
	case completed:
		done.Status, done.Text = StatusOK, p.answer
	case why == stopByUser:
		done.Status, done.Text = StatusStopped, "stopped"
	case why == stopByTimeout:
		done.Status, done.Text = StatusFailed, fmt.Sprintf("the CLI ran longer than %s and was stopped", w.cfg.TurnTimeout)
	case p.failed != "" && (code == nil || *code != 0) && authFailure.MatchString(stderr.String()):
		// A CLI that names a failed login only on stderr.
		done.Status, done.Text = StatusLoginRequired, spec.CLI.Name()+" needs you to sign in again"
		t.emit(Event{Type: EventLoginRequired, Text: cut(stderr.String(), maxToolText)})
	case p.failed != "":
		done.Status, done.Text = StatusFailed, cut(p.failed, maxEventText)
	case code == nil || *code != 0:
		done.Status, done.Text = StatusFailed, failureText(spec.CLI, code, stderr.String())
		if authFailure.MatchString(stderr.String()) {
			done.Status, done.Text = StatusLoginRequired, spec.CLI.Name()+" needs you to sign in again"
			t.emit(Event{Type: EventLoginRequired, Text: cut(stderr.String(), maxToolText)})
		}
	case spec.CLI != Codex && !p.result:
		done.Status, done.Text = StatusFailed, "the CLI ended without a result"
	default:
		done.Status, done.Text = StatusOK, p.answer
	}
	missing := spec.Resume && done.Status == StatusFailed && !p.activity &&
		(sessionMissing[spec.CLI].MatchString(p.failed) || sessionMissing[spec.CLI].MatchString(stderr.String()))
	return done, missing
}

// sessionMissing matches each CLI's own error for resuming a session that doesn't exist, as
// the pinned versions print it: Claude Code 2.1.311's "No conversation found with session ID:
// <id>", Codex 0.169.0's "thread/resume failed: no rollout found for thread id <id>". Grok
// Build's guide says only that -r "errors if the session does not exist": its pattern is the
// narrowest that says so, to be checked against a real one.
var sessionMissing = map[CLI]*regexp.Regexp{
	Claude: regexp.MustCompile(`No conversation found with session ID: \S+`),
	Codex:  regexp.MustCompile(`no rollout found for thread id \S+`),
	Grok:   regexp.MustCompile(`(?i)session \S+ (not found|does not exist)`),
}

// newUUID returns a random (version 4) UUID, a new session ID for Claude Code and Grok Build.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type stopReason int

const (
	stopNone stopReason = iota
	stopByUser
	stopByTimeout
	stopLogin
)

func exitCode(err error) *int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		c := 0
		return &c
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		c := ee.ExitCode()
		return &c
	}
	return nil // killed by a signal
}

func failureText(cli CLI, code *int, stderr string) string {
	msg := cli.Name() + " failed"
	if code != nil {
		msg += " (exit " + strconv.Itoa(*code) + ")"
	} else {
		msg += " (stopped by a signal)"
	}
	if s := strings.TrimSpace(stderr); s != "" {
		msg += ": " + cut(s, maxToolText)
	}
	return msg
}

// readLines calls fn with each line of r (without the newline), up to 16 MiB a line; a longer
// line is skipped.
func readLines(r io.Reader, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 || isPrefix {
			long = append(long, chunk...)
			if len(long) > 16<<20 {
				long = long[:0]
				for isPrefix && err == nil { // skip the rest of the line
					_, isPrefix, err = br.ReadLine()
				}
				continue
			}
		}
		if !isPrefix && len(long) > 0 {
			fn(long)
			long = long[:0]
		}
		if err != nil {
			return
		}
	}
}

// tailBuffer keeps the last max bytes written to it: a CLI's stderr, where login and
// configuration errors show up.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
