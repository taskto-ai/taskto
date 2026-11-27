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

type Config struct {
	Layout           Layout
	Bins             map[CLI]string
	Path             string
	Overrides        map[CLI]Overrides
	Helper           string
	Isolation        *Isolation
	AdoptLegacy      bool
	OneServer        bool
	UnsafePrivileged bool

	MaxTurns, MaxTurnsPerUser int
	TurnTimeout               time.Duration
	StopGrace                 time.Duration
	Keep                      time.Duration
	AskTimeout                time.Duration
}

type Worker struct {
	cfg Config

	mu                   sync.Mutex
	turns                map[string]*Turn
	logins               map[loginKey]*Login
	running              int
	perUser              map[string]int
	freed                chan struct{}
	named                chan struct{}
	closed               bool
	wg                   sync.WaitGroup
	gates                []*gate
	helpers              map[*helperRun]struct{}
	groups               map[int]groupRec
	groupWait            time.Duration
	groupSeq             uint64
	helperLaunches       atomic.Int64
	helpersBuilt         atomic.Int64
	dialHookForTest      func()
	detachHookForTest    func() error
	retiredHookForTest   func(identity string)
	arrivedHookForTest   func(boot string)
	provisionHookForTest func() error
	handOffHookForTest   func()
	loginRuns            map[*Login]struct{}
	accountsMu           sync.Mutex
	volumeLock           *os.File
	bootsMu              sync.Mutex
	boots                []bootEntry
	bootSeq              int64
	authorityLost        string
	installation         string
	reserved             map[string]reservation
	reservedTop          int64
	nextFree             int64
	usedHomes            map[int64]bool
	censusMu             sync.Mutex
	accounts             map[string]accountRecord
	allocateHookForTest  func() error
	adoptHookForTest     func() error
	browsers             Browsers

	approvals     net.Listener
	approveSocket string
	approvalsDir  string
}

var (
	ErrUnavailable = errors.New("this CLI is not installed on the worker")
	ErrTurnClash   = errors.New("a different turn has this ID")
	ErrClosed      = errors.New("the worker is shutting down")
)

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
	cfg.AskTimeout = min(cfg.AskTimeout, replyWait-5*time.Minute)
	if cfg.Helper == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cliworker: finding the helper: %w", err)
		}
		cfg.Helper = exe
	}
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
	if err := w.openVolume(); err != nil {
		w.closeVolume()
		return nil, err
	}
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

func (w *Worker) stateDir() string { return filepath.Join(w.cfg.Layout.Root, ".worker") }

type Browsers interface {
	StopAll(ctx context.Context, a cliowner.Account) error
	Unforget(again func(identity string) bool, before time.Time)
}

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

func (w *Worker) Available(cli CLI) bool { return w.cfg.Bins[cli] != "" }

func (w *Worker) sweep() error {
	if iso := w.cfg.Isolation; iso != nil {
		n, err := killUIDs(iso.UIDBase)
		if n > 0 {
			slog.Warn("cliworker: stopped the CLI processes an earlier worker left running", "processes", n)
		}
		if err != nil {
			return fmt.Errorf("cliworker: stopping what an earlier worker left running: %w", err)
		}
	}
	if err := w.sweepDetached(); err != nil {
		return err
	}
	partial, _ := filepath.Glob(filepath.Join(w.cfg.Layout.Root, provisionPrefix+"*"))
	for _, p := range partial {
		if err := removeTree(p, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("cliworker: removing a home an earlier worker was making: %w", err)
		}
	}
	records, _ := filepath.Glob(filepath.Join(w.stateDir(), "*.pid"))
	for _, rec := range records {
		b, err := os.ReadFile(filepath.Clean(rec))
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

func (w *Worker) Turn(id string) (*Turn, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.turns[id]
	return t, ok
}

func (w *Worker) TurnOf(a cliowner.Account, id string) (*Turn, bool) {
	if w.retired(a.Identity) {
		return nil, false
	}
	t, ok := w.Turn(id)
	if !ok || t.acct.id != a.Identity || t.spec.User != a.User {
		return nil, false
	}
	return t, true
}

func (w *Worker) Stop(id string) {
	if t, ok := w.Turn(id); ok {
		t.stop()
	}
}

func (w *Worker) StopTurnOf(a cliowner.Account, id string) {
	if t, ok := w.TurnOf(a, id); ok {
		t.stop()
	}
}

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

func (w *Worker) closeVolume() {
	if w.volumeLock != nil {
		_ = w.volumeLock.Close()
		w.volumeLock = nil
	}
}

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

func (w *Worker) sessionNamed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.named)
	w.named = make(chan struct{})
}

func (w *Worker) run(ctx context.Context, t *Turn, bin string) {
	if err := w.acquire(ctx, t); err != nil {
		t.finish(Event{Type: EventDone, Status: StatusStopped, Text: "stopped before it started"})
		return
	}
	defer w.release(t.acct.id)
	spec := t.spec
	done, missing := w.runCLI(ctx, t, bin, spec)
	if missing && ctx.Err() == nil {
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
	cmd.Stdin = io.MultiReader(bytes.NewReader(header), strings.NewReader(spec.Prompt))
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
		select {
		case <-exited:
			return
		default:
		}
		stopped = why
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
	unreaped := waitLeader(pgid)
	if unreaped {
		close(exited)
		<-stopperDone
		_ = signalGroup(pgid, sigKill)
		select {
		case <-linesDone:
		case <-time.After(5 * time.Second):
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

var sessionMissing = map[CLI]*regexp.Regexp{
	Claude: regexp.MustCompile(`No conversation found with session ID: \S+`),
	Codex:  regexp.MustCompile(`no rollout found for thread id \S+`),
	Grok:   regexp.MustCompile(`(?i)session \S+ (not found|does not exist)`),
}

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
	return nil
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

func readLines(r io.Reader, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 || isPrefix {
			long = append(long, chunk...)
			if len(long) > 16<<20 {
				long = long[:0]
				for isPrefix && err == nil {
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
