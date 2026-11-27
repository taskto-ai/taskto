package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed snapshot.js
var snapshotJS string

var passwordField = regexp.MustCompile(`\[e\d+\] input password `)

func HasPasswordField(snapshot string) bool { return passwordField.MatchString(snapshot) }

const maxSnapshot = 40 << 10

const (
	loadWait   = 15 * time.Second
	settleWait = 300 * time.Millisecond
)

type instance struct {
	kind Kind
	cdp  *cdp

	mu       sync.Mutex
	sessions map[string]string
	worlds   map[string]int64
	agent    map[string]bool
}

func newInstance(kind Kind, c *cdp) *instance {
	return &instance{kind: kind, cdp: c, sessions: map[string]string{}, worlds: map[string]int64{}, agent: map[string]bool{}}
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

func (b *instance) tab(t targetInfo) Tab {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Tab{ID: t.TargetID, Title: t.Title, URL: t.URL, Agent: b.agent[t.TargetID]}
}

func (b *instance) do(ctx context.Context, a Action) (Result, error) {
	switch a.Op {
	case OpTabs:
		tabs, err := b.tabs(ctx)
		return Result{Tabs: tabs}, err
	case OpOpen:
		return b.open(ctx, a.URL)
	case OpClose:
		return Result{}, b.close(ctx, a.Tab)
	}
	session, err := b.attach(ctx, a.Tab)
	if err != nil {
		return Result{}, err
	}
	switch a.Op {
	case OpNavigate:
		err = b.navigate(ctx, a.Tab, session, a.URL)
	case OpSnapshot:
		var text string
		text, err = b.snapshot(ctx, a.Tab, session)
		if err == nil {
			return Result{Snapshot: text}, nil
		}
	case OpClick:
		err = b.click(ctx, a.Tab, session, a.Ref)
	case OpType:
		err = b.typeText(ctx, a.Tab, session, a.Ref, a.Text, a.Submit)
	case OpPress:
		err = b.front(ctx, a.Tab, session)
		if err == nil {
			err = b.press(ctx, session, a.Key)
			b.settle(ctx, session)
		}
	case OpFront:
		err = b.front(ctx, a.Tab, session)
	}
	if err != nil {
		return Result{}, err
	}
	tab, err := b.info(ctx, a.Tab)
	return Result{Tab: &tab}, err
}

func (b *instance) tabs(ctx context.Context) ([]Tab, error) {
	var out struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := b.cdp.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
		return nil, err
	}
	tabs := []Tab{}
	open := map[string]bool{}
	for _, t := range out.TargetInfos {
		if t.Type == "page" {
			tabs = append(tabs, b.tab(t))
			open[t.TargetID] = true
		}
	}
	b.prune(open)
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].ID < tabs[j].ID })
	return tabs, nil
}

func (b *instance) info(ctx context.Context, tab string) (Tab, error) {
	var out struct {
		TargetInfo targetInfo `json:"targetInfo"`
	}
	if err := b.cdp.Call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": tab}, &out); err != nil {
		return Tab{}, noTab(err)
	}
	return b.tab(out.TargetInfo), nil
}

func noTab(err error) error {
	var ce *cdpError
	if errors.As(err, &ce) && (strings.HasPrefix(ce.Message, "No target with given id") || strings.HasPrefix(ce.Message, "No session with given id")) {
		return ErrNoTab
	}
	return err
}

func goneContext(err error) bool {
	var ce *cdpError
	return errors.As(err, &ce) && (strings.HasPrefix(ce.Message, "Cannot find context with specified id") ||
		strings.HasPrefix(ce.Message, "Execution context was destroyed"))
}

func (b *instance) open(ctx context.Context, url string) (Result, error) {
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := b.cdp.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &out); err != nil {
		return Result{}, err
	}
	b.mu.Lock()
	b.agent[out.TargetID] = true
	b.mu.Unlock()
	session, err := b.attach(ctx, out.TargetID)
	if err != nil {
		return Result{}, err
	}
	if url != "about:blank" {
		if err := b.navigate(ctx, out.TargetID, session, url); err != nil {
			tab, _ := b.info(ctx, out.TargetID)
			return Result{Tab: &tab}, err
		}
	}
	tab, err := b.info(ctx, out.TargetID)
	return Result{Tab: &tab}, err
}

func (b *instance) attach(ctx context.Context, tab string) (string, error) {
	b.mu.Lock()
	s, ok := b.sessions[tab]
	b.mu.Unlock()
	if ok {
		return s, nil
	}
	var info struct {
		TargetInfo targetInfo `json:"targetInfo"`
	}
	if err := b.cdp.Call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": tab}, &info); err != nil {
		return "", noTab(err)
	}
	if info.TargetInfo.Type != "page" {
		return "", ErrNoTab
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.cdp.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tab, "flatten": true}, &out); err != nil {
		return "", noTab(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[tab]; ok {
		return s, nil
	}
	b.sessions[tab] = out.SessionID
	return out.SessionID, nil
}

func (b *instance) prune(open map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.sessions {
		if !open[id] {
			delete(b.sessions, id)
		}
	}
	for id := range b.worlds {
		if !open[id] {
			delete(b.worlds, id)
		}
	}
	for id := range b.agent {
		if !open[id] {
			delete(b.agent, id)
		}
	}
}

func (b *instance) forget(tab string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, tab)
	delete(b.worlds, tab)
	delete(b.agent, tab)
}

func (b *instance) navigate(ctx context.Context, tab, session, url string) error {
	var out struct {
		ErrorText string `json:"errorText"`
	}
	if err := b.cdp.Call(ctx, session, "Page.navigate", map[string]any{"url": url}, &out); err != nil {
		return noTab(err)
	}
	b.dropWorld(tab)
	if out.ErrorText != "" {
		return fmt.Errorf("the page didn't load: %s", out.ErrorText)
	}
	b.waitLoad(ctx, session, loadWait)
	return nil
}

func (b *instance) dropWorld(tab string) {
	b.mu.Lock()
	delete(b.worlds, tab)
	b.mu.Unlock()
}

type evalResult struct {
	Result struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

func (b *instance) eval(ctx context.Context, session string, context int64, expr string) (json.RawMessage, error) {
	params := map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": false}
	if context != 0 {
		params["contextId"] = context
	}
	var out evalResult
	if err := b.cdp.Call(ctx, session, "Runtime.evaluate", params, &out); err != nil {
		return nil, err
	}
	if d := out.ExceptionDetails; d != nil {
		msg := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			msg = d.Exception.Description
		}
		return nil, fmt.Errorf("in the page: %s", msg)
	}
	return out.Result.Value, nil
}

func (b *instance) waitLoad(ctx context.Context, session string, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		v, err := b.eval(ctx, session, 0, "document.readyState")
		if err == nil && string(v) == `"complete"` {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (b *instance) settle(ctx context.Context, session string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(settleWait):
	}
	b.waitLoad(ctx, session, loadWait)
}

func (b *instance) snapshot(ctx context.Context, tab, session string) (string, error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := b.cdp.Call(ctx, session, "Page.getFrameTree", nil, &tree); err != nil {
		return "", noTab(err)
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := b.cdp.Call(ctx, session, "Page.createIsolatedWorld", map[string]any{
		"frameId": tree.FrameTree.Frame.ID, "worldName": "taskto",
	}, &world); err != nil {
		return "", err
	}
	v, err := b.eval(ctx, session, world.ExecutionContextID, snapshotJS)
	if err != nil {
		return "", err
	}
	var text string
	if err := json.Unmarshal(v, &text); err != nil {
		return "", fmt.Errorf("the snapshot: %w", err)
	}
	b.mu.Lock()
	b.worlds[tab] = world.ExecutionContextID
	b.mu.Unlock()
	return cutText(text, maxSnapshot), nil
}

func (b *instance) inWorld(ctx context.Context, tab, session, expr string) (json.RawMessage, error) {
	b.mu.Lock()
	world, ok := b.worlds[tab]
	b.mu.Unlock()
	if !ok {
		return nil, ErrStaleRef
	}
	v, err := b.eval(ctx, session, world, expr)
	if goneContext(err) {
		b.dropWorld(tab)
		return nil, ErrStaleRef
	}
	return v, err
}

func (b *instance) front(ctx context.Context, tab, session string) error {
	if b.kind != Personal {
		return nil
	}
	if err := b.cdp.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": tab}, nil); err != nil {
		return noTab(err)
	}
	return b.cdp.Call(ctx, session, "Page.bringToFront", nil, nil)
}

func (b *instance) click(ctx context.Context, tab, session, ref string) error {
	if err := b.front(ctx, tab, session); err != nil {
		return err
	}
	v, err := b.inWorld(ctx, tab, session, "__taskto.rect("+quote(ref)+")")
	if err != nil {
		return err
	}
	var at *struct{ X, Y float64 }
	if err := json.Unmarshal(v, &at); err != nil || at == nil {
		return ErrStaleRef
	}
	for _, ev := range []map[string]any{
		{"type": "mouseMoved", "x": at.X, "y": at.Y},
		{"type": "mousePressed", "x": at.X, "y": at.Y, "button": "left", "buttons": 1, "clickCount": 1},
		{"type": "mouseReleased", "x": at.X, "y": at.Y, "button": "left", "buttons": 0, "clickCount": 1},
	} {
		if err := b.cdp.Call(ctx, session, "Input.dispatchMouseEvent", ev, nil); err != nil {
			return err
		}
	}
	b.dropWorldIfNavigated(ctx, tab, session)
	return nil
}

func (b *instance) dropWorldIfNavigated(ctx context.Context, tab, session string) {
	b.settle(ctx, session)
	if _, err := b.inWorld(ctx, tab, session, "1"); errors.Is(err, ErrStaleRef) {
		b.dropWorld(tab)
	}
}

func (b *instance) typeText(ctx context.Context, tab, session, ref, text string, submit bool) error {
	if err := b.front(ctx, tab, session); err != nil {
		return err
	}
	v, err := b.inWorld(ctx, tab, session, "__taskto.focus("+quote(ref)+")")
	if err != nil {
		return err
	}
	if string(v) != "true" {
		return ErrStaleRef
	}
	if text != "" {
		if err := b.cdp.Call(ctx, session, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
			return err
		}
	}
	if submit {
		if err := b.press(ctx, session, "Enter"); err != nil {
			return err
		}
	}
	b.dropWorldIfNavigated(ctx, tab, session)
	return nil
}

type keyStroke struct {
	code string
	vk   int
	text string
}

var keys = map[string]keyStroke{
	"Enter":      {"Enter", 13, "\r"},
	"Tab":        {"Tab", 9, ""},
	"Escape":     {"Escape", 27, ""},
	"Backspace":  {"Backspace", 8, ""},
	"Delete":     {"Delete", 46, ""},
	"ArrowUp":    {"ArrowUp", 38, ""},
	"ArrowDown":  {"ArrowDown", 40, ""},
	"ArrowLeft":  {"ArrowLeft", 37, ""},
	"ArrowRight": {"ArrowRight", 39, ""},
	"PageUp":     {"PageUp", 33, ""},
	"PageDown":   {"PageDown", 34, ""},
	"Home":       {"Home", 36, ""},
	"End":        {"End", 35, ""},
	"Space":      {"Space", 32, " "},
}

func keyNames() string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (b *instance) press(ctx context.Context, session, name string) error {
	k := keys[name]
	domKey := name
	if name == "Space" {
		domKey = " "
	}
	down := map[string]any{"type": "rawKeyDown", "key": domKey, "code": k.code, "windowsVirtualKeyCode": k.vk}
	if k.text != "" {
		down["type"], down["text"] = "keyDown", k.text
	}
	up := map[string]any{"type": "keyUp", "key": domKey, "code": k.code, "windowsVirtualKeyCode": k.vk}
	for _, ev := range []map[string]any{down, up} {
		if err := b.cdp.Call(ctx, session, "Input.dispatchKeyEvent", ev, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *instance) close(ctx context.Context, tab string) error {
	b.mu.Lock()
	mine := b.agent[tab]
	b.mu.Unlock()
	if !mine {
		if _, err := b.info(ctx, tab); err != nil {
			return err
		}
		return ErrNotAgentTab
	}
	if b.kind == Personal {
		tabs, err := b.tabs(ctx)
		if err != nil {
			return err
		}
		if len(tabs) == 1 && tabs[0].ID == tab {
			if err := b.cdp.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, nil); err != nil {
				return err
			}
		}
	}
	if err := b.cdp.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": tab}, nil); err != nil {
		return noTab(err)
	}
	b.forget(tab)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := b.info(ctx, tab); errors.Is(err, ErrNoTab) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "\n… (cut: the page goes on)"
	s = s[:n-len(more)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + more
}
