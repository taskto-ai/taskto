package agentbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/browser"
)

// The browser tools (internal/agenttools/tools.json), for the conversation's owner. Every
// call acts on that user's own browsers: the user comes from the conversation's capability,
// never from the arguments.

// toolArgs are every browser tool's arguments; each tool takes the ones its schema names.
type toolArgs struct {
	Browser browser.Kind `json:"browser"`
	Tab     string       `json:"tab"`
	URL     string       `json:"url"`
	Ref     string       `json:"ref"`
	Text    *string      `json:"text"`
	Submit  bool         `json:"submit"`
	Key     string       `json:"key"`
	Reason  string       `json:"reason"`
	Message string       `json:"message"`
}

// allowed is the arguments each tool takes, beyond "browser".
var allowed = map[string][]string{
	"browser_tabs":     {},
	"browser_open":     {"url"},
	"browser_navigate": {"tab", "url"},
	"browser_snapshot": {"tab"},
	"browser_click":    {"tab", "ref"},
	"browser_type":     {"tab", "ref", "text", "submit"},
	"browser_press":    {"tab", "key"},
	"browser_close":    {"tab"},
	"browser_help":     {"tab", "reason", "message"},
}

var ops = map[string]browser.Op{
	"browser_tabs":     browser.OpTabs,
	"browser_open":     browser.OpOpen,
	"browser_navigate": browser.OpNavigate,
	"browser_snapshot": browser.OpSnapshot,
	"browser_click":    browser.OpClick,
	"browser_type":     browser.OpType,
	"browser_press":    browser.OpPress,
	"browser_close":    browser.OpClose,
	"browser_help":     browser.OpFront,
}

// helpReasons are browser_help's reasons.
var helpReasons = map[string]bool{"login": true, "captcha": true, "two_factor": true, "other": true}

func refuse(status int, format string, args ...any) error {
	return &agenttools.Refusal{Status: status, Message: fmt.Sprintf(format, args...)}
}

// signInNote goes with a snapshot of the personal browser that has a password field. Real
// models, told only by browser_help's description, sometimes submitted the empty form or asked
// for the password in the chat instead: said here, at the moment it matters, they don't.
const signInNote = "This page asks for a password. Don't ask the user for it in the chat, and don't " +
	"submit the form: if you need to be signed in, call browser_help (reason login) so they sign in " +
	"themselves in the live view."

// decodeArgs reads name's arguments strictly: an argument the tool doesn't take is refused,
// so a misspelt one doesn't silently do something else.
func decodeArgs(name string, raw json.RawMessage) (toolArgs, error) {
	var fields map[string]json.RawMessage
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return toolArgs{}, refuse(http.StatusBadRequest, "invalid arguments: one JSON object expected")
	}
	ok := map[string]bool{"browser": name != "browser_help"}
	for _, f := range allowed[name] {
		ok[f] = true
	}
	for f := range fields {
		if !ok[f] {
			return toolArgs{}, refuse(http.StatusBadRequest, "invalid arguments: %s doesn't take %q", name, f)
		}
	}
	var a toolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolArgs{}, refuse(http.StatusBadRequest, "invalid arguments: %v", err)
	}
	if a.Browser == "" {
		a.Browser = browser.Personal
	}
	return a, nil
}

// Call implements agenttools.BrowserTools: runs tool name for user, in conversation.
func (s *Service) Call(ctx context.Context, user, conversation int64, name string, raw json.RawMessage) (any, error) {
	op, known := ops[name]
	if !known {
		return nil, refuse(http.StatusNotFound, "unknown tool")
	}
	if err := s.Usable(ctx); err != nil {
		return nil, refuse(http.StatusConflict, "%s", err.Error())
	}
	a, err := decodeArgs(name, raw)
	if err != nil {
		return nil, err
	}
	action := browser.Action{Op: op, Browser: a.Browser, Tab: a.Tab, URL: a.URL, Ref: a.Ref, Submit: a.Submit, Key: a.Key}
	if a.Text != nil {
		action.Text = *a.Text
	} else if name == "browser_type" {
		return nil, refuse(http.StatusBadRequest, "text is needed")
	}
	if name == "browser_help" {
		if !helpReasons[a.Reason] {
			return nil, refuse(http.StatusBadRequest, "reason must be login, captcha, two_factor or other")
		}
		if a.Message = strings.TrimSpace(a.Message); a.Message == "" || utf8.RuneCountInString(a.Message) > 500 {
			return nil, refuse(http.StatusBadRequest, "message must say in up to 500 characters what the user should do")
		}
	}
	// Checked here too, before anything reaches the worker, which checks it again: the URL's
	// scheme (no file:, chrome: or javascript:), the browser, a tab's and a ref's form, the key.
	if err := action.Check(); err != nil {
		return nil, toolError(ctx, name, err)
	}
	res, err := s.b.Do(ctx, user, action)
	if err != nil {
		return nil, toolError(ctx, name, err)
	}
	slog.DebugContext(ctx, "browser tool", "tool", name, "user", user, "conversation", conversation)
	switch name {
	case "browser_tabs":
		return map[string]any{"browser": a.Browser, "tabs": res.Tabs}, nil
	case "browser_snapshot":
		out := map[string]any{"tab": a.Tab, "snapshot": res.Snapshot}
		if a.Browser == browser.Personal && browser.HasPasswordField(res.Snapshot) {
			out["note"] = signInNote
		}
		return out, nil
	case "browser_close":
		return map[string]any{"closed": a.Tab}, nil
	case "browser_help":
		slog.InfoContext(ctx, "the agent asked the user to take over the browser", "user", user, "conversation", conversation, "reason", a.Reason)
		return map[string]any{
			"status": "waiting_for_user",
			"tab":    res.Tab,
			"note": "The user now sees your message and a live view of their browser. End your turn now, " +
				"saying in one short sentence what they need to do. They will reply when they are done, " +
				"or that they won't do it now; then take a new snapshot before you go on.",
		}, nil
	}
	return map[string]any{"tab": res.Tab}, nil
}

// toolError turns a browser's error into a refusal for the model, in its own words.
func toolError(ctx context.Context, name string, err error) error {
	var bad *browser.BadAction
	switch {
	case errors.As(err, &bad):
		return refuse(http.StatusBadRequest, "%s", err.Error())
	case errors.Is(err, browser.ErrNoTab):
		return refuse(http.StatusNotFound, "%s", err.Error())
	case errors.Is(err, browser.ErrStaleRef), errors.Is(err, browser.ErrNotAgentTab):
		return refuse(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, browser.ErrUnavailable), errors.Is(err, browser.ErrBusy), errors.Is(err, browser.ErrClosed), errors.Is(err, browser.ErrNotRunning), errors.Is(err, browser.ErrAccountClosing):
		return refuse(http.StatusServiceUnavailable, "%s", err.Error())
	case ctx.Err() != nil:
		return ctx.Err()
	}
	slog.WarnContext(ctx, "browser tool failed", "tool", name, "err", err)
	// Only what is about the page reaches the model (and so the conversation): a page that
	// didn't load (net::ERR_…), an error the page's own script threw, a browser that stopped.
	// Anything else can name the server's insides (paths, sockets), and stays in the log.
	msg := firstLine(err.Error())
	for _, prefix := range pageErrors {
		if strings.HasPrefix(msg, prefix) {
			return refuse(http.StatusBadGateway, "%s", msg)
		}
	}
	return refuse(http.StatusBadGateway, "the browser failed: try again, or ask the user to look at it")
}

// pageErrors start the browser's errors that are about the page, which the model may see.
var pageErrors = []string{"the page didn't load: ", "in the page: ", "the browser stopped"}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 500 {
		s = s[:500]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
