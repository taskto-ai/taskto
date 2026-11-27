// Package cliworker runs the official coding-agent CLIs (Claude Code, Codex, Grok Build) for
// taskto users, one process per turn, and turns what each prints into one small event stream.
// The design is docs/cli-workers.md.
//
// A Worker keeps every running turn with a log of its events, so a caller that lost the
// stream (the agent sidecar restarted) re-attaches by turn ID and reads on, instead of
// starting the CLI again. Each turn's CLI runs in a process group of its own with a clean
// environment, in the user's own directories (Layout); whatever it leaves behind is stopped
// when the turn ends, and a new Worker first stops every CLI an earlier one left running.
//
// In local mode (this package on its own) every CLI runs as the server's OS user: separate
// directories keep users' sessions and settings apart, not their secrets. The container
// mode adds a Unix user per taskto user around the same turn code.
package cliworker

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// CLI names one of the supported CLIs.
type CLI string

// The supported CLIs.
const (
	Claude CLI = "claude"
	Codex  CLI = "codex"
	Grok   CLI = "grok"
)

// CLIs lists the supported CLIs in display order.
var CLIs = []CLI{Claude, Codex, Grok}

// Valid reports whether c is a supported CLI.
func (c CLI) Valid() bool {
	switch c {
	case Claude, Codex, Grok:
		return true
	}
	return false
}

// Name is the CLI's product name, for messages.
func (c CLI) Name() string {
	switch c {
	case Claude:
		return "Claude Code"
	case Codex:
		return "Codex"
	case Grok:
		return "Grok Build"
	}
	return string(c)
}

// Slot names one of a user's account directories for a CLI, each with a config directory of
// its own (a login, its sessions and settings): "" is the first, "2" to "9" the user's other
// logins, and "s<id>" the directory for a workspace's shared account, which holds no login.
type Slot string

var slotPattern = regexp.MustCompile(`^([2-9]|s[1-9][0-9]{0,11})?$`)

// Valid reports whether s names a slot.
func (s Slot) Valid() bool { return slotPattern.MatchString(string(s)) }

// Shared reports whether s is a shared account's directory, which has no login of its own.
func (s Slot) Shared() bool { return strings.HasPrefix(string(s), "s") }

// EventType is the kind of an Event.
type EventType string

// The events of a turn, the same for every CLI.
const (
	// EventSession carries the CLI's session ID in Session: first for Claude Code and Grok
	// (which take ours), and as soon as Codex names its thread.
	EventSession EventType = "session"
	// EventText is a piece of the answer the CLI shows the user, in Text.
	EventText EventType = "text"
	// EventThinking is the CLI's reasoning, in Text.
	EventThinking EventType = "thinking"
	// EventTool is a tool call starting or ending, in Tool.
	EventTool EventType = "tool"
	// EventUsage carries tokens and, when the CLI reports them, usage limits.
	EventUsage EventType = "usage"
	// EventNotice is something the CLI reported that doesn't stop the turn (a retry, a
	// warning), in Text.
	EventNotice EventType = "notice"
	// EventPermission is a permission question to the owner (Permission, status asked), and
	// later its answer (allowed or denied).
	EventPermission EventType = "permission"
	// EventLoginRequired means the CLI's login is missing, expired or revoked: the turn
	// stops, and needs a new login to go on.
	EventLoginRequired EventType = "login_required"
	// EventInterrupted is the server's, not a CLI's: the turn resumes a CLI session that a
	// restart interrupted, so some of its steps may have run twice.
	EventInterrupted EventType = "interrupted"
	// EventDone is always the last event: Status says how the turn ended, Text is the final
	// answer (or the error), Exit the CLI's exit code when it exited on its own.
	EventDone EventType = "done"
)

// Status is how a turn ended.
type Status string

// The ways a turn ends.
const (
	StatusOK            Status = "ok"
	StatusFailed        Status = "failed"
	StatusLoginRequired Status = "login_required"
	StatusStopped       Status = "stopped"
)

// Event is one thing that happened in a turn. Seq numbers a turn's events from 1.
type Event struct {
	Seq        int         `json:"seq"`
	Type       EventType   `json:"type"`
	Text       string      `json:"text,omitempty"`
	Session    string      `json:"session,omitempty"`
	Tool       *Tool       `json:"tool,omitempty"`
	Permission *Permission `json:"permission,omitempty"`
	Tokens     *Tokens     `json:"tokens,omitempty"`
	Limits     []Limit     `json:"limits,omitempty"`
	Status     Status      `json:"status,omitempty"`
	Exit       *int        `json:"exit,omitempty"`
}

// Tool is a tool call of the CLI, shortened for display: Input and Output are cut to a few
// KB, since the CLI's own session keeps them whole.
type Tool struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Input  string `json:"input,omitempty"`
	Status string `json:"status"` // running, done or failed
	Output string `json:"output,omitempty"`
	// Saved is the app a finished create_app or update_app of taskto's tools answered with, read
	// from the whole result (Output is cut), so the conversation can show the app the CLI saved.
	Saved *SavedApp `json:"saved,omitempty"`
}

// SavedApp names an app a CLI turn saved: its space and its id.
type SavedApp struct {
	Space int64 `json:"space"`
	App   int64 `json:"app"`
}

// Tokens is a turn's token use, as the CLI reported it.
type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// Limit is one usage limit of the account a CLI is signed in with.
type Limit struct {
	// Window names it: five_hour, seven_day, … for Claude Code; primary and secondary for
	// Codex, with WindowMinutes saying how long they are.
	Window        string     `json:"window"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	// Status is Claude Code's: allowed, allowed_warning or rejected.
	Status string `json:"status,omitempty"`
	// Detail is the CLI's own words, for a limit known only from its message ("Usage limit
	// reached · resets 3pm").
	Detail string `json:"detail,omitempty"`
}

// TurnSpec says what a turn runs.
type TurnSpec struct {
	// ID names the turn for re-attaching: starting a turn whose ID is running or recently
	// finished returns that turn instead of starting another. 1–64 of [A-Za-z0-9_-].
	ID   string `json:"id"`
	User int64  `json:"user"`
	// Identity and Boot name the account the turn is for, with User (cliowner.Account): the worker
	// runs it in that account's own home.
	Identity string `json:"identity"`
	Boot     string `json:"boot,omitempty"`
	// Installation is the server's installation (cliowner.Account.Installation).
	Installation string `json:"installation,omitempty"`
	CLI          CLI    `json:"cli"`
	// Slot is the account directory the turn runs in: the CLI's login and sessions there.
	Slot Slot `json:"slot,omitempty"`
	// Proxy points the CLI at the server's proxy instead of a login, for a workspace's shared
	// account (Slot "s<id>"): the server adds the account's key to its requests, and the CLI
	// only ever holds the turn's token.
	Proxy *Proxy `json:"proxy,omitempty"`
	// Tools, if set, gives the CLI taskto's own tools (docs/cli-tools.md): the server's MCP
	// endpoint for turns, with this turn's token.
	Tools *Tools `json:"tools,omitempty"`
	// Model is passed to the CLI's model flag; empty leaves the CLI's default.
	Model string `json:"model,omitempty"`
	// Conversation fixes the working directory: Claude Code finds a session by it.
	Conversation int64 `json:"conversation"`
	// Session is the CLI session to start (Resume false) or resume (Resume true). Claude
	// Code and Grok need one to start (a UUID the caller recorded first); Codex starts
	// without one and reports its thread in an EventSession.
	Session string `json:"session,omitempty"`
	Resume  bool   `json:"resume,omitempty"`
	Prompt  string `json:"prompt"`
	// Permissions says whether the CLI asks before it acts: Ask (the default) or AllowAll.
	Permissions Permissions `json:"permissions,omitempty"`
	// FreshPrompt is the prompt to use instead if the session to resume turns out not to
	// exist and a new one starts in its place (with the conversation so far, say); empty
	// means Prompt.
	FreshPrompt string `json:"fresh_prompt,omitempty"`
}

// Proxy is where a turn on a shared account sends its model requests, and with what.
type Proxy struct {
	// URL is the server's proxy as the CLI reaches it, without a path ("http://taskto:8081").
	URL string `json:"url"`
	// Token is the turn's own credential for the proxy, never the account's key.
	Token string `json:"token"`
	// Model, if set, is the model the CLI asks for, whatever the conversation chose: a gateway
	// may name its models differently.
	Model string `json:"model,omitempty"`
}

var proxyTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{16,}$`)

// Tools is where a turn's CLI reaches taskto's own tools (the knowledge base, boards, apps and
// the agent browser), and with what (docs/cli-tools.md).
type Tools struct {
	// URL is the server's MCP endpoint for turns, as the CLI reaches it
	// ("http://127.0.0.1:8081/cli-mcp").
	URL string `json:"url"`
	// Token is the turn's own credential for the endpoint: it acts as the conversation's owner,
	// in that conversation, until the turn ends.
	Token string `json:"token"`
	// ReadOnly names the tools that change nothing: the permission hook lets them through
	// without asking the owner.
	ReadOnly []string `json:"read_only,omitempty"`
}

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (t *Tools) check() error {
	if t == nil {
		return nil
	}
	u, err := url.Parse(t.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("invalid tools URL")
	case len(t.Token) > 1024 || !proxyTokenPattern.MatchString(t.Token):
		return fmt.Errorf("invalid tools token")
	case len(t.ReadOnly) > 200:
		return fmt.Errorf("too many read-only tools")
	}
	for _, name := range t.ReadOnly {
		if !toolNamePattern.MatchString(name) {
			return fmt.Errorf("invalid tool name")
		}
	}
	return nil
}

func (p *Proxy) check(slot Slot) error {
	if p == nil {
		if slot.Shared() {
			return fmt.Errorf("a shared account's turn needs the proxy")
		}
		return nil
	}
	u, err := url.Parse(p.URL)
	switch {
	case !slot.Shared():
		return fmt.Errorf("only a shared account's turn goes through the proxy")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("invalid proxy URL")
	case len(p.Token) > 1024 || !proxyTokenPattern.MatchString(p.Token):
		return fmt.Errorf("invalid proxy token")
	case p.Model != "" && !modelPattern.MatchString(p.Model):
		return fmt.Errorf("invalid proxy model")
	}
	return nil
}

// base is the proxy's URL without a trailing slash.
func (p *Proxy) base() string { return strings.TrimRight(p.URL, "/") }

var (
	turnIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9._:/\[\]-]{1,128}$`)
)

// errUnsupported is New's error on a system without process groups.
var errUnsupported = errors.New("CLI workers need a Unix system")

// maxPrompt bounds a prompt: it is passed in argv, which the kernel limits too.
const maxPrompt = 96 << 10

// Account is the account the turn is for.
func (s TurnSpec) Account() cliowner.Account {
	return cliowner.Account{User: s.User, Identity: s.Identity, Boot: s.Boot, Installation: s.Installation}
}

func (s TurnSpec) check() error {
	toolsErr := s.Tools.check()
	switch {
	case !turnIDPattern.MatchString(s.ID):
		return fmt.Errorf("turn ID must be 1-64 of A-Z a-z 0-9 _ -")
	case !s.Account().Valid():
		return fmt.Errorf("a turn needs an account")
	case !s.CLI.Valid():
		return fmt.Errorf("unknown CLI %q", s.CLI)
	case !s.Slot.Valid():
		return fmt.Errorf("invalid account slot")
	case s.Proxy.check(s.Slot) != nil:
		return s.Proxy.check(s.Slot)
	case toolsErr != nil:
		return fmt.Errorf("taskto's tools: %w", toolsErr)
	case s.Conversation <= 0:
		return fmt.Errorf("a turn needs a conversation")
	case s.Model != "" && !modelPattern.MatchString(s.Model):
		return fmt.Errorf("invalid model name")
	case s.Session != "" && !sessionPattern.MatchString(s.Session):
		return fmt.Errorf("invalid session ID")
	case s.Resume && s.Session == "":
		return fmt.Errorf("resuming needs a session ID")
	case s.CLI != Codex && s.Session == "":
		return fmt.Errorf("%s needs a session ID to start", s.CLI.Name())
	case s.CLI == Codex && !s.Resume && s.Session != "":
		return fmt.Errorf("codex names a new session itself: start it without one")
	case !s.Permissions.Valid():
		return fmt.Errorf("permissions must be ask or allow_all")
	case s.Prompt == "" || len(s.Prompt) > maxPrompt || len(s.FreshPrompt) > maxPrompt:
		return fmt.Errorf("the prompt must be 1 byte to 96 KiB")
	}
	return nil
}
