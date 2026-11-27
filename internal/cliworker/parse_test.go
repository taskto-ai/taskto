package cliworker

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// parseFile runs the parser over a recorded stream and returns its events.
func parseFile(t *testing.T, cli CLI, name string) ([]Event, *parser) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	p := newParser(cli, func(e Event) { events = append(events, e) })
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	return events, p
}

func types(events []Event) []EventType {
	out := make([]EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// The streams in testdata were recorded from the real CLIs (versions in the file names)
// running against a gateway, with paths and host names replaced. A CLI upgrade re-records
// them; a change in what the worker reads fails here.
func TestParseClaudeCodeTurn(t *testing.T) {
	events, p := parseFile(t, Claude, "claude-2.1.311-turn.jsonl")
	want := []EventType{EventSession, EventThinking, EventTool, EventTool, EventText, EventUsage}
	if got := types(events); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if events[0].Session != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("session = %q", events[0].Session)
	}
	call, result := events[2].Tool, events[3].Tool
	if call.Name != "Bash" || call.Status != "in_progress" || !strings.Contains(call.Input, "echo hi > a.txt") {
		t.Errorf("tool call = %+v", call)
	}
	if result.ID != call.ID || result.Name != "Bash" || result.Status != "completed" || result.Output != "hi" {
		t.Errorf("tool result = %+v", result)
	}
	if events[4].Text != "done" || p.answer != "done" || !p.result || p.failed != "" || p.login {
		t.Errorf("answer %q, result %v, failed %q, login %v", p.answer, p.result, p.failed, p.login)
	}
	if tok := events[5].Tokens; tok == nil || tok.Input == 0 || tok.Output == 0 {
		t.Errorf("tokens = %+v", tok)
	}
}

// Claude Code retries a refused login for minutes (system/api_retry, error_status 401): the
// first retry is enough to know.
func TestParseClaudeCodeLoginRefused(t *testing.T) {
	events, p := parseFile(t, Claude, "claude-2.1.311-401.jsonl")
	if got := types(events); !slices.Equal(got, []EventType{EventSession, EventLoginRequired}) {
		t.Fatalf("events = %v", got)
	}
	if !p.login {
		t.Error("login not flagged")
	}
}

func TestParseCodexTurn(t *testing.T) {
	events, p := parseFile(t, Codex, "codex-0.169.0-turn.jsonl")
	want := []EventType{EventSession, EventNotice, EventTool, EventTool, EventText, EventUsage}
	if got := types(events); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if events[0].Session != "01a1f475-29de-7321-a396-a9604888dacd" {
		t.Errorf("session = %q", events[0].Session)
	}
	if tool := events[3].Tool; tool.Name != "shell" || tool.Status != "completed" || tool.Output != "hi\n" || !strings.Contains(tool.Input, "echo hi") {
		t.Errorf("tool = %+v", tool)
	}
	if p.answer != "done" || p.failed != "" || p.login {
		t.Errorf("answer %q, failed %q, login %v", p.answer, p.failed, p.login)
	}
}

// A Codex MCP tool call (recorded against a scripted model and a stand-in for taskto's
// endpoint): the tool event names the server and the tool, with the call's arguments, which
// the sidecar reads for a browser_help (docs/cli-tools.md).
func TestParseCodexMCPToolCall(t *testing.T) {
	events, p := parseFile(t, Codex, "codex-0.169.0-mcp.jsonl")
	var tools []Tool
	for _, e := range events {
		if e.Type == EventTool {
			tools = append(tools, *e.Tool)
		}
	}
	if len(tools) != 2 || tools[0].Name != "taskto.write_doc" || tools[0].Status != "in_progress" || tools[1].Status != "completed" {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].Input != `{"path":"notes.md"}` {
		t.Errorf("input = %q", tools[0].Input)
	}
	if p.answer != "done" {
		t.Errorf("answer %q", p.answer)
	}
	// A call without arguments shows none, whether the field is absent or null.
	for _, line := range []string{
		`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"taskto","tool":"list_spaces","status":"in_progress"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"mcp_tool_call","server":"taskto","tool":"list_spaces","arguments":null,"status":"in_progress"}}`,
	} {
		var got []Event
		newParser(Codex, func(e Event) { got = append(got, e) }).line([]byte(line))
		if len(got) != 1 || got[0].Tool == nil || got[0].Tool.Input != "" {
			t.Errorf("%s: %+v", line, got)
		}
	}
}

// Codex retries a 401 five times ("Reconnecting... 1/5 (… 401 Unauthorized …)") before the
// turn fails: the first retry is enough to know.
func TestParseCodexLoginRefused(t *testing.T) {
	events, p := parseFile(t, Codex, "codex-0.169.0-401.jsonl")
	got := types(events)
	if !slices.Equal(got, []EventType{EventSession, EventNotice, EventLoginRequired}) {
		t.Fatalf("events = %v", got)
	}
	if !p.login {
		t.Error("login not flagged")
	}
}

func TestParseClaudeCodeRateLimit(t *testing.T) {
	var events []Event
	p := newParser(Claude, func(e Event) { events = append(events, e) })
	p.line([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1795000000,"rateLimitType":"seven_day","utilization":0.85},"session_id":"s"}`))
	if len(events) != 1 || len(events[0].Limits) != 1 {
		t.Fatalf("events = %+v", events)
	}
	l := events[0].Limits[0]
	if l.Window != "seven_day" || l.Status != "allowed_warning" || l.UsedPercent == nil || *l.UsedPercent != 85 || l.ResetsAt == nil || l.ResetsAt.Unix() != 1795000000 {
		t.Errorf("limit = %+v", l)
	}
}

// Without a rate_limit_event, a turn refused on a plan limit is known from its message.
func TestParseLimitMessage(t *testing.T) {
	for _, tc := range []struct {
		cli  CLI
		line string
	}{
		{Claude, `{"type":"result","subtype":"success","is_error":true,"result":"Usage limit reached · resets 3pm (Europe/Lisbon)"}`},
		{Claude, `{"type":"result","subtype":"success","is_error":true,"result":"You've reached your weekly limit · resets Mon 9am"}`},
		{Codex, `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again at 3:05 PM."}}`},
	} {
		var events []Event
		p := newParser(tc.cli, func(e Event) { events = append(events, e) })
		p.line([]byte(tc.line))
		if len(events) != 1 || len(events[0].Limits) != 1 || events[0].Limits[0].Status != "rejected" || events[0].Limits[0].Detail == "" {
			t.Errorf("%s: events = %+v", tc.line, events)
		}
		if p.failed == "" || p.login {
			t.Errorf("%s: failed %q, login %v", tc.line, p.failed, p.login)
		}
	}
}

func TestParseIgnoresNonJSON(t *testing.T) {
	var events []Event
	p := newParser(Claude, func(e Event) { events = append(events, e) })
	for _, line := range []string{"", "not json", "[1,2]", `"x"`, `{"no":"type"}`, `{"type":"unknown"}`, `{"type":"assistant","message":5}`} {
		p.line([]byte(line))
	}
	if len(events) != 0 {
		t.Errorf("events = %+v", events)
	}
}

func TestCodexLimits(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "19")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	thread := "01a1f475-29de-7321-a396-a9604888dacd"
	lines := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":300,"resets_at":1795000000},"secondary":null}}}`,
		`{"type":"response_item","payload":{"type":"message"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":12.5,"window_minutes":300,"resets_at":1795000000},"secondary":{"used_percent":3,"window_minutes":10080}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":null,"secondary":null}}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-19T10-38-01-"+thread+".jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	got := codexLimits(home, thread)
	if len(got) != 2 || got[0].Window != "primary" || *got[0].UsedPercent != 12.5 || got[0].WindowMinutes != 300 || got[1].Window != "secondary" || *got[1].UsedPercent != 3 {
		t.Fatalf("limits = %+v", got)
	}
	if codexLimits(home, "other-thread") != nil || codexLimits(home, "../../etc") != nil {
		t.Error("limits for a thread without a file")
	}
}

// A prompt never goes in argv, where one starting with "-" would read as a flag: it goes on
// stdin, or (Grok Build) in a file the helper writes.
func TestTurnCommandKeepsThePromptOutOfArgv(t *testing.T) {
	prompt := "--dangerously-skip-permissions please"
	for _, spec := range []TurnSpec{
		{CLI: Claude, Session: "s1", Prompt: prompt},
		{CLI: Claude, Session: "s1", Resume: true, Prompt: prompt, Model: "opus"},
		{CLI: Codex, Prompt: prompt},
		{CLI: Codex, Session: "t1", Resume: true, Prompt: prompt},
		{CLI: Grok, Session: "s1", Prompt: prompt},
		{CLI: Grok, Session: "s1", Resume: true, Prompt: prompt},
	} {
		c := turnCommand(spec)
		for _, a := range c.args {
			if strings.Contains(a, "please") {
				t.Errorf("%s: the prompt is in argv: %q", spec.CLI, c.args)
			}
		}
		joined := strings.Join(c.args, " ")
		if c.promptFile != (spec.CLI == Grok) || (spec.CLI == Grok && !strings.Contains(joined, "--prompt-file "+promptFilePlaceholder)) {
			t.Errorf("%s: prompt file %v, args %q", spec.CLI, c.promptFile, c.args)
		}
		switch {
		case spec.CLI == Claude && spec.Resume && !strings.Contains(joined, "--resume s1"):
		case spec.CLI == Claude && !spec.Resume && !strings.Contains(joined, "--session-id s1"):
		case spec.CLI == Codex && spec.Resume && !strings.Contains(joined, "resume t1 -"):
		case spec.CLI == Grok && spec.Resume && !strings.Contains(joined, "-r s1"):
		case spec.CLI == Grok && !spec.Resume && !strings.Contains(joined, "-s s1"):
		default:
			continue
		}
		t.Errorf("%s resume=%v: args %q", spec.CLI, spec.Resume, c.args)
	}
}

func TestTurnSpecCheck(t *testing.T) {
	ok := TurnSpec{ID: "t1", User: 1, Identity: IdentityForTest(1), CLI: Claude, Conversation: 2, Session: "s", Prompt: "hi"}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*TurnSpec){
		"bad id":             func(s *TurnSpec) { s.ID = "../x" },
		"no user":            func(s *TurnSpec) { s.User = 0 },
		"bad cli":            func(s *TurnSpec) { s.CLI = "bash" },
		"no conversation":    func(s *TurnSpec) { s.Conversation = 0 },
		"bad model":          func(s *TurnSpec) { s.Model = "a b" },
		"bad session":        func(s *TurnSpec) { s.Session = "../x" },
		"claude no session":  func(s *TurnSpec) { s.Session = "" },
		"codex new with id":  func(s *TurnSpec) { s.CLI = Codex },
		"resume no session":  func(s *TurnSpec) { s.CLI, s.Session, s.Resume = Codex, "", true },
		"empty prompt":       func(s *TurnSpec) { s.Prompt = "" },
		"prompt too long":    func(s *TurnSpec) { s.Prompt = strings.Repeat("x", maxPrompt+1) },
		"model with a space": func(s *TurnSpec) { s.Model = "--x y" },
	} {
		s := ok
		mutate(&s)
		if s.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCut(t *testing.T) {
	if got := cut("héllo wörld", 8); len(got) > 8 || !strings.HasSuffix(got, "…") {
		t.Errorf("cut = %q", got)
	}
	if got := cut("short", 8); got != "short" {
		t.Errorf("cut = %q", got)
	}
}

func TestMergeEnvReplaces(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4"})
	if !slices.Equal(got, []string{"A=1", "B=3", "C=4"}) {
		t.Errorf("env = %q", got)
	}
}

func TestAuthFailurePatterns(t *testing.T) {
	for _, msg := range []string{
		"Invalid API key · Please run /login",
		"OAuth token has expired. Please obtain a new token or refresh your existing token.",
		"Reconnecting... 1/5 (unexpected status 401 Unauthorized: Invalid API key, url: https://api.example.com/v1/responses)",
		"Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.",
		"api_error_status: authentication_failed",
		"Error: not signed in. Run `grok login`.",
	} {
		if !authFailure.MatchString(msg) {
			t.Errorf("not a login failure: %q", msg)
		}
	}
	for _, msg := range []string{
		"curl https://example.com/api returned 401",
		"the build failed after 401 seconds",
		"refactor the refresh token rotation in auth.go",
		"Usage limit reached · resets 3pm",
		"the disk is full",
	} {
		if authFailure.MatchString(msg) {
			t.Errorf("taken for a login failure: %q", msg)
		}
	}
}

func TestOneTimeCode(t *testing.T) {
	for text, want := range map[string]string{
		// Codex 0.169.0
		"Follow these steps to sign in with ChatGPT using device code authorization:\n\n1. Open this link\n   https://auth.openai.com/codex/device\n\n2. Enter this one-time code (expires in 15 minutes)\n   ABCD-EFGHI\n": "ABCD-EFGHI",
		// Grok Build 1.0.89: the code is in the URL too, but taken from its own line.
		"To sign in, open this URL in your browser:\n\n  https://accounts.x.ai/oauth2/device?user_code=S8X2-4PCV\n\nConfirm this code in your browser:\n\n  S8X2-4PCV\n": "S8X2-4PCV",
		// Noise: an uppercase token far from any mention of a code.
		"Build ABCD-1234 ready.\nNothing else.\nStill nothing.\nMore.\nAnd more.\n": "",
		"Your code:\n\n\n\n\nWXYZ-1234 (too far)\n":                                 "",
	} {
		if got := oneTimeCode(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}

// Grok Build 1.0.89, signed out (recorded from the real binary in the worker image): the
// reason is only in errors[].
func TestParseGrokNotSignedIn(t *testing.T) {
	events, p := parseFile(t, Grok, "grok-1.0.89-not-signed-in.jsonl")
	if !p.login {
		t.Fatalf("login not flagged: %+v, failed %q", events, p.failed)
	}
	if got := types(events); !slices.Contains(got, EventLoginRequired) {
		t.Fatalf("events = %v", got)
	}
}

// The app a CLI saved through taskto's create_app or update_app is named in the tool event, read
// from the whole answer even where the event's output is cut, so the conversation can show it.
func TestParseSavedApps(t *testing.T) {
	big := strings.Repeat("x", 3*maxToolText)
	answer := `{"id":7,"space_id":3,"name":"Tracker","definition":{"note":"` + big + `"}}`
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	saved := func(cli CLI, lines ...string) []*SavedApp {
		var out []*SavedApp
		p := newParser(cli, func(e Event) {
			if e.Type == EventTool && e.Tool.Status != "in_progress" {
				out = append(out, e.Tool.Saved)
			}
		})
		for _, l := range lines {
			p.line([]byte(l))
		}
		return out
	}
	claude := func(tool, input, content string, isError bool) []string {
		return []string{
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"` + tool + `","input":` + input + `}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":` + strconv.FormatBool(isError) + `,"content":` + quote(content) + `}]}}`,
		}
	}
	want := &SavedApp{Space: 3, App: 7}
	for name, c := range map[string]struct {
		cli   CLI
		lines []string
	}{
		"Claude Code create":  {Claude, claude("mcp__taskto__create_app", `{}`, answer, false)},
		"Claude Code update":  {Claude, claude("mcp__taskto__update_app", `{}`, answer, false)},
		"Grok Build":          {Grok, claude("taskto__create_app", `{}`, answer, false)},
		"Grok Build use_tool": {Grok, claude("use_tool", `{"tool_name":"taskto__update_app","tool_input":{}}`, answer, false)},
		"Codex": {Codex, []string{
			`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"taskto","tool":"create_app","arguments":{},"status":"completed","result":{"content":[{"type":"text","text":` + quote(answer) + `}]}}}`,
		}},
	} {
		got := saved(c.cli, c.lines...)
		if len(got) != 1 || got[0] == nil || *got[0] != *want {
			t.Errorf("%s: saved = %+v", name, got)
		}
	}
	for name, lines := range map[string][]string{
		"a failed save":        claude("mcp__taskto__create_app", `{}`, answer, true),
		"another tool":         claude("mcp__taskto__read_doc", `{}`, answer, false),
		"another server":       claude("mcp__other__create_app", `{}`, answer, false),
		"an answer not an app": claude("mcp__taskto__create_app", `{}`, "error: nope", false),
	} {
		if got := saved(Claude, lines...); len(got) != 1 || got[0] != nil {
			t.Errorf("%s: saved = %+v", name, got)
		}
	}
}
