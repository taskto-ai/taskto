package cliworker

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type CLI string

const (
	Claude CLI = "claude"
	Codex  CLI = "codex"
	Grok   CLI = "grok"
)

var CLIs = []CLI{Claude, Codex, Grok}

func (c CLI) Valid() bool {
	switch c {
	case Claude, Codex, Grok:
		return true
	}
	return false
}

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

type Slot string

var slotPattern = regexp.MustCompile(`^([2-9]|s[1-9][0-9]{0,11})?$`)

func (s Slot) Valid() bool { return slotPattern.MatchString(string(s)) }

func (s Slot) Shared() bool { return strings.HasPrefix(string(s), "s") }

type EventType string

const (
	EventSession       EventType = "session"
	EventText          EventType = "text"
	EventThinking      EventType = "thinking"
	EventTool          EventType = "tool"
	EventUsage         EventType = "usage"
	EventNotice        EventType = "notice"
	EventPermission    EventType = "permission"
	EventLoginRequired EventType = "login_required"
	EventInterrupted   EventType = "interrupted"
	EventDone          EventType = "done"
)

type Status string

const (
	StatusOK            Status = "ok"
	StatusFailed        Status = "failed"
	StatusLoginRequired Status = "login_required"
	StatusStopped       Status = "stopped"
)

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

type Tool struct {
	ID     string    `json:"id,omitempty"`
	Name   string    `json:"name"`
	Input  string    `json:"input,omitempty"`
	Status string    `json:"status"`
	Output string    `json:"output,omitempty"`
	Saved  *SavedApp `json:"saved,omitempty"`
}

type SavedApp struct {
	Space int64 `json:"space"`
	App   int64 `json:"app"`
}

type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

type Limit struct {
	Window        string     `json:"window"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Status        string     `json:"status,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

type TurnSpec struct {
	ID           string      `json:"id"`
	User         int64       `json:"user"`
	CLI          CLI         `json:"cli"`
	Slot         Slot        `json:"slot,omitempty"`
	Proxy        *Proxy      `json:"proxy,omitempty"`
	Tools        *Tools      `json:"tools,omitempty"`
	Model        string      `json:"model,omitempty"`
	Conversation int64       `json:"conversation"`
	Session      string      `json:"session,omitempty"`
	Resume       bool        `json:"resume,omitempty"`
	Prompt       string      `json:"prompt"`
	Permissions  Permissions `json:"permissions,omitempty"`
	FreshPrompt  string      `json:"fresh_prompt,omitempty"`
}

type Proxy struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Model string `json:"model,omitempty"`
}

var proxyTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{16,}$`)

type Tools struct {
	URL      string   `json:"url"`
	Token    string   `json:"token"`
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

func (p *Proxy) base() string { return strings.TrimRight(p.URL, "/") }

var (
	turnIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9._:/\[\]-]{1,128}$`)
)

var errUnsupported = errors.New("CLI workers need a Unix system")

const maxPrompt = 96 << 10

func (s TurnSpec) check() error {
	toolsErr := s.Tools.check()
	switch {
	case !turnIDPattern.MatchString(s.ID):
		return fmt.Errorf("turn ID must be 1-64 of A-Z a-z 0-9 _ -")
	case s.User <= 0:
		return fmt.Errorf("a turn needs a user")
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
