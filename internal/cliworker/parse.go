package cliworker

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds on what an event carries. The CLI's own session keeps everything whole.
const (
	maxToolText  = 2 << 10  // a tool call's input or output
	maxEventText = 64 << 10 // a text, thinking or notice event
)

// parser turns a CLI's stdout, one JSON object per line, into Events. It is one turn's.
type parser struct {
	cli  CLI
	emit func(Event)

	answer string // the final answer so far
	failed string // the error the turn failed with, if any
	login  bool   // the CLI's login is missing or expired
	result bool   // a final result arrived (Claude Code and Grok)
	// activity: the CLI did something (answered, thought, ran a tool) before it ended.
	activity bool
	tools    map[string]string
	inputs   map[string]string // a tool call's input, by its ID, for the meta-tool that names its tool there
}

func newParser(cli CLI, emit func(Event)) *parser {
	p := &parser{cli: cli, tools: map[string]string{}, inputs: map[string]string{}}
	p.emit = func(e Event) {
		if e.Type == EventText || e.Type == EventThinking || e.Type == EventTool {
			p.activity = true
		}
		emit(e)
	}
	return p
}

// authFailure matches the CLIs' own messages for a missing, expired or revoked login, and for a
// rejected key (a gateway's 401): "Invalid API key · Please run /login", "OAuth token has
// expired", Codex's "unexpected status 401 Unauthorized", "refresh token was already used …
// log in again", Grok's "not signed in". It is only matched against the CLI's errors about
// its own requests (a failed turn, a retry, stderr of a failed run), never a tool's output,
// where a 401 is some other server's business.
var authFailure = regexp.MustCompile(`(?i)\b401 unauthori[sz]ed|status:? 401\b|invalid api key|please run /login|oauth token (has )?expired|(access|auth|oauth) token (has )?(expired|been revoked)|authentication_failed|not (logged|signed) in\b|(log|sign) in again|refresh token (was )?(already used|reused|expired|revoked|invalid)`)

// usageLimit matches Claude Code's (and Grok's) messages for a turn refused on a plan limit,
// the fallback when no rate_limit_event said so: "Usage limit reached · resets 3pm",
// "You've reached your … limit", "You've hit your … limit", "out of usage credits".
var usageLimit = regexp.MustCompile(`(?i)usage limit reached|you've (reached|hit) your [a-z ]{0,30}limit|out of usage credits`)

// limitReached reports a turn refused on a plan limit, known only from its message.
func (p *parser) limitReached(msg string) {
	p.emit(Event{Type: EventUsage, Limits: []Limit{{Window: "plan", Status: "rejected", Detail: cut(msg, maxToolText)}}})
}

// line handles one line of the CLI's stdout. Lines that aren't JSON objects (a CLI's stray
// output) are ignored.
func (p *parser) line(b []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &head) != nil || head.Type == "" {
		return
	}
	if p.cli == Codex {
		p.codex(head.Type, b)
	} else {
		p.streamJSON(head.Type, b)
	}
}

// loginRequired records that the login failed, once.
func (p *parser) loginRequired(detail string) {
	if p.login {
		return
	}
	p.login = true
	p.emit(Event{Type: EventLoginRequired, Text: cut(detail, maxToolText)})
}

// streamJSON reads Claude Code's stream-json, which Grok Build's streaming-messages-json
// follows too.
func (p *parser) streamJSON(typ string, b []byte) {
	switch typ {
	case "system":
		var e struct {
			Subtype     string `json:"subtype"`
			SessionID   string `json:"session_id"`
			Error       string `json:"error"`
			ErrorStatus int    `json:"error_status"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		switch e.Subtype {
		case "init":
			if e.SessionID != "" {
				p.emit(Event{Type: EventSession, Session: e.SessionID})
			}
		case "api_retry":
			// A 401 is retried for minutes; it won't get better without a new login.
			if e.Error == "authentication_failed" || e.ErrorStatus == 401 {
				p.loginRequired("the CLI's login was refused (" + e.Error + ")")
				return
			}
			p.emit(Event{Type: EventNotice, Text: "retrying the request: " + e.Error})
		}
	case "assistant":
		var e struct {
			Error   string `json:"error"`
			Message struct {
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if e.Error == "authentication_failed" {
			p.loginRequired("the CLI's login was refused")
			return
		}
		if e.Error == "rate_limit" {
			p.limitReached(contentTextOf(e.Message.Content))
		}
		for _, raw := range e.Message.Content {
			p.assistantBlock(raw)
		}
	case "user":
		var e struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return // a plain string: the prompt itself
		}
		for _, bl := range blocks {
			if bl.Type != "tool_result" {
				continue
			}
			status := "completed"
			if bl.IsError {
				status = "failed"
			}
			text := contentText(bl.Content)
			p.emit(Event{Type: EventTool, Tool: &Tool{
				ID: bl.ToolUseID, Name: p.tools[bl.ToolUseID], Status: status,
				Output: cut(text, maxToolText), Saved: savedApp(p.tools[bl.ToolUseID], p.inputs[bl.ToolUseID], text, !bl.IsError),
			}})
		}
	case "rate_limit_event":
		var e struct {
			Info struct {
				Status        string   `json:"status"`
				ResetsAt      *int64   `json:"resetsAt"`
				RateLimitType string   `json:"rateLimitType"`
				Utilization   *float64 `json:"utilization"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		l := Limit{Window: e.Info.RateLimitType, Status: e.Info.Status, ResetsAt: unixTime(e.Info.ResetsAt)}
		if l.Window == "" {
			l.Window = "unknown"
		}
		if e.Info.Utilization != nil {
			pct := *e.Info.Utilization * 100 // a fraction of the cap
			l.UsedPercent = &pct
		}
		p.emit(Event{Type: EventUsage, Limits: []Limit{l}})
	case "result":
		var e struct {
			Subtype        string            `json:"subtype"`
			IsError        bool              `json:"is_error"`
			Result         string            `json:"result"`
			APIErrorStatus *int              `json:"api_error_status"`
			Errors         []json.RawMessage `json:"errors"`
			Usage          *struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		p.result = true
		if e.Usage != nil {
			p.emit(Event{Type: EventUsage, Tokens: &Tokens{Input: e.Usage.Input, Output: e.Usage.Output}})
		}
		if !e.IsError {
			p.answer = e.Result
			return
		}
		// Grok Build says why only in errors[] ("Not signed in. …"), with no result text.
		msg := e.Result
		for _, raw := range e.Errors {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				msg = strings.TrimSpace(strings.TrimSpace(msg) + "\n" + s)
			}
		}
		if msg == "" {
			msg = e.Subtype
		}
		if (e.APIErrorStatus != nil && *e.APIErrorStatus == 401) || authFailure.MatchString(msg) {
			p.loginRequired(msg)
			return
		}
		if usageLimit.MatchString(msg) {
			p.limitReached(msg)
		}
		p.failed = msg
	case "error": // Grok's error line: {"type":"error","message":…}
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if authFailure.MatchString(e.Message) {
			p.loginRequired(e.Message)
			return
		}
		p.failed = e.Message
	}
}

func (p *parser) assistantBlock(raw json.RawMessage) {
	var bl struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &bl) != nil {
		return
	}
	switch bl.Type {
	case "text":
		if bl.Text != "" {
			p.emit(Event{Type: EventText, Text: cut(bl.Text, maxEventText)})
		}
	case "thinking":
		if bl.Thinking != "" {
			p.emit(Event{Type: EventThinking, Text: cut(bl.Thinking, maxEventText)})
		}
	case "tool_use", "server_tool_use":
		p.tools[bl.ID] = bl.Name
		p.inputs[bl.ID] = compactJSON(bl.Input)
		p.emit(Event{Type: EventTool, Tool: &Tool{
			ID: bl.ID, Name: bl.Name, Status: "in_progress", Input: cut(compactJSON(bl.Input), maxToolText),
		}})
	}
}

// codex reads `codex exec --json`.
func (p *parser) codex(typ string, b []byte) {
	switch typ {
	case "thread.started":
		var e struct {
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(b, &e) == nil && e.ThreadID != "" {
			p.emit(Event{Type: EventSession, Session: e.ThreadID})
		}
	case "item.started", "item.updated", "item.completed":
		var e struct {
			Item codexItem `json:"item"`
		}
		if json.Unmarshal(b, &e) == nil {
			p.codexItem(e.Item, typ == "item.completed")
		}
	case "turn.completed":
		var e struct {
			Usage *struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &e) == nil && e.Usage != nil {
			p.emit(Event{Type: EventUsage, Tokens: &Tokens{Input: e.Usage.Input, Output: e.Usage.Output}})
		}
	case "turn.failed":
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if authFailure.MatchString(e.Error.Message) {
			p.loginRequired(e.Error.Message)
			return
		}
		if codexLimit.MatchString(e.Error.Message) {
			p.limitReached(e.Error.Message)
		}
		p.failed = e.Error.Message
	case "error":
		// Retries ("Reconnecting... 1/5 (…)") and other trouble. A 401 is retried, uselessly.
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if authFailure.MatchString(e.Message) {
			p.loginRequired(e.Message)
			return
		}
		p.emit(Event{Type: EventNotice, Text: cut(e.Message, maxToolText)})
	}
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`
	Query            string `json:"query"`
	Server           string `json:"server"`
	Tool             string `json:"tool"`
	// Arguments are an MCP tool call's (taskto's own tools among them: docs/cli-tools.md).
	Arguments json.RawMessage `json:"arguments"`
	// Result is an MCP tool call's answer: {"content": [{"type": "text", "text": …}]}.
	Result  json.RawMessage `json:"result"`
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func (p *parser) codexItem(it codexItem, completed bool) {
	var saved *SavedApp
	tool := func(name, input, output string, failed bool) {
		status := "in_progress"
		if completed {
			status = "completed"
			if failed {
				status = "failed"
			}
		}
		p.emit(Event{Type: EventTool, Tool: &Tool{
			ID: it.ID, Name: name, Status: status, Input: cut(input, maxToolText), Output: cut(output, maxToolText),
			Saved: saved,
		}})
	}
	switch it.Type {
	case "agent_message":
		if completed && it.Text != "" {
			p.answer = it.Text
			p.emit(Event{Type: EventText, Text: cut(it.Text, maxEventText)})
		}
	case "reasoning":
		if completed && it.Text != "" {
			p.emit(Event{Type: EventThinking, Text: cut(it.Text, maxEventText)})
		}
	case "command_execution":
		tool("shell", it.Command, it.AggregatedOutput, it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0))
	case "file_change":
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Kind+" "+c.Path)
		}
		tool("edit", strings.Join(paths, "\n"), "", it.Status == "failed")
	case "mcp_tool_call":
		// No arguments (absent, or null) show as none.
		input := compactJSON(it.Arguments)
		if input == "null" {
			input = ""
		}
		name := it.Server + "." + it.Tool
		var answer struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(it.Result, &answer)
		saved = savedApp(name, input, contentText(answer.Content), completed && it.Status != "failed")
		tool(name, input, "", it.Status == "failed")
	case "web_search":
		tool("web_search", it.Query, "", false)
	case "error":
		// The warning that hook trust is bypassed is ours to know (drivers.go), not the user's.
		if completed && !strings.Contains(it.Message, "--dangerously-bypass-hook-trust") {
			p.emit(Event{Type: EventNotice, Text: cut(it.Message, maxToolText)})
		}
	}
}

// codexLimit matches Codex's message for a turn refused on a plan limit ("You've hit your
// usage limit … try again at …").
var codexLimit = regexp.MustCompile(`(?i)usage limit|you've hit your|rate limit reached`)

// contentTextOf joins the text blocks of an assistant message's content.
func contentTextOf(blocks []json.RawMessage) string {
	var out []string
	for _, raw := range blocks {
		var bl struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &bl) == nil && bl.Type == "text" {
			out = append(out, bl.Text)
		}
	}
	return strings.Join(out, "\n")
}

// contentText is the text of a tool result's content: a string, or text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out []string
	for _, bl := range blocks {
		if bl.Type == "text" {
			out = append(out, bl.Text)
		}
	}
	return strings.Join(out, "\n")
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // shown as text, never as HTML: keep "&&" and "<" readable
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func unixTime(sec *int64) *time.Time {
	if sec == nil || *sec <= 0 {
		return nil
	}
	t := time.Unix(*sec, 0).UTC()
	return &t
}

// cut shortens s to at most n bytes, on a rune boundary, marking that it was cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…"
	s = s[:n-len(mark)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + mark
}

// saveTools are taskto's tools that save an app, answering with it.
var saveTools = map[string]bool{"create_app": true, "update_app": true}

// savedApp is the app a finished call of taskto's create_app or update_app saved, if the call was
// one and it succeeded, from the whole of its answer (an app's JSON: its id and its space_id). The
// CLIs name taskto's tools mcp__taskto__<tool> (Claude Code), taskto.<tool> (Codex's server and
// tool) and taskto__<tool> (Grok Build, which may also call it through use_tool, with the name
// in the input).
func savedApp(name, input, answer string, ok bool) *SavedApp {
	if !ok {
		return nil
	}
	tool, found := strings.CutPrefix(name, "mcp__"+toolsServer+"__")
	if !found {
		tool, found = strings.CutPrefix(name, toolsServer+"__")
	}
	if !found {
		tool, found = strings.CutPrefix(name, toolsServer+".")
	}
	if name == "use_tool" {
		var call struct {
			ToolName string `json:"tool_name"`
		}
		_ = json.Unmarshal([]byte(input), &call)
		tool, found = strings.CutPrefix(call.ToolName, toolsServer+"__")
	}
	if !found || !saveTools[tool] {
		return nil
	}
	var app struct {
		ID      int64 `json:"id"`
		SpaceID int64 `json:"space_id"`
	}
	if json.Unmarshal([]byte(answer), &app) != nil || app.ID < 1 || app.SpaceID < 1 {
		return nil
	}
	return &SavedApp{Space: app.SpaceID, App: app.ID}
}
