//go:build unix

// Package clitest provides fake Claude Code, Codex and Grok Build CLIs for tests. They print
// what the real CLIs print (the event streams recorded in ../testdata), so the worker, the
// server and the sidecar can be tested without a subscription or a network.
//
// The fakes are the test binary itself, run under the CLIs' names: call Main first thing in
// TestMain, and Install to get the executables.
//
//	func TestMain(m *testing.M) {
//		clitest.Main()
//		os.Exit(m.Run())
//	}
//
// What a fake does is set per user and CLI by a Scenario in the CLI's config directory
// (WriteScenario). A fake is signed in when its credentials file exists there (SignIn), as
// the real CLIs keep their logins. Every run appends a Call to calls.jsonl there, so tests can
// check what the worker passed: arguments, environment, working directory and prompt.
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
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
)

// Scenario is what a fake does on its next turns.
type Scenario struct {
	// Answer is the final answer; empty answers "echo: <prompt>".
	Answer string `json:"answer,omitempty"`
	// SleepMS makes the turn run a tool for that long before it answers.
	SleepMS int `json:"sleep_ms,omitempty"`
	// Background starts a process that outlives the CLI, in its process group.
	Background bool `json:"background,omitempty"`
	// HoldStdout makes the background process keep the CLI's stdout open.
	HoldStdout bool `json:"hold_stdout,omitempty"`
	// Expired makes the login fail the way each CLI reports it.
	Expired bool `json:"expired,omitempty"`
	// RateLimits reports usage limits (Claude Code's rate_limit_event, Codex's rollout).
	RateLimits bool `json:"rate_limits,omitempty"`
	// Fail makes the turn fail with this message.
	Fail string `json:"fail,omitempty"`
	// Garbage prints lines that aren't JSON among the events.
	Garbage bool `json:"garbage,omitempty"`
	// Tool makes the fake use a tool before it answers, asking its permission hook first the
	// way the real CLI does, and report "tool <name>: ran" or "tool <name>: denied: <why>".
	Tool *ToolUse `json:"tool,omitempty"`
	// Probes are things the fake tries before it answers, reporting each outcome in its
	// answer, as a command run by a model might: "read:<path>", "list:<dir>",
	// "write:<path>", "kill:<pid>", "caps" (its effective capabilities), "nnp" (whether
	// no_new_privs is set, Linux), "uid", and "ask:<session>" (its permission hook, asked
	// about that session).
	Probes []string `json:"probes,omitempty"`
	// MCP makes the fake call TaskTo's tools, as the real CLI would: at the endpoint, with the
	// token, of the MCP settings the worker wrote, after asking its permission hook. Each call
	// is reported in the answer as "mcp <tool>: ok <result>", "mcp <tool>: refused <message>",
	// "mcp <tool>: denied: <why>" or "mcp <tool>: error <what>".
	MCP []MCPCall `json:"mcp,omitempty"`
}

// MCPCall is a call of one of TaskTo's tools by a fake (Scenario.MCP).
type MCPCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Call is one run of a fake, as calls.jsonl records it.
type Call struct {
	UID    int      `json:"uid"`
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Dir    string   `json:"dir"`
	Prompt string   `json:"prompt"`
	PID    int      `json:"pid"`
}

var names = map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME", "grok": "GROK_HOME"}

// credentials is where each CLI keeps its login, relative to its config directory.
var credentials = map[string]string{"claude": ".credentials.json", "codex": "auth.json", "grok": "auth.json"}

// Main runs a fake CLI and exits if this process was started as one, or the worker's helper
// (cliworker.RunHelper) or permission hook (cliworker.RunApprove) if it was started as that;
// otherwise it returns.
func Main() {
	if len(os.Args) > 1 && os.Args[1] == cliworker.HelperArg {
		exit(cliworker.RunHelper())
	}
	if len(os.Args) > 1 && os.Args[1] == cliworker.ApproveArg {
		exit(cliworker.RunApprove(os.Args[2:]))
	}
	name := filepath.Base(os.Args[0])
	if _, ok := names[name]; !ok {
		return
	}
	exit(run(name, os.Args[1:]))
}

// exit ends a process that is a fake, the helper or the permission hook. It is not os.Exit:
// in a test binary built with -cover, os.Exit prints "warning: GOCOVERDIR not set, no
// coverage data emitted" on stderr, and the worker reads a command's stderr along with its
// stdout (the helper's JSON answers, a CLI's status), so under `go test -cover` every one of
// these processes would spoil its own output. These processes have no coverage directory to
// write to, so nothing is lost by leaving without the coverage exit hook.
func exit(code int) {
	syscall.Exit(code)
}

// Install puts the fakes in dir and returns their paths, by CLI name.
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

// WriteScenario sets what the fake cli does for the user whose config directory for it is
// configDir.
func WriteScenario(t testing.TB, configDir string, s Scenario) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(configDir, "scenario.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// SignIn makes the fake cli signed in for the config directory.
func SignIn(t testing.TB, cli, configDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, credentials[cli]), []byte(`{"fake":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Calls returns the fake's runs recorded in configDir.
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

// keyVars are the variables that give each CLI an API key, as a gateway or the server's proxy
// for a shared account does: with one set, the real CLI needs no login.
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

// work runs the scenario's tool: a sleep (as a real sleep process, so stopping the turn has
// a child to stop) and optionally a process that outlives the CLI.
func (f *fake) work() {
	if f.scenario.Background {
		bg := exec.Command("sleep", "300")
		if f.scenario.HoldStdout {
			bg.Stdout = os.Stdout
		}
		_ = bg.Start()
		_ = os.WriteFile(filepath.Join(f.config, "background.pid"), []byte(fmt.Sprint(bg.Process.Pid)), 0o600)
	}
	if f.scenario.SleepMS > 0 {
		_ = exec.Command("sleep", fmt.Sprintf("%.3f", float64(f.scenario.SleepMS)/1000)).Run()
	}
}

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
			// Ask the permission hook about someone else's session, as a command might.
			answer += "\nprobe " + p + ": " + f.useTool(session, ToolUse{Name: "Bash", Input: map[string]any{"command": "touch stolen"}})
			continue
		}
		answer += "\nprobe " + p + ": " + probe(p)
	}
	return answer
}

// probe tries what p says and reports the outcome: "ok …" or the error.
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
		// A Unix socket: what it says first (a VNC server's banner).
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
		// An X display's socket, filesystem or abstract: the server's answer to a connection
		// setup without a cookie.
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
		// Little-endian, protocol 11.0, no authorization.
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

// sessions keeps each session's prompts, so a resume of an unknown session fails as the real
// CLIs do, and a resumed answer can show it remembers.
func (f *fake) session(id string, create bool) (prompts []string, err error) {
	path := filepath.Join(f.config, "sessions", id+".json")
	b, err := os.ReadFile(path)
	switch {
	case err == nil && create:
		return nil, fmt.Errorf("session ID %s is already in use", id)
	case err != nil && !create:
		// Each CLI's own words, as recorded from the real ones (Grok Build's from its guide).
		switch f.name {
		case "claude":
			return nil, fmt.Errorf("No conversation found with session ID: %s", id) //nolint:staticcheck // Claude Code's own capitalised message
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

// streamArgs reads Claude Code's and Grok Build's headless flags.
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
		// As recorded from Claude Code 2.1.311: a 401 is retried with backoff, for minutes.
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
		f.work()
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
		b, _ := io.ReadAll(os.Stdin) // the real Codex waits here until stdin closes
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
		// As recorded from Codex 0.169.0 against a gateway that refused the key.
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
		f.work()
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

// writeRollout writes the session file Codex keeps, with the usage limits it records there.
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

// GoodCode is the code the fake Claude Code login accepts; any other is refused.
const GoodCode = "fake-code-0000"

// Approve finishes a fake Codex or Grok Build device-code login waiting in configDir, as the
// user would on the provider's page.
func Approve(t testing.TB, configDir string) { approve(t, configDir, "approve") }

// Deny refuses a fake device-code login waiting in configDir.
func Deny(t testing.TB, configDir string) { approve(t, configDir, "deny") }

// approve writes the verdict the waiting fake polls for, whole and at once (a file renamed
// into place): written in place, the fake could read it created but still empty, and take
// that for a refusal.
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

// claudeAuth is `claude auth login|status|logout`, printing what Claude Code 2.1.311 prints.
func (f *fake) claudeAuth(cmd string) int {
	creds := filepath.Join(f.config, credentials["claude"])
	switch cmd {
	case "login":
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
		_, err := os.Stat(creds)
		method := "none"
		if err == nil {
			method = "claude.ai"
		}
		f.emit(map[string]any{"loggedIn": err == nil, "authMethod": method, "apiProvider": "firstParty", "configDirectory": f.config})
		if err != nil {
			return 1 // as Claude Code 2.1.311 does on Linux when signed out
		}
		return 0
	case "logout":
		_ = os.Remove(creds)
		fmt.Println("Successfully logged out from your Anthropic account.")
		return 0
	}
	return 2
}

// deviceAuth is Codex's and Grok Build's `login --device-auth`, `login status` and `logout`.
// The device-code login waits for Approve or Deny, polling as the real ones do.
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
		// As Codex 0.169.0 prints it (its binary's strings).
		fmt.Println("\nFollow these steps to sign in with ChatGPT using device code authorization:")
		fmt.Println("\n1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m")
		fmt.Println("\n2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mFAKE-C0DE1\x1b[0m")
		fmt.Println("\n\x1b[90mDevice codes are a common phishing target. Never share this code.\x1b[0m")
	} else {
		// As Grok Build 1.0.89 prints it (recorded).
		fmt.Println("\nTo sign in, open this URL in your browser:\n\n  https://accounts.x.ai/oauth2/device?user_code=GR0K-FAKE")
		fmt.Println("\n  (Could not open browser automatically — open the URL above manually.)\n\nConfirm this code in your browser:\n\n  GR0K-FAKE")
		fmt.Println("\n\x1b[90mOnly continue with a code you requested. Don't share it with anyone.\x1b[0m\n\nWaiting for authorization...")
	}
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

// ToolUse is a tool call of a fake.
type ToolUse struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// claudeAsksFor are the tools Claude Code's own rules let run without asking, in this fake.
var claudeAsksFor = func(tool string) bool {
	return !slices.Contains([]string{"Read", "Glob", "Grep", "LS"}, tool)
}

// useTool asks the permission hook the real CLI would ask (cliworker's, in the settings it
// wrote), and says whether the tool ran:
//   - Claude Code: its PermissionRequest hook, in settings.json, unless it runs with
//     bypassPermissions or its own rules allow the tool; and only with --setting-sources
//     user, as the user settings are where the hook is;
//   - Codex: its PreToolUse hook, in hooks.json, only with --dangerously-bypass-hook-trust;
//   - Grok Build: its PreToolUse hook, in config.toml, failing closed.
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
		// As Codex 0.169 does: a hook that exits 2 with nothing on stderr is ignored, its JSON
		// too, and the tool runs.
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
	// A reply decides whatever the exit code; a hook that failed without one blocks (exit 2
	// for Claude Code and Codex, failClosed for Grok Build).
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

// flag returns the value after name in the fake's arguments.
func (f *fake) flag(name string) string {
	for i, a := range f.args {
		if a == name && i+1 < len(f.args) {
			return f.args[i+1]
		}
	}
	return ""
}

// jsonHook returns the command of the first hook for event in the config directory's file.
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

// tomlHook returns Grok Build's PreToolUse hook from config.toml as a command, if any.
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
	return exec.Command(command, args...) //nolint:gosec // G204: the worker's own hook, in a test
}

// runHook runs a hook with input on its stdin and returns its stdout.
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

// The fakes' MCP client: what each real CLI does with the settings the worker wrote for
// TaskTo's tools (docs/cli-tools.md), as checked against the real Claude Code and Codex.

// toolsServer is how the fake finds TaskTo's MCP server: its URL and Authorization header,
// and for Codex whether its own approval lets a tool without readOnlyHint run.
type toolsServer struct {
	url, auth      string
	codex, approve bool
}

var (
	tomlKey    = regexp.MustCompile(`^([a-z_]+) = ("(?:[^"\\]|\\.)*")$`)
	tomlHeader = regexp.MustCompile(`^headers = \{ Authorization = ("(?:[^"\\]|\\.)*") \}$`)
)

// tools reads the fake's MCP settings for the server "taskto": Claude Code's --mcp-config
// file, Codex's and Grok Build's [mcp_servers.taskto] table in config.toml. Variables in a
// header are expanded from the environment, as Claude Code and Grok Build do; Codex reads its
// bearer_token_env_var.
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

// callTool calls TaskTo's tool c.Tool, as the i-th tool call of the turn: it shows the call in
// the CLI's events, asks the permission hook under the CLI's name for it, and calls the
// endpoint.
func (f *fake) callTool(session string, i int, c MCPCall) string {
	srv, err := f.tools()
	if err != nil {
		return "error " + err.Error()
	}
	args := c.Args
	if args == nil {
		args = map[string]any{}
	}
	// The names each CLI gives the tool: Claude Code and Codex mcp__taskto__<tool>; Grok Build
	// calls it through its use_tool meta-tool.
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

// showTool prints a tool call's event as the CLI does.
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

// call runs c on the server: initialize, then (for Codex, to read the tool's readOnlyHint)
// tools/list, then tools/call.
func (s toolsServer) call(c MCPCall) string {
	if _, err := s.rpc(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fake", "version": "1"},
	}); err != nil {
		return "error " + err.Error()
	}
	if s.codex && !s.approve {
		// Codex's own approval: without "approve", a tool without readOnlyHint is refused
		// under approval_policy "never" (checked against Codex 0.169).
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

// rpc sends one JSON-RPC request to the server and returns its result.
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
