package cliworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LoginState is where a login is.
type LoginState string

// The states of a login, in order; the last four are final.
const (
	LoginStarting  LoginState = "starting"
	LoginWaiting   LoginState = "waiting" // for the user, at the URL (and, for Claude Code, to paste a code back)
	LoginConnected LoginState = "connected"
	LoginFailed    LoginState = "failed"
	LoginExpired   LoginState = "expired"
	LoginCancelled LoginState = "cancelled"
)

// Final reports whether the login is over.
func (s LoginState) Final() bool {
	return s == LoginConnected || s == LoginFailed || s == LoginExpired || s == LoginCancelled
}

// LoginInfo is what the user sees of a login: the CLI's own sign-in URL and one-time code.
type LoginInfo struct {
	CLI   CLI        `json:"cli"`
	Slot  Slot       `json:"slot,omitempty"`
	State LoginState `json:"state"`
	// URL is where the user signs in. URLTrusted says it is on the CLI's known sign-in host;
	// one that isn't is shown as text with a warning, not as a link.
	URL        string `json:"url,omitempty"`
	URLTrusted bool   `json:"url_trusted"`
	// Code is the one-time code the user enters at the URL (Codex, Grok Build).
	Code string `json:"code,omitempty"`
	// NeedsCode means the user pastes the code the sign-in page shows back here (Claude Code).
	NeedsCode bool `json:"needs_code"`
	// Message says why a login failed, in the CLI's words when it gave some.
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// AuthStatus is what a CLI says of its login.
type AuthStatus struct {
	LoggedIn bool `json:"logged_in"`
	// Method is how: the CLI's own name for it (claude.ai, chatgpt, api_key, oauth, …).
	Method string `json:"method,omitempty"`
}

// signInHosts are the hosts each CLI's login sends the user to (and their subdomains).
var signInHosts = map[CLI][]string{
	Claude: {"claude.ai", "claude.com", "anthropic.com"},
	Codex:  {"auth.openai.com", "chatgpt.com", "openai.com"},
	Grok:   {"x.ai", "grok.com"},
}

// loginCommand is each CLI's own login, the device-code flow where there is one. Never a
// command that prints a token for us to read (`claude setup-token`): the CLI writes its
// credentials into the user's config directory itself.
func loginCommand(cli CLI) []string {
	switch cli {
	case Claude:
		return []string{"auth", "login", "--claudeai"}
	case Codex:
		return []string{"login", "--device-auth"}
	default:
		return []string{"login", "--device-auth"}
	}
}

func logoutCommand(cli CLI) []string {
	if cli == Claude {
		return []string{"auth", "logout"}
	}
	return []string{"logout"}
}

// Login is one login in progress, or recently finished.
type Login struct {
	user int64
	cli  CLI
	slot Slot

	mu     sync.Mutex
	info   LoginInfo
	out    []byte // what the command printed, ANSI escapes removed, bounded
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{} // closed once the URL is known or the login is over
}

// Info returns the login as the user sees it.
func (l *Login) Info() LoginInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

// Done is closed when the login is over.
func (l *Login) Done() <-chan struct{} { return l.done }

// Errors of the login methods.
var (
	ErrNoLogin      = errors.New("no sign-in is in progress")
	ErrNoCodeNeeded = errors.New("this sign-in doesn't take a code")
	ErrBadCode      = errors.New("the code must be 1 to 512 printable characters")
)

// loginTimeout is how long a login waits for the user: the device codes expire in 15 minutes.
const loginTimeout = 15 * time.Minute

// loginKey names a login: one per user, CLI and account slot.
type loginKey struct {
	user int64
	cli  CLI
	slot Slot
}

// StartLogin starts cli's login for user, or returns the one in progress. It waits a little
// for the CLI to print its URL, so the answer usually has it.
func (w *Worker) StartLogin(user int64, cli CLI, slot Slot) (*Login, error) {
	if user <= 0 || !cli.Valid() || !slot.Valid() || slot.Shared() {
		return nil, fmt.Errorf("invalid user, CLI or account")
	}
	bin := w.cfg.Bins[cli]
	if bin == "" {
		return nil, ErrUnavailable
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	key := loginKey{user, cli, slot}
	if old := w.logins[key]; old != nil && !old.Info().State.Final() {
		w.mu.Unlock()
		return old, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	l := &Login{
		user: user, cli: cli, slot: slot, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}),
		info: LoginInfo{CLI: cli, Slot: slot, State: LoginStarting, StartedAt: time.Now().UTC(), NeedsCode: cli == Claude},
	}
	w.logins[key] = l
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		w.runLogin(ctx, l, bin)
	}()
	select {
	case <-l.ready:
	case <-time.After(15 * time.Second):
	}
	return l, nil
}

// Login returns user's login for cli's account in slot, in progress or finished.
func (w *Worker) Login(user int64, cli CLI, slot Slot) (*Login, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	l := w.logins[loginKey{user, cli, slot}]
	return l, l != nil
}

// SubmitLoginCode passes the code the sign-in page showed to user's Claude Code login.
func (w *Worker) SubmitLoginCode(user int64, cli CLI, slot Slot, code string) error {
	l, ok := w.Login(user, cli, slot)
	if !ok || l.Info().State.Final() {
		return ErrNoLogin
	}
	if !l.Info().NeedsCode {
		return ErrNoCodeNeeded
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 512 || strings.IndexFunc(code, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return ErrBadCode
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stdin == nil {
		return ErrNoLogin
	}
	_, err := io.WriteString(l.stdin, code+"\n")
	return err
}

// CancelLogin stops user's login for cli's account in slot, if one is in progress.
func (w *Worker) CancelLogin(user int64, cli CLI, slot Slot) {
	if l, ok := w.Login(user, cli, slot); ok {
		l.mu.Lock()
		if !l.info.State.Final() {
			l.info.State = LoginCancelled
		}
		l.mu.Unlock()
		l.cancel()
	}
}

var (
	ansiEscape  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	urlPattern  = regexp.MustCompile(`https://[^\s"'<>]+`)
	codePattern = regexp.MustCompile(`\b[A-Z0-9]{4,5}-[A-Z0-9]{4,6}\b`)
)

func (w *Worker) runLogin(ctx context.Context, l *Login, bin string) {
	defer close(l.done)
	defer l.cancel()
	defer func() {
		select {
		case <-l.ready:
		default:
			close(l.ready)
		}
	}()
	finish := func(state LoginState, msg string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.info.State.Final() { // a cancel set it already
			l.info.State, l.info.Message = state, msg
		}
	}
	cmd, header, err := w.helperCommand(l.user, helperRequest{Op: "exec", CLI: l.cli, Slot: l.slot, Bin: bin, Args: loginCommand(l.cli)})
	if err != nil {
		finish(LoginFailed, "the worker could not start the sign-in")
		slog.Error("cliworker: login", "cli", l.cli, "err", err)
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		finish(LoginFailed, err.Error())
		return
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		finish(LoginFailed, "the CLI could not start: "+err.Error())
		return
	}
	// The helper's request first; what follows is the CLI's stdin (the pasted code).
	if _, err := stdin.Write(header); err != nil {
		finish(LoginFailed, "the CLI could not start: "+err.Error())
		_ = signalGroup(cmd.Process.Pid, sigKill)
		_ = cmd.Wait()
		return
	}
	l.mu.Lock()
	l.stdin = stdin
	l.mu.Unlock()
	pgid := cmd.Process.Pid
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = signalGroup(pgid, sigTerm)
			select {
			case <-stopped:
			case <-time.After(w.cfg.StopGrace):
				_ = signalGroup(pgid, sigKill)
			}
		case <-stopped:
		}
	}()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				l.output(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	// As for a turn: what the login left behind is killed while the CLI is still unreaped
	// where the system allows, so its group's ID can't have been reused.
	unreaped := waitLeader(pgid)
	if unreaped {
		close(stopped)
		_ = signalGroup(pgid, sigKill)
	}
	waitErr := cmd.Wait()
	_ = pw.Close()
	<-readDone
	if !unreaped {
		close(stopped)
		_ = signalGroup(pgid, sigKill)
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		finish(LoginExpired, "the sign-in wasn't finished in time: start again")
		return
	case ctx.Err() != nil:
		finish(LoginCancelled, "")
		return
	case waitErr != nil:
		finish(LoginFailed, l.lastWords())
		return
	}
	statusCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := w.Status(statusCtx, l.user, l.cli, l.slot)
	switch {
	case err != nil:
		finish(LoginFailed, "the CLI finished, but its status could not be read: "+err.Error())
	case !st.LoggedIn:
		finish(LoginFailed, "the CLI finished, but it isn't signed in")
	default:
		finish(LoginConnected, "")
	}
}

// output takes what the login command printed, and picks the URL and code out of it.
func (l *Login) output(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.out) < 16<<10 {
		l.out = append(l.out, b...)
	}
	text := ansiEscape.ReplaceAllString(string(l.out), "")
	if l.info.URL == "" || !l.info.URLTrusted {
		if u, trusted := signInURL(l.cli, text); u != "" && (l.info.URL == "" || trusted) {
			l.info.URL, l.info.URLTrusted = u, trusted
		}
	}
	if l.cli != Claude && l.info.Code == "" {
		l.info.Code = oneTimeCode(text)
	}
	complete := l.info.URL != "" && (l.cli == Claude || l.info.Code != "")
	if complete && l.info.State == LoginStarting {
		l.info.State = LoginWaiting
		close(l.ready)
	}
}

// oneTimeCode is the device code in a device login's output: on a line that speaks of the
// code, or within the three lines after one ("2. Enter this one-time code …" then the code;
// Grok's "Confirm this code in your browser:" then the code). An uppercase token elsewhere
// in the output is never taken for it, nor one inside a URL.
func oneTimeCode(text string) string {
	lines := strings.Split(text, "\n")
	mention := -10
	for i, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "code") && !strings.Contains(lower, "https://") {
			mention = i
		}
		if i-mention > 3 || strings.Contains(line, "https://") {
			continue
		}
		if m := codePattern.FindString(line); m != "" {
			return m
		}
	}
	return ""
}

// signInURL returns the first https URL in text on cli's sign-in hosts, or else the first
// https URL at all (untrusted), or "".
func signInURL(cli CLI, text string) (string, bool) {
	first := ""
	for _, raw := range urlPattern.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;:)")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if first == "" {
			first = raw
		}
		host := strings.ToLower(u.Hostname())
		for _, h := range signInHosts[cli] {
			if host == h || strings.HasSuffix(host, "."+h) {
				return raw, true
			}
		}
	}
	return first, false
}

// lastWords is the end of what the command printed, for a failed login's message.
func (l *Login) lastWords() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := strings.TrimSpace(ansiEscape.ReplaceAllString(string(l.out), ""))
	lines := strings.Split(text, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	msg := strings.TrimSpace(strings.Join(lines, " "))
	if msg == "" {
		return "the CLI's sign-in failed"
	}
	return cut(msg, 512)
}

// Status asks cli whether user is signed in: `claude auth status --json`, `codex login
// status`; Grok Build has no status command, so its credentials file is looked for. Each runs
// as the user, through the helper.
func (w *Worker) Status(ctx context.Context, user int64, cli CLI, slot Slot) (AuthStatus, error) {
	bin := w.cfg.Bins[cli]
	if bin == "" {
		return AuthStatus{}, ErrUnavailable
	}
	if !slot.Valid() || slot.Shared() {
		return AuthStatus{}, errors.New("invalid account")
	}
	switch cli {
	case Claude:
		// Signed out, it exits 1 on Linux (0 on macOS) with the same JSON: read the JSON.
		out, errOut, err := w.runQuick(ctx, user, helperRequest{Op: "exec", CLI: cli, Slot: slot, Bin: bin, Args: []string{"auth", "status", "--json"}})
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			return AuthStatus{}, err
		}
		var st struct {
			LoggedIn   bool   `json:"loggedIn"`
			AuthMethod string `json:"authMethod"`
		}
		if i, j := bytes.IndexByte(out, '{'), bytes.LastIndexByte(out, '}'); i >= 0 && j > i {
			out = out[i : j+1]
		}
		if err := json.Unmarshal(out, &st); err != nil {
			slog.Warn("cliworker: unexpected status output", "cli", cli, "stderr", cut(strings.TrimSpace(string(errOut)), 512))
			return AuthStatus{}, fmt.Errorf("unexpected status output from %s", cli.Name())
		}
		return AuthStatus{LoggedIn: st.LoggedIn, Method: st.AuthMethod}, nil
	case Codex:
		// Exit 0 means signed in ("Logged in using ChatGPT", on stderr), 1 not.
		// Its answer is text, which it prints on stderr: read both streams, here only.
		out, errOut, err := w.runQuick(ctx, user, helperRequest{Op: "exec", CLI: cli, Slot: slot, Bin: bin, Args: []string{"login", "status"}})
		var ee *exec.ExitError
		switch {
		case err == nil:
			method := "chatgpt"
			if strings.Contains(strings.ToLower(string(out)+" "+string(errOut)), "api key") {
				method = "api_key"
			}
			return AuthStatus{LoggedIn: true, Method: method}, nil
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			return AuthStatus{}, nil //nolint:nilerr // exit 1 is the answer "not logged in", not a failure
		default:
			return AuthStatus{}, err
		}
	default:
		out, _, err := w.runQuick(ctx, user, helperRequest{Op: "authfile", CLI: cli, Slot: slot})
		if err != nil {
			return AuthStatus{}, err
		}
		var st struct {
			Exists bool `json:"exists"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
			return AuthStatus{}, fmt.Errorf("unexpected answer from the helper")
		}
		if !st.Exists {
			return AuthStatus{}, nil
		}
		return AuthStatus{LoggedIn: true, Method: "oauth"}, nil
	}
}

// Logout signs user out of cli with its own logout command, then removes everything the CLI
// kept for them (sessions, settings): disconnecting starts over.
func (w *Worker) Logout(ctx context.Context, user int64, cli CLI, slot Slot) error {
	if !slot.Valid() {
		return errors.New("invalid account")
	}
	// A login in progress is stopped first, and waited for: one that ends while the files go
	// could write its credentials back.
	w.CancelLogin(user, cli, slot)
	if l, ok := w.Login(user, cli, slot); ok {
		select {
		case <-l.Done():
		case <-time.After(w.cfg.StopGrace + 5*time.Second):
			return errors.New("the sign-in in progress didn't stop")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// A shared account's directory holds no login to sign out of.
	if bin := w.cfg.Bins[cli]; bin != "" && !slot.Shared() {
		if _, _, err := w.runQuick(ctx, user, helperRequest{Op: "exec", CLI: cli, Slot: slot, Bin: bin, Args: logoutCommand(cli)}); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) { // "not logged in" is fine
				slog.Warn("cliworker: logout", "cli", cli, "err", err)
			}
		}
	}
	_, _, err := w.runQuick(ctx, user, helperRequest{Op: "reset", CLI: cli, Slot: slot})
	return err
}

// Forget removes everything the worker keeps for user, for an account that is deleted: it stops
// the logins in progress, signs the CLIs out (best effort: a login that can't be signed out is
// still deleted with the directory) and removes the user's directory, with their logins,
// sessions, working directories and browser profile. Idempotent.
func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	for _, cli := range []CLI{Claude, Codex, Grok} {
		for _, slot := range allSlots {
			w.CancelLogin(user, cli, slot)
		}
	}
	_, _, err := w.runQuick(ctx, user, helperRequest{Op: "forget"})
	return err
}

// allSlots are the slots of a user's own accounts: the first and 2 to 9.
var allSlots = []Slot{"", "2", "3", "4", "5", "6", "7", "8", "9"}

// codexLimits asks the helper, as the user, for the usage limits in thread's session file.
func (w *Worker) codexLimits(user int64, slot Slot, thread string) []Limit {
	if thread == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _, err := w.runQuick(ctx, user, helperRequest{Op: "limits", CLI: Codex, Slot: slot, Thread: thread})
	if err != nil {
		slog.Warn("cliworker: reading Codex's usage limits", "err", err)
		return nil
	}
	var limits []Limit
	if err := json.Unmarshal(bytes.TrimSpace(out), &limits); err != nil {
		return nil
	}
	return limits
}

// runQuick runs a short helper request for user (a status, a logout, a read) and returns its
// stdout and stderr, apart: the helper's JSON answers and a CLI's JSON are on stdout, and a
// warning on stderr (a runtime's, a CLI's) must never be parsed with them. A caller reads
// stderr only for a CLI that answers there, or to say why. It is stopped after 30 s or when
// ctx ends.
func (w *Worker) runQuick(ctx context.Context, user int64, req helperRequest) (stdoutBytes, stderrBytes []byte, err error) {
	cmd, header, err := w.helperCommand(user, req)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd.Stdin = bytes.NewReader(header)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: 64 << 10}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4 << 10}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = signalGroup(cmd.Process.Pid, sigKill)
		case <-done:
		}
	}()
	// As for a turn: leftovers are killed while the leader is unreaped, where the system allows.
	unreaped := waitLeader(cmd.Process.Pid)
	if unreaped {
		close(done)
		_ = signalGroup(cmd.Process.Pid, sigKill)
	}
	err = cmd.Wait()
	if !unreaped {
		close(done)
		_ = signalGroup(cmd.Process.Pid, sigKill)
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	return len(p), nil
}
