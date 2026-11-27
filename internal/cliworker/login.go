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

type LoginState string

const (
	LoginStarting  LoginState = "starting"
	LoginWaiting   LoginState = "waiting"
	LoginConnected LoginState = "connected"
	LoginFailed    LoginState = "failed"
	LoginExpired   LoginState = "expired"
	LoginCancelled LoginState = "cancelled"
)

func (s LoginState) Final() bool {
	return s == LoginConnected || s == LoginFailed || s == LoginExpired || s == LoginCancelled
}

type LoginInfo struct {
	CLI        CLI        `json:"cli"`
	Slot       Slot       `json:"slot,omitempty"`
	State      LoginState `json:"state"`
	URL        string     `json:"url,omitempty"`
	URLTrusted bool       `json:"url_trusted"`
	Code       string     `json:"code,omitempty"`
	NeedsCode  bool       `json:"needs_code"`
	Message    string     `json:"message,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
}

type AuthStatus struct {
	LoggedIn bool   `json:"logged_in"`
	Method   string `json:"method,omitempty"`
}

var signInHosts = map[CLI][]string{
	Claude: {"claude.ai", "claude.com", "anthropic.com"},
	Codex:  {"auth.openai.com", "chatgpt.com", "openai.com"},
	Grok:   {"x.ai", "grok.com"},
}

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

type Login struct {
	user int64
	cli  CLI
	slot Slot

	mu     sync.Mutex
	info   LoginInfo
	out    []byte
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{}
}

func (l *Login) Info() LoginInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

func (l *Login) Done() <-chan struct{} { return l.done }

var (
	ErrNoLogin      = errors.New("no sign-in is in progress")
	ErrNoCodeNeeded = errors.New("this sign-in doesn't take a code")
	ErrBadCode      = errors.New("the code must be 1 to 512 printable characters")
)

const loginTimeout = 15 * time.Minute

type loginKey struct {
	user int64
	cli  CLI
	slot Slot
}

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
	if w.isForgotten(user) {
		w.mu.Unlock()
		return nil, ErrForgotten
	}
	if w.gated(user, cli, slot) {
		w.mu.Unlock()
		return nil, ErrRemoving
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
	w.loginRuns[l] = struct{}{}
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.loginRuns, l)
			w.mu.Unlock()
		}()
		w.runLogin(ctx, l, bin)
	}()
	select {
	case <-l.ready:
	case <-time.After(15 * time.Second):
	}
	return l, nil
}

func (w *Worker) Login(user int64, cli CLI, slot Slot) (*Login, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	l := w.logins[loginKey{user, cli, slot}]
	return l, l != nil
}

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

func (w *Worker) CancelLogin(user int64, cli CLI, slot Slot) {
	if l, ok := w.Login(user, cli, slot); ok {
		l.stop()
	}
}

func (l *Login) stop() {
	l.mu.Lock()
	if !l.info.State.Final() {
		l.info.State = LoginCancelled
	}
	l.mu.Unlock()
	l.cancel()
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
		if !l.info.State.Final() {
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
	w.trackGroup(cmd.Process.Pid, l.user, l.cli, l.slot)
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
			return AuthStatus{}, nil
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

func (w *Worker) Logout(ctx context.Context, user int64, cli CLI, slot Slot) error {
	if !slot.Valid() {
		return errors.New("invalid account")
	}
	sc := scope{user: user, cli: cli, slot: slot}
	end, err := w.beginGate(ctx, sc)
	if err != nil {
		return err
	}
	defer end()
	if w.isForgotten(user) {
		return ErrForgotten
	}
	if err := w.quiesce(ctx, sc); err != nil {
		return err
	}
	if bin := w.cfg.Bins[cli]; bin != "" && !slot.Shared() {
		if _, _, err := w.runHelper(ctx, user, helperRequest{Op: "exec", CLI: cli, Slot: slot, Bin: bin, Args: logoutCommand(cli)}); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				slog.Warn("cliworker: logout", "cli", cli, "err", err)
			}
		}
	}
	if err := w.waitGroups(ctx, sc); err != nil {
		return err
	}
	_, _, err = w.runHelper(ctx, user, helperRequest{Op: "reset", CLI: cli, Slot: slot})
	return err
}

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

func (w *Worker) runQuick(ctx context.Context, user int64, req helperRequest) (stdoutBytes, stderrBytes []byte, err error) {
	ctx, release, err := w.admit(ctx, user, req.CLI, req.Slot)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return w.runHelper(ctx, user, req)
}

func (w *Worker) runHelper(ctx context.Context, user int64, req helperRequest) (stdoutBytes, stderrBytes []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
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
	w.trackGroup(cmd.Process.Pid, user, req.CLI, req.Slot)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = signalGroup(cmd.Process.Pid, sigKill)
		case <-done:
		}
	}()
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
