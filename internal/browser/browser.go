// Package browser runs a Chromium per taskto user for the agent, and lets the user watch it and
// take over (docs/agent-browser.md).
//
// Each user has up to two browsers:
//
//   - the personal one, headed on a virtual display (Xvfb), with a profile kept between runs
//     (cookies, logins), and a live view over VNC (x11vnc on a Unix socket in the user's own
//     directory);
//   - the testing one, headless, with a new temporary profile each time it starts, for trying
//     apps with nothing of the user's in it.
//
// Go drives each one over the Chrome DevTools Protocol on --remote-debugging-pipe: a pipe
// only this process holds, so Chromium listens on no port another user could reach. The
// processes are started by a Host: as the server's own OS user in local mode (LocalHost), or
// as the user's own Unix user through the CLI worker's helper in container mode. Either way
// the Manager owns them: it starts a browser on the first request, stops it after a few idle
// minutes, and keeps at most a few running at once.
package browser

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// Kind is one of a user's two browsers.
type Kind string

// The browsers.
const (
	// Personal is headed, with a profile kept between runs and a live view.
	Personal Kind = "personal"
	// Testing is headless, with a new, empty profile each time it starts.
	Testing Kind = "testing"
)

// Valid reports whether k names a browser.
func (k Kind) Valid() bool { return k == Personal || k == Testing }

// Tab is an open tab.
type Tab struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Agent says the agent opened it: only those may be closed by the agent.
	Agent bool `json:"agent"`
}

// Op is what an Action does.
type Op string

// The actions on a browser. Each starts the browser if it isn't running, except Tabs.
const (
	OpTabs     Op = "tabs"
	OpOpen     Op = "open"
	OpNavigate Op = "navigate"
	OpSnapshot Op = "snapshot"
	OpClick    Op = "click"
	OpType     Op = "type"
	OpPress    Op = "press"
	OpClose    Op = "close"
	// OpFront brings a tab to the front of the personal browser, for the user to take over.
	OpFront Op = "front"
)

// Action is one thing the agent does in a browser.
type Action struct {
	Op      Op     `json:"op"`
	Browser Kind   `json:"browser"`
	Tab     string `json:"tab,omitempty"`
	URL     string `json:"url,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Text    string `json:"text,omitempty"`
	// Submit presses Enter after Text is typed.
	Submit bool   `json:"submit,omitempty"`
	Key    string `json:"key,omitempty"`
}

// Result is what an Action returns: Tabs for OpTabs, the Tab acted on for the others, and the
// page as text for OpSnapshot.
type Result struct {
	Tabs     []Tab  `json:"tabs,omitempty"`
	Tab      *Tab   `json:"tab,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Errors of the Manager. Their messages are for the agent and the user.
var (
	ErrUnavailable = errors.New("this browser is not installed on the server")
	ErrBusy        = errors.New("too many browsers are open on this server; try again in a few minutes")
	ErrNotRunning  = errors.New("the browser is not running")
	ErrNoTab       = errors.New("no such tab: list the tabs with browser_tabs")
	ErrNotAgentTab = errors.New("this tab is the user's own: only tabs the agent opened can be closed")
	ErrStaleRef    = errors.New("no such element on the page now: take a new snapshot")
	ErrTooManyLive = errors.New("too many live views of this browser are open: close one first")
	ErrClosed      = errors.New("the browser service is shutting down")
	// ErrAccountClosing is a start refused while the browsers of the user's deleted account are
	// being stopped and their files removed (Manager.Forget).
	ErrAccountClosing = errors.New("this account is being deleted: its browsers can't be started")
)

// BadAction is a mistake in an Action's arguments; its message says which.
type BadAction struct{ msg string }

func (e *BadAction) Error() string { return e.msg }

func badf(format string, args ...any) error { return &BadAction{fmt.Sprintf(format, args...)} }

var (
	tabPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	refPattern = regexp.MustCompile(`^e[0-9]{1,6}$`)
)

// maxText bounds what is typed in one action.
const maxText = 16 << 10

// Check checks a's arguments for its op: the browser, a tab's and a ref's form, the URL's
// scheme (http, https or about:blank), the key. The Manager checks every action; the server
// checks them too, before they reach the worker.
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

// checkURL accepts the pages the agent may open: http and https, and about:blank. Not file:,
// chrome: or javascript:, which would reach the user's files or the browser's own pages.
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
