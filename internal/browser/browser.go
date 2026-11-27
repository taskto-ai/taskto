package browser

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

type Kind string

const (
	Personal Kind = "personal"
	Testing  Kind = "testing"
)

func (k Kind) Valid() bool { return k == Personal || k == Testing }

type Tab struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Agent bool   `json:"agent"`
}

type Op string

const (
	OpTabs     Op = "tabs"
	OpOpen     Op = "open"
	OpNavigate Op = "navigate"
	OpSnapshot Op = "snapshot"
	OpClick    Op = "click"
	OpType     Op = "type"
	OpPress    Op = "press"
	OpClose    Op = "close"
	OpFront    Op = "front"
)

type Action struct {
	Op      Op     `json:"op"`
	Browser Kind   `json:"browser"`
	Tab     string `json:"tab,omitempty"`
	URL     string `json:"url,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Text    string `json:"text,omitempty"`
	Submit  bool   `json:"submit,omitempty"`
	Key     string `json:"key,omitempty"`
}

type Result struct {
	Tabs     []Tab  `json:"tabs,omitempty"`
	Tab      *Tab   `json:"tab,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

var (
	ErrUnavailable    = errors.New("this browser is not installed on the server")
	ErrBusy           = errors.New("too many browsers are open on this server; try again in a few minutes")
	ErrNotRunning     = errors.New("the browser is not running")
	ErrNoTab          = errors.New("no such tab: list the tabs with browser_tabs")
	ErrNotAgentTab    = errors.New("this tab is the user's own: only tabs the agent opened can be closed")
	ErrStaleRef       = errors.New("no such element on the page now: take a new snapshot")
	ErrTooManyLive    = errors.New("too many live views of this browser are open: close one first")
	ErrClosed         = errors.New("the browser service is shutting down")
	ErrAccountClosing = errors.New("this account is being deleted: its browsers can't be started")
)

type BadAction struct{ msg string }

func (e *BadAction) Error() string { return e.msg }

func badf(format string, args ...any) error { return &BadAction{fmt.Sprintf(format, args...)} }

var (
	tabPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	refPattern = regexp.MustCompile(`^e[0-9]{1,6}$`)
)

const maxText = 16 << 10

func (a Action) Check() error {
	if !a.Browser.Valid() {
		return badf("browser must be %q or %q", Personal, Testing)
	}
	needTab := func() error {
		if !tabPattern.MatchString(a.Tab) {
			return badf("a tab id from browser_tabs or browser_open is needed")
		}
		return nil
	}
	needRef := func() error {
		if !refPattern.MatchString(a.Ref) {
			return badf("ref must be an element's ref from browser_snapshot, such as e12")
		}
		return nil
	}
	switch a.Op {
	case OpTabs:
		return nil
	case OpOpen:
		return checkURL(a.URL)
	case OpNavigate:
		if err := needTab(); err != nil {
			return err
		}
		return checkURL(a.URL)
	case OpSnapshot, OpClose, OpFront:
		return needTab()
	case OpClick:
		if err := needTab(); err != nil {
			return err
		}
		return needRef()
	case OpType:
		if err := needTab(); err != nil {
			return err
		}
		if err := needRef(); err != nil {
			return err
		}
		if len(a.Text) > maxText {
			return badf("text is over %d KiB", maxText>>10)
		}
		return nil
	case OpPress:
		if err := needTab(); err != nil {
			return err
		}
		if _, ok := keys[a.Key]; !ok {
			return badf("key must be one of %s", keyNames())
		}
		return nil
	}
	return badf("unknown action %q", a.Op)
}

func checkURL(raw string) error {
	if raw == "about:blank" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8<<10 || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return badf("url must be an http or https address")
	}
	return nil
}
