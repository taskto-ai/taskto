//go:build unix

package clitest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
)

type Scenario struct {
	Answer        string    `json:"answer,omitempty"`
	SleepMS       int       `json:"sleep_ms,omitempty"`
	TermWriteMS   int       `json:"term_write_ms,omitempty"`
	StatusSleepMS int       `json:"status_sleep_ms,omitempty"`
	LogoutSleepMS int       `json:"logout_sleep_ms,omitempty"`
	Background    bool      `json:"background,omitempty"`
	HoldStdout    bool      `json:"hold_stdout,omitempty"`
	Expired       bool      `json:"expired,omitempty"`
	RateLimits    bool      `json:"rate_limits,omitempty"`
	Warn          string    `json:"warn,omitempty"`
	Fail          string    `json:"fail,omitempty"`
	Garbage       bool      `json:"garbage,omitempty"`
	Tool          *ToolUse  `json:"tool,omitempty"`
	Probes        []string  `json:"probes,omitempty"`
	MCP           []MCPCall `json:"mcp,omitempty"`
}

type MCPCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

type Call struct {
	UID    int      `json:"uid"`
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Dir    string   `json:"dir"`
	Prompt string   `json:"prompt"`
	PID    int      `json:"pid"`
}

var names = map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME", "grok": "GROK_HOME"}

var credentials = map[string]string{"claude": ".credentials.json", "codex": "auth.json", "grok": "auth.json"}

func Main() {
	if len(os.Args) > 1 && os.Args[1] == cliworker.HelperArg {
		if filepath.Base(os.Args[0]) == noisyHelperName {
			fmt.Fprintln(os.Stderr, HelperWarning)
		}
		os.Exit(cliworker.RunHelper())
	}
	if len(os.Args) > 1 && os.Args[1] == cliworker.ApproveArg {
		os.Exit(cliworker.RunApprove(os.Args[2:]))
	}
	name := filepath.Base(os.Args[0])
	if _, ok := names[name]; !ok {
		return
	}
	os.Exit(run(name, os.Args[1:]))
}

const noisyHelperName = "taskto-noisy-helper"

const HelperWarning = "warning: something the helper's runtime says on stderr"

func InstallNoisyHelper(t testing.TB, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, noisyHelperName)
	if err := os.Symlink(exe, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func Install(t testing.TB, dir string) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name := range names {
		path := filepath.Join(dir, name)
		if err := os.Symlink(exe, path); err != nil {
			t.Fatal(err)
		}
		out[name] = path
	}
	return out
}

func WriteScenario(t testing.TB, configDir string, s Scenario) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(configDir, "scenario.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func SignIn(t testing.TB, cli, configDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, credentials[cli]), []byte(`{"fake":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func Calls(t testing.TB, configDir string) []Call {
	t.Helper()
	f, err := os.Open(filepath.Join(configDir, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

type fake struct {
	name     string
	config   string
	args     []string
	scenario Scenario
	out      *json.Encoder
	term     chan os.Signal
}

func run(name string, args []string) int {
	config := os.Getenv(names[name])
	if config == "" {
		fmt.Fprintf(os.Stderr, "%s: %s is not set\n", name, names[name])
		return 2
	}
	f := &fake{name: name, config: config, args: args, out: json.NewEncoder(os.Stdout)}
	if b, err := os.ReadFile(filepath.Join(config, "scenario.json")); err == nil {
		_ = json.Unmarshal(b, &f.scenario)
	}
	if f.scenario.SleepMS > 0 {
		f.term = make(chan os.Signal, 1)
		signal.Notify(f.term, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	}
	switch {
	case name == "claude" && len(args) >= 2 && args[0] == "auth":
		return f.claudeAuth(args[1])
	case name != "claude" && len(args) >= 1 && (args[0] == "login" || args[0] == "logout"):
		return f.deviceAuth(args)
	}
	switch name {
	case "codex":
		if len(args) > 0 && args[0] == "exec" {
			return f.codexExec(args[1:])
		}
	default:
		if prompt, session, resume, ok := f.streamArgs(); ok {
			return f.streamTurn(prompt, session, resume)
		}
	}
	fmt.Fprintf(os.Stderr, "%s: unsupported arguments %q\n", name, args)
	return 2
}

func (f *fake) record(prompt string) {
	dir, _ := os.Getwd()
	b, _ := json.Marshal(Call{UID: os.Getuid(), Args: f.args, Env: os.Environ(), Dir: dir, Prompt: prompt, PID: os.Getpid()})
	file, err := os.OpenFile(filepath.Join(f.config, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = file.Write(append(b, '\n'))
		_ = file.Close()
	}
}

var keyVars = map[string]string{"claude": "ANTHROPIC_AUTH_TOKEN", "codex": "TASKTO_PROXY_TOKEN", "grok": "XAI_API_KEY"}

func (f *fake) signedIn() bool {
	if os.Getenv(keyVars[f.name]) != "" {
		return !f.scenario.Expired
	}
	_, err := os.Stat(filepath.Join(f.config, credentials[f.name]))
	return err == nil && !f.scenario.Expired
}

func (f *fake) emit(v any) { _ = f.out.Encode(v) }

func (f *fake) garbage() {
	if f.scenario.Garbage {
		fmt.Println("not json at all")
		fmt.Println("[1, 2, 3]")
	}
}

func (f *fake) work() error {
	if f.scenario.Background {
		bg := exec.Command("sleep", "300")
		if f.scenario.HoldStdout {
			bg.Stdout = os.Stdout
		}
		_ = bg.Start()
		pidFile := filepath.Join(f.config, "background.pid")
		_ = os.WriteFile(pidFile+".tmp", []byte(fmt.Sprint(bg.Process.Pid)), 0o600)
		_ = os.Rename(pidFile+".tmp", pidFile)
	}
	if f.scenario.SleepMS > 0 {
		defer signal.Stop(f.term)
		timer := time.NewTimer(time.Duration(f.scenario.SleepMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case sig := <-f.term:
			f.writeOnTerm()
			return fmt.Errorf("the tool was interrupted by %v", sig)
		}
	}
	return nil
}

func (f *fake) writeOnTerm() {
	if f.scenario.TermWriteMS <= 0 {
		return
	}
	time.Sleep(time.Duration(f.scenario.TermWriteMS) * time.Millisecond)
	_ = os.MkdirAll(f.config, 0o700)
	_ = os.WriteFile(filepath.Join(f.config, LateWrite), []byte("saved on the way out"), 0o600)
}

func (f *fake) exitOnTerm() {
	if f.scenario.TermWriteMS <= 0 {
		return
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-interrupted
		f.writeOnTerm()
		os.Exit(1)
	}()
}

const (
	LateWrite      = "late-write"
	StatusStarted  = "status.started"
	StatusFinished = "status.finished"
	LogoutStarted  = "logout.started"
	LogoutFinished = "logout.finished"
)

func (f *fake) answer(prompt, session string) string {
	answer := "echo: " + prompt
	if f.scenario.Answer != "" {
		answer = f.scenario.Answer
	}
	if u := f.scenario.Tool; u != nil {
		answer += "\ntool " + u.Name + ": " + f.useTool(session, *u)
	}
	for i, c := range f.scenario.MCP {
		answer += "\nmcp " + c.Tool + ": " + f.callTool(session, i, c)
	}
	for _, p := range f.scenario.Probes {
		if session, ok := strings.CutPrefix(p, "ask:"); ok {
			answer += "\nprobe " + p + ": " + f.useTool(session, ToolUse{Name: "Bash", Input: map[string]any{"command": "touch stolen"}})
			continue
		}
		answer += "\nprobe " + p + ": " + probe(p)
	}
	return answer
}

func probe(p string) string {
	kind, arg, _ := strings.Cut(p, ":")
	outcome := func(err error, ok string) string {
		if err != nil {
			return "error " + err.Error()
		}
		return "ok " + ok
	}
	switch kind {
	case "read":
		b, err := os.ReadFile(arg)
		return outcome(err, strings.TrimSpace(string(b)))
	case "list":
		entries, err := os.ReadDir(arg)
		return outcome(err, fmt.Sprint(len(entries)))
	case "write":
		return outcome(os.WriteFile(arg, []byte("probe"), 0o600), "")
	case "kill":
		var pid int
		_, _ = fmt.Sscan(arg, &pid)
		return outcome(syscall.Kill(pid, syscall.SIGKILL), "")
	case "caps":
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return outcome(err, "")
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "CapPrm:") {
				arg += strings.Join(strings.Fields(line), " ") + " "
			}
		}
		return "ok " + strings.TrimSpace(arg)
	case "nnp":
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return outcome(err, "")
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
				return "ok " + strings.TrimSpace(v)
			}
		}
		return "ok unknown"
	case "uid":
		return fmt.Sprintf("ok %d %d", os.Getuid(), os.Getgid())
	case "connect":
		conn, err := net.DialTimeout("unix", arg, 2*time.Second)
		if err != nil {
			return outcome(err, "")
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 12)
		n, _ := io.ReadFull(conn, b)
		return "ok " + strings.TrimSpace(string(b[:n]))
	case "x11", "x11abstract":
		addr := arg
		if kind == "x11abstract" {
			addr = "@" + arg
		}
		conn, err := net.DialTimeout("unix", addr, 2*time.Second)
		if err != nil {
			return outcome(err, "")
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
			return outcome(err, "")
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(conn, b); err != nil {
			return outcome(err, "")
		}
		return map[byte]string{0: "ok refused", 1: "ok accepted", 2: "ok authenticate"}[b[0]]
	}
	return "error unknown probe"
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (f *fake) session(id string, create bool) (prompts []string, err error) {
	path := filepath.Join(f.config, "sessions", id+".json")
	b, err := os.ReadFile(path)
	switch {
	case err == nil && create:
		return nil, fmt.Errorf("session ID %s is already in use", id)
	case err != nil && !create:
		switch f.name {
		case "claude":
			return nil, fmt.Errorf("No conversation found with session ID: %s", id)
		case "codex":
			return nil, fmt.Errorf("thread/resume: thread/resume failed: no rollout found for thread id %s (code -32600)", id)
		}
		return nil, fmt.Errorf("session %s not found", id)
	case err == nil:
		_ = json.Unmarshal(b, &prompts)
	}
	return prompts, nil
}

func (f *fake) saveSession(id string, prompts []string) {
	_ = os.MkdirAll(filepath.Join(f.config, "sessions"), 0o700)
	b, _ := json.Marshal(prompts)
	_ = os.WriteFile(filepath.Join(f.config, "sessions", id+".json"), b, 0o600)
}

func (f *fake) streamArgs() (prompt, session string, resume, ok bool) {
	a := f.args
	val := func(flag string) string {
		if i := slices.Index(a, flag); i >= 0 && i+1 < len(a) {
			return a[i+1]
		}
		return ""
	}
	if f.name == "claude" {
		if !slices.Contains(a, "-p") || val("--output-format") != "stream-json" {
			return "", "", false, false
		}
		b, _ := io.ReadAll(os.Stdin)
		prompt = string(b)
		if s := val("--resume"); s != "" {
			return prompt, s, true, true
		}
		return prompt, val("--session-id"), false, val("--session-id") != ""
	}
	file := val("--prompt-file")
	if file == "" || val("--output-format") != "streaming-messages-json" {
		return "", "", false, false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", "", false, false
	}
	if s := val("-r"); s != "" {
		return string(b), s, true, true
	}
	return string(b), val("-s"), false, val("-s") != ""
}

func (f *fake) streamTurn(prompt, session string, resume bool) int {
	f.record(prompt)
	prompts, err := f.session(session, !resume)
	if err != nil {
		f.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error(), "session_id": session})
		return 1
	}
	f.emit(map[string]any{"type": "system", "subtype": "init", "session_id": session, "cwd": mustWD(), "model": "fake"})
	f.garbage()
	if !f.signedIn() {
		for attempt := 1; attempt <= 10; attempt++ {
			f.emit(map[string]any{"type": "system", "subtype": "api_retry", "attempt": attempt, "max_retries": 10, "error_status": 401, "error": "authentication_failed", "session_id": session})
			time.Sleep(time.Second)
		}
		f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "api_error_status": 401, "result": "Invalid API key · Please run /login", "session_id": session})
		return 1
	}
	prompts = append(prompts, prompt)
	f.saveSession(session, prompts)
	if f.scenario.SleepMS > 0 || f.scenario.Background {
		f.emit(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "sleep"}},
		}}})
		if err := f.work(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f.name, err)
			return 1
		}
		f.emit(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "slept", "is_error": false},
		}}})
	}
	if f.scenario.RateLimits {
		f.emit(map[string]any{"type": "rate_limit_event", "session_id": session, "rate_limit_info": map[string]any{
			"status": "allowed_warning", "rateLimitType": "five_hour", "utilization": 0.8, "resetsAt": time.Now().Add(time.Hour).Unix(),
		}})
	}
	if f.scenario.Fail != "" {
		f.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": f.scenario.Fail, "session_id": session})
		return 1
	}
	answer := f.answer(prompt, session)
	if len(prompts) > 1 {
		answer += fmt.Sprintf(" (turn %d of this session)", len(prompts))
	}
	f.emit(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "thinking about it"},
		map[string]any{"type": "text", "text": answer},
	}}})
	f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": answer, "session_id": session,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
	return 0
}

func (f *fake) codexExec(args []string) int {
	var model, resumeID string
	rest := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "--skip-git-repo-check", "--dangerously-bypass-hook-trust":
		case "--model":
			i++
			if i < len(args) {
				model = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	_ = model
	if len(rest) == 3 && rest[0] == "resume" {
		resumeID, rest = rest[1], rest[2:]
	}
	if len(rest) != 1 {
		fmt.Fprintf(os.Stderr, "codex exec: unsupported arguments %q\n", args)
		return 2
	}
	prompt := rest[0]
	if prompt == "-" {
		fmt.Fprintln(os.Stderr, "Reading prompt from stdin...")
		b, _ := io.ReadAll(os.Stdin)
		prompt = string(b)
	}
	f.record(prompt)
	thread := resumeID
	var prompts []string
	if resumeID != "" {
		var err error
		if prompts, err = f.session(resumeID, false); err != nil {
			fmt.Fprintln(os.Stderr, "Error: "+err.Error())
			return 1
		}
	} else {
		thread = uuid()
	}
	f.emit(map[string]any{"type": "thread.started", "thread_id": thread})
	f.garbage()
	f.emit(map[string]any{"type": "turn.started"})
	if !f.signedIn() {
		msg := "unexpected status 401 Unauthorized: Invalid API key, url: https://api.example.com/v1/responses"
		for i := 1; i <= 5; i++ {
			f.emit(map[string]any{"type": "error", "message": fmt.Sprintf("Reconnecting... %d/5 (%s)", i, msg)})
			time.Sleep(500 * time.Millisecond)
		}
		f.emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": msg}})
		return 1
	}
	prompts = append(prompts, prompt)
	f.saveSession(thread, prompts)
	if f.scenario.SleepMS > 0 || f.scenario.Background {
		item := map[string]any{"id": "item_1", "type": "command_execution", "command": "/bin/sh -lc 'sleep'", "aggregated_output": "", "exit_code": nil, "status": "in_progress"}
		f.emit(map[string]any{"type": "item.started", "item": item})
		if err := f.work(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f.name, err)
			return 1
		}
		item["status"], item["exit_code"], item["aggregated_output"] = "completed", 0, "slept\n"
		f.emit(map[string]any{"type": "item.completed", "item": item})
	}
	f.writeRollout(thread)
	if f.scenario.Fail != "" {
		f.emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": f.scenario.Fail}})
		return 1
	}
	answer := f.answer(prompt, thread)
	if len(prompts) > 1 {
		answer += fmt.Sprintf(" (turn %d of this session)", len(prompts))
	}
	f.emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_2", "type": "reasoning", "text": "thinking about it"}})
	f.emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_3", "type": "agent_message", "text": answer}})
	f.emit(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
	return 0
}

func (f *fake) writeRollout(thread string) {
	dir := filepath.Join(f.config, "sessions", "2026", "11", "19")
	_ = os.MkdirAll(dir, 0o700)
	var limits any
	if f.scenario.RateLimits {
		limits = map[string]any{
			"limit_id": "codex",
			"primary":  map[string]any{"used_percent": 42.5, "window_minutes": 300, "resets_at": time.Now().Add(2 * time.Hour).Unix()},
			"secondary": map[string]any{"used_percent": 7.0, "window_minutes": 10080,
				"resets_at": time.Now().Add(72 * time.Hour).Unix()},
		}
	} else {
		limits = map[string]any{"limit_id": "codex", "primary": nil, "secondary": nil}
	}
	line, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": nil, "rate_limits": limits}})
	file, err := os.OpenFile(filepath.Join(dir, "rollout-2026-11-19T10-00-00-"+thread+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = file.Write(append(line, '\n'))
		_ = file.Close()
	}
}

func mustWD() string {
	d, _ := os.Getwd()
	return strings.TrimSpace(d)
}

const GoodCode = "fake-code-0000"

func Approve(t testing.TB, configDir string) { approve(t, configDir, "approve") }

func Deny(t testing.TB, configDir string) { approve(t, configDir, "deny") }

func approve(t testing.TB, configDir, verdict string) {
	t.Helper()
	f, err := os.CreateTemp(configDir, ".device-verdict-*")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(verdict)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(configDir, "device-verdict"))
	}
	if err != nil {
		_ = os.Remove(f.Name())
		t.Fatal(err)
	}
}

func (f *fake) claudeAuth(cmd string) int {
	creds := filepath.Join(f.config, credentials["claude"])
	switch cmd {
	case "login":
		f.exitOnTerm()
		fmt.Println("Opening browser to sign in…")
		fmt.Println("If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-4000-8000-000000000000" +
			"&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&code_challenge=fake&code_challenge_method=S256&state=fake")
		fmt.Print("Paste code here if prompted > ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Println("\nLogin cancelled.")
			return 1
		}
		if strings.TrimSpace(line) != GoodCode {
			fmt.Println("\nOAuth error: Invalid code")
			return 1
		}
		if err := os.WriteFile(creds, []byte(`{"fake":true}`), 0o600); err != nil {
			return 1
		}
		fmt.Println("\nLogin successful.")
		return 0
	case "status":
		if f.scenario.Warn != "" {
			fmt.Fprintln(os.Stderr, f.scenario.Warn)
		}
		if f.scenario.StatusSleepMS > 0 {
			_ = os.WriteFile(filepath.Join(f.config, StatusStarted), nil, 0o600)
			time.Sleep(time.Duration(f.scenario.StatusSleepMS) * time.Millisecond)
			_ = os.MkdirAll(f.config, 0o700)
			_ = os.WriteFile(filepath.Join(f.config, StatusFinished), nil, 0o600)
		}
		_, err := os.Stat(creds)
		method := "none"
		if err == nil {
			method = "claude.ai"
		}
		f.emit(map[string]any{"loggedIn": err == nil, "authMethod": method, "apiProvider": "firstParty", "configDirectory": f.config})
		if err != nil {
			return 1
		}
		return 0
	case "logout":
		if f.scenario.LogoutSleepMS > 0 {
			_ = os.WriteFile(filepath.Join(f.config, LogoutStarted), nil, 0o600)
			time.Sleep(time.Duration(f.scenario.LogoutSleepMS) * time.Millisecond)
			_ = os.MkdirAll(f.config, 0o700)
			_ = os.WriteFile(filepath.Join(f.config, LogoutFinished), nil, 0o600)
		}
		_ = os.Remove(creds)
		fmt.Println("Successfully logged out from your Anthropic account.")
		return 0
	}
	return 2
}

func (f *fake) deviceAuth(args []string) int {
	creds := filepath.Join(f.config, credentials[f.name])
	switch {
	case args[0] == "logout":
		_ = os.Remove(creds)
		fmt.Println("Successfully logged out")
		return 0
	case len(args) == 2 && args[1] == "status" && f.name == "codex":
		if _, err := os.Stat(creds); err != nil {
			fmt.Fprintln(os.Stderr, "Not logged in")
			return 1
		}
		fmt.Fprintln(os.Stderr, "Logged in using ChatGPT")
		return 0
	case len(args) == 2 && args[1] == "--device-auth":
	default:
		return 2
	}
	if f.name == "codex" {
		fmt.Println("\nFollow these steps to sign in with ChatGPT using device code authorization:")
		fmt.Println("\n1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m")
		fmt.Println("\n2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mFAKE-C0DE1\x1b[0m")
		fmt.Println("\n\x1b[90mDevice codes are a common phishing target. Never share this code.\x1b[0m")
	} else {
		fmt.Println("\nTo sign in, open this URL in your browser:\n\n  https://accounts.x.ai/oauth2/device?user_code=GR0K-FAKE")
		fmt.Println("\n  (Could not open browser automatically — open the URL above manually.)\n\nConfirm this code in your browser:\n\n  GR0K-FAKE")
		fmt.Println("\n\x1b[90mOnly continue with a code you requested. Don't share it with anyone.\x1b[0m\n\nWaiting for authorization...")
	}
	f.exitOnTerm()
	verdict := filepath.Join(f.config, "device-verdict")
	for {
		b, err := os.ReadFile(verdict)
		if err == nil {
			_ = os.Remove(verdict)
			if strings.TrimSpace(string(b)) != "approve" {
				fmt.Println("Error logging in: the device code was declined")
				return 1
			}
			if err := os.WriteFile(creds, []byte(`{"fake":true}`), 0o600); err != nil {
				return 1
			}
			fmt.Println("Successfully logged in")
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type ToolUse struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

var claudeAsksFor = func(tool string) bool {
	return !slices.Contains([]string{"Read", "Glob", "Grep", "LS"}, tool)
}

func (f *fake) useTool(session string, u ToolUse) string {
	dir, _ := os.Getwd()
	input, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "session_id": session, "tool_name": u.Name, "tool_input": u.Input, "cwd": dir,
	})
	var reply []byte
	var err error
	switch f.name {
	case "claude":
		if f.flag("--permission-mode") == "bypassPermissions" || !claudeAsksFor(u.Name) {
			return "ran"
		}
		if f.flag("--setting-sources") != "user" {
			return "denied: no permission handler"
		}
		command := f.jsonHook("settings.json", "PermissionRequest")
		if command == "" {
			return "denied: no permission handler"
		}
		input = []byte(strings.Replace(string(input), `"PreToolUse"`, `"PermissionRequest"`, 1))
		reply, err = runHook(exec.Command("/bin/sh", "-c", command), input)
	case "codex":
		command := f.jsonHook("hooks.json", "PreToolUse")
		if command == "" || !slices.Contains(f.args, "--dangerously-bypass-hook-trust") {
			return "ran"
		}
		cmd := exec.Command("/bin/sh", "-c", command)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		cmd.Stdin = strings.NewReader(string(input))
		reply, err = cmd.Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			if strings.TrimSpace(stderr.String()) == "" {
				return "ran"
			}
			return "denied: " + strings.TrimSpace(stderr.String())
		}
	default:
		cmd := f.tomlHook()
		if cmd == nil {
			return "ran"
		}
		reply, err = runHook(cmd, input)
	}
	var out struct {
		Specific struct {
			Decision struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
			PermissionDecision string `json:"permissionDecision"`
			Reason             string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if jerr := json.Unmarshal(reply, &out); jerr != nil {
		return fmt.Sprintf("denied: the hook failed: %q (%v)", reply, err)
	}
	switch {
	case out.Specific.Decision.Behavior == "allow" || out.Specific.PermissionDecision == "allow":
		return "ran"
	case out.Specific.Decision.Behavior == "deny":
		return "denied: " + out.Specific.Decision.Message
	default:
		return "denied: " + out.Specific.Reason
	}
}

func (f *fake) flag(name string) string {
	for i, a := range f.args {
		if a == name && i+1 < len(f.args) {
			return f.args[i+1]
		}
	}
	return ""
}

func (f *fake) jsonHook(file, event string) string {
	b, err := os.ReadFile(filepath.Join(f.config, file))
	if err != nil {
		return ""
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &settings) != nil || len(settings.Hooks[event]) == 0 || len(settings.Hooks[event][0].Hooks) == 0 {
		return ""
	}
	return settings.Hooks[event][0].Hooks[0].Command
}

var (
	tomlCommand = regexp.MustCompile(`command = ("(?:[^"\\]|\\.)*")`)
	tomlArgs    = regexp.MustCompile(`args = \[([^\]]*)\]`)
)

func (f *fake) tomlHook() *exec.Cmd {
	b, err := os.ReadFile(filepath.Join(f.config, "config.toml"))
	if err != nil || !strings.Contains(string(b), "[[hooks.PreToolUse]]") || !strings.Contains(string(b), "failClosed = true") {
		return nil
	}
	var command string
	var args []string
	if m := tomlCommand.FindSubmatch(b); m == nil || json.Unmarshal(m[1], &command) != nil {
		return nil
	}
	if m := tomlArgs.FindSubmatch(b); m == nil || json.Unmarshal(append(append([]byte("["), m[1]...), ']'), &args) != nil {
		return nil
	}
	return exec.Command(command, args...)
}

func runHook(cmd *exec.Cmd, input []byte) ([]byte, error) {
	cmd.Stdin = strings.NewReader(string(input))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

type toolsServer struct {
	url, auth      string
	codex, approve bool
}

var (
	tomlKey    = regexp.MustCompile(`^([a-z_]+) = ("(?:[^"\\]|\\.)*")$`)
	tomlHeader = regexp.MustCompile(`^headers = \{ Authorization = ("(?:[^"\\]|\\.)*") \}$`)
)

func (f *fake) tools() (toolsServer, error) {
	var s toolsServer
	if f.name == "claude" {
		file := f.flag("--mcp-config")
		if file == "" || !slices.Contains(f.args, "--strict-mcp-config") {
			return s, errors.New("no MCP settings")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return s, err
		}
		var settings struct {
			Servers map[string]struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &settings); err != nil {
			return s, err
		}
		srv, ok := settings.Servers["taskto"]
		if !ok || srv.Type != "http" {
			return s, errors.New("no taskto server")
		}
		return toolsServer{url: srv.URL, auth: os.ExpandEnv(srv.Headers["Authorization"])}, nil
	}
	b, err := os.ReadFile(filepath.Join(f.config, "config.toml"))
	if err != nil {
		return s, err
	}
	in := false
	tokenVar := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == "[mcp_servers.taskto]"
			continue
		}
		if !in {
			continue
		}
		if m := tomlHeader.FindStringSubmatch(line); m != nil {
			var v string
			_ = json.Unmarshal([]byte(m[1]), &v)
			s.auth = os.ExpandEnv(v)
		}
		m := tomlKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var v string
		_ = json.Unmarshal([]byte(m[2]), &v)
		switch m[1] {
		case "url":
			s.url = v
		case "bearer_token_env_var":
			tokenVar = v
		case "default_tools_approval_mode":
			s.approve = v == "approve"
		}
	}
	if s.codex = f.name == "codex"; s.codex && tokenVar != "" {
		s.auth = "Bearer " + os.Getenv(tokenVar)
	}
	if s.url == "" {
		return s, errors.New("no taskto server")
	}
	return s, nil
}

func (f *fake) callTool(session string, i int, c MCPCall) string {
	srv, err := f.tools()
	if err != nil {
		return "error " + err.Error()
	}
	args := c.Args
	if args == nil {
		args = map[string]any{}
	}
	use := ToolUse{Name: "mcp__taskto__" + c.Tool, Input: args}
	if f.name == "grok" {
		use = ToolUse{Name: "use_tool", Input: map[string]any{"tool_name": "taskto__" + c.Tool, "tool_input": args}}
	}
	id := fmt.Sprintf("mcp-%d", i+1)
	f.showTool(id, c, use, "in_progress")
	if verdict := f.useTool(session, use); verdict != "ran" {
		f.showTool(id, c, use, "failed")
		return verdict
	}
	out := srv.call(c)
	status := "completed"
	if !strings.HasPrefix(out, "ok ") {
		status = "failed"
	}
	f.showTool(id, c, use, status)
	return out
}

func (f *fake) showTool(id string, c MCPCall, use ToolUse, status string) {
	if f.name == "codex" {
		event := "item.completed"
		if status == "in_progress" {
			event = "item.started"
		}
		f.emit(map[string]any{"type": event, "item": map[string]any{
			"id": id, "type": "mcp_tool_call", "server": "taskto", "tool": c.Tool, "arguments": use.Input, "status": status,
		}})
		return
	}
	if status == "in_progress" {
		f.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": use.Name, "input": use.Input},
		}}})
		return
	}
	f.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": status, "is_error": status != "completed"},
	}}})
}

func (s toolsServer) call(c MCPCall) string {
	if _, err := s.rpc(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fake", "version": "1"},
	}); err != nil {
		return "error " + err.Error()
	}
	if s.codex && !s.approve {
		res, err := s.rpc(2, "tools/list", map[string]any{})
		if err != nil {
			return "error " + err.Error()
		}
		var list struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations struct {
					ReadOnly bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(res, &list)
		for _, t := range list.Tools {
			if t.Name == c.Tool && !t.Annotations.ReadOnly {
				return "error MCP tool call requires approval, but approval policy is never"
			}
		}
	}
	args := c.Args
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.rpc(3, "tools/call", map[string]any{"name": c.Tool, "arguments": args})
	if err != nil {
		return "error " + err.Error()
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil || len(out.Content) == 0 {
		return "error unreadable result"
	}
	text := out.Content[0].Text
	if len(text) > 4000 {
		text = text[:4000]
	}
	if out.IsError {
		return "refused " + text
	}
	return "ok " + text
}

func (s toolsServer) rpc(id int, method string, params any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if s.auth != "" {
		req.Header.Set("Authorization", s.auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		return nil, fmt.Errorf("unreadable reply: %q", b)
	}
	if msg.Error != nil {
		return nil, errors.New(msg.Error.Message)
	}
	return msg.Result, nil
}
