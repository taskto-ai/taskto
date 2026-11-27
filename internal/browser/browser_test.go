package browser

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fakeBrowser(t *testing.T, answer func(method string, params json.RawMessage) (any, *cdpError)) *cdp {
	t.Helper()
	toBrowserR, toBrowserW := io.Pipe()
	fromBrowserR, fromBrowserW := io.Pipe()
	go func() {
		defer fromBrowserW.Close()
		br := bufio.NewReader(toBrowserR)
		for {
			msg, err := readMessage(br)
			if err != nil {
				return
			}
			var m struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(msg, &m); err != nil {
				t.Errorf("not JSON: %q", msg)
				return
			}
			_, _ = fromBrowserW.Write([]byte(`{"method":"Target.targetInfoChanged","params":{}}` + "\x00"))
			res, cerr := answer(m.Method, m.Params)
			out := map[string]any{"id": m.ID}
			if cerr != nil {
				out["error"] = cerr
			} else {
				out["result"] = res
			}
			b, _ := json.Marshal(out)
			if _, err := fromBrowserW.Write(append(b, 0)); err != nil {
				return
			}
		}
	}()
	c := newCDP(toBrowserW, fromBrowserR)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCDPCall(t *testing.T) {
	c := fakeBrowser(t, func(method string, params json.RawMessage) (any, *cdpError) {
		switch method {
		case "Browser.getVersion":
			return map[string]string{"product": "Chrome/1"}, nil
		case "Echo":
			return params, nil
		}
		return nil, &cdpError{Code: -32601, Message: "'" + method + "' wasn't found"}
	})
	ctx := context.Background()
	var v struct {
		Product string `json:"product"`
	}
	if err := c.Call(ctx, "", "Browser.getVersion", nil, &v); err != nil || v.Product != "Chrome/1" {
		t.Fatalf("getVersion = %+v, %v", v, err)
	}
	var echo map[string]string
	if err := c.Call(ctx, "S1", "Echo", map[string]string{"a": "b\x00c"}, &echo); err != nil || echo["a"] != "b\x00c" {
		t.Fatalf("echo = %v, %v", echo, err)
	}
	var ce *cdpError
	if err := c.Call(ctx, "", "Nope", nil, nil); !errors.As(err, &ce) || ce.Code != -32601 {
		t.Fatalf("unknown method: %v", err)
	}
	_ = c.Close()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the pipe's end wasn't noticed")
	}
	if err := c.Call(ctx, "", "Browser.getVersion", nil, nil); !errors.Is(err, errPipeClosed) {
		t.Fatalf("after close: %v", err)
	}
}

func TestCDPCallHonoursContext(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	c := fakeBrowser(t, func(string, json.RawMessage) (any, *cdpError) {
		<-block
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "", "Slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadMessageBounds(t *testing.T) {
	big := strings.Repeat("x", maxMessage+1) + "\x00"
	if _, err := readMessage(bufio.NewReader(strings.NewReader(big))); err == nil {
		t.Fatal("a message over the limit was read")
	}
	msg, err := readMessage(bufio.NewReaderSize(strings.NewReader(strings.Repeat("y", 100<<10)+"\x00"), 16))
	if err != nil || len(msg) != 100<<10 {
		t.Fatalf("a long message: %d bytes, %v", len(msg), err)
	}
}

func TestActionCheck(t *testing.T) {
	good := []Action{
		{Op: OpTabs, Browser: Personal},
		{Op: OpOpen, Browser: Testing, URL: "https://example.com/a?b=c"},
		{Op: OpOpen, Browser: Personal, URL: "about:blank"},
		{Op: OpNavigate, Browser: Personal, Tab: "ABC123", URL: "http://127.0.0.1:8080/"},
		{Op: OpClick, Browser: Personal, Tab: "ABC123", Ref: "e12"},
		{Op: OpType, Browser: Personal, Tab: "ABC123", Ref: "e1", Text: "hello", Submit: true},
		{Op: OpPress, Browser: Personal, Tab: "ABC123", Key: "Enter"},
		{Op: OpClose, Browser: Testing, Tab: "ABC123"},
	}
	for _, a := range good {
		if err := a.Check(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Action{
		{Op: OpTabs, Browser: "other"},
		{Op: OpOpen, Browser: Personal, URL: "file:///etc/passwd"},
		{Op: OpOpen, Browser: Personal, URL: "chrome://settings"},
		{Op: OpOpen, Browser: Personal, URL: "javascript:alert(1)"},
		{Op: OpOpen, Browser: Personal, URL: "https:///nohost"},
		{Op: OpNavigate, Browser: Personal, Tab: "../x", URL: "https://example.com"},
		{Op: OpClick, Browser: Personal, Tab: "ABC", Ref: "12"},
		{Op: OpType, Browser: Personal, Tab: "ABC", Ref: "e1", Text: strings.Repeat("a", maxText+1)},
		{Op: OpPress, Browser: Personal, Tab: "ABC", Key: "F13"},
		{Op: "eval", Browser: Personal},
	}
	for _, a := range bad {
		var ba *BadAction
		if err := a.Check(); !errors.As(err, &ba) {
			t.Errorf("%+v: %v", a, err)
		}
	}
}

func TestPrepare(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "u1", "browser")}
	if err := Prepare(p, ProcSpec{Program: Xvfb, Kind: Personal}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.Root, p.Home(), p.Tmp(), p.Run()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("%s: %v", dir, err)
			continue
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v", dir, info.Mode())
		}
	}
	b, err := os.ReadFile(p.Xauthority())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p.Xauthority()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cookie file: %v %v", info, err)
	}
	if binary.BigEndian.Uint16(b) != 0xffff || !strings.Contains(string(b), "MIT-MAGIC-COOKIE-1") || len(b) != 2+2+2+2+18+2+16 {
		t.Errorf("cookie file = %x", b)
	}
	again, _ := os.ReadFile(p.Xauthority())
	if err := Prepare(p, ProcSpec{Program: Xvfb, Kind: Personal}); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := os.ReadFile(p.Xauthority()); string(fresh) == string(again) {
		t.Error("the cookie didn't change on a new start")
	}

	if err := os.MkdirAll(p.Profile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("otherhost-123", filepath.Join(p.Profile(), "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Profile(), "Cookies"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(p, ProcSpec{Program: Chromium, Kind: Personal}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.Profile(), "SingletonLock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Profile(), "Cookies")); string(b) != "kept" {
		t.Error("the profile wasn't kept")
	}

	if err := os.MkdirAll(p.Testing(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Testing(), "Cookies"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(p, ProcSpec{Program: Chromium, Kind: Testing}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(p.Testing()); len(entries) != 0 {
		t.Errorf("the testing profile has %v", entries)
	}
	if err := Prepare(p, ProcSpec{Program: X11vnc, Kind: Testing}); err == nil {
		t.Error("the testing browser has no display")
	}
}

func TestCommand(t *testing.T) {
	p := Paths{Root: "/homes/u1/browser"}
	argv, env, err := Command(p, ProcSpec{Program: X11vnc, Kind: Personal, Display: 3}, "/usr/bin/x11vnc", "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(argv, " ")
	for _, want := range []string{"-unixsock /homes/u1/browser/run/vnc.sock", "-rfbport 0", "-rfbportv6 -1", "-noipv6", "-display :3", "-auth /homes/u1/browser/run/xauthority"} {
		if !strings.Contains(line, want) {
			t.Errorf("x11vnc lacks %q: %s", want, line)
		}
	}
	if !slices.Contains(env, "XAUTHORITY=/homes/u1/browser/run/xauthority") || !slices.Contains(env, "HOME=/homes/u1/browser/home") {
		t.Errorf("x11vnc env = %v", env)
	}

	argv, _, _ = Command(p, ProcSpec{Program: Xvfb, Kind: Personal}, "/usr/bin/Xvfb", "/usr/bin")
	line = strings.Join(argv, " ")
	for _, want := range []string{"-nolisten tcp", "-nolisten local", "-auth /homes/u1/browser/run/xauthority", "-displayfd 3"} {
		if !strings.Contains(line, want) {
			t.Errorf("Xvfb lacks %q: %s", want, line)
		}
	}

	for _, kind := range []Kind{Personal, Testing} {
		argv, env, _ := Command(p, ProcSpec{Program: Chromium, Kind: kind, Display: 3}, "/usr/bin/chromium", "/usr/bin")
		line := strings.Join(argv, " ")
		if !strings.Contains(line, "--remote-debugging-pipe") || strings.Contains(line, "remote-debugging-port") ||
			strings.Contains(line, "AutomationControlled") || strings.Contains(line, "--no-sandbox") {
			t.Errorf("%s: %s", kind, line)
		}
		if kind == Testing && (!strings.Contains(line, "--headless") || !strings.Contains(line, "--user-data-dir=/homes/u1/browser/testing")) {
			t.Errorf("testing: %s", line)
		}
		if kind == Personal && (strings.Contains(line, "--headless") || !strings.Contains(line, "--user-data-dir=/homes/u1/browser/profile") || !slices.Contains(env, "DISPLAY=:3")) {
			t.Errorf("personal: %s %v", line, env)
		}
	}
	argv, _, _ = Command(p, ProcSpec{Program: Chromium, Kind: Testing, NoSandbox: true}, "/usr/bin/chromium", "/usr/bin")
	if !slices.Contains(argv, "--no-sandbox") {
		t.Errorf("no sandbox: %v", argv)
	}
}

func TestCutText(t *testing.T) {
	if got := cutText("short", 100); got != "short" {
		t.Error(got)
	}
	s := strings.Repeat("é", 100)
	got := cutText(s, 80)
	if len(got) > 80 || !strings.HasSuffix(got, "(cut: the page goes on)") || !strings.HasPrefix(got, "é") {
		t.Errorf("%d %q", len(got), got)
	}
}

func TestErrorClassification(t *testing.T) {
	if err := noTab(&cdpError{Code: -32000, Message: "No target with given id found"}); !errors.Is(err, ErrNoTab) {
		t.Errorf("no target: %v", err)
	}
	if err := noTab(&cdpError{Code: -32001, Message: "No session with given id"}); !errors.Is(err, ErrNoTab) {
		t.Errorf("no session: %v", err)
	}
	other := &cdpError{Code: -32000, Message: "Not attached to an active page; No target in sight"}
	if err := noTab(other); errors.Is(err, ErrNoTab) {
		t.Errorf("another error became ErrNoTab")
	}
	for msg, want := range map[string]bool{
		"Cannot find context with specified id":   true,
		"Execution context was destroyed.":        true,
		"Some other context problem in the page":  false,
		"Uncaught TypeError: context is not here": false,
	} {
		if got := goneContext(&cdpError{Code: -32000, Message: msg}); got != want {
			t.Errorf("%q: %v", msg, got)
		}
	}
}

func TestTabsPrunesClosedTabs(t *testing.T) {
	c := fakeBrowser(t, func(method string, _ json.RawMessage) (any, *cdpError) {
		return map[string]any{"targetInfos": []map[string]string{{"targetId": "A", "type": "page", "title": "a", "url": "about:blank"}}}, nil
	})
	b := newInstance(Personal, c)
	for _, id := range []string{"A", "B"} {
		b.sessions[id], b.worlds[id], b.agent[id] = "S"+id, 1, true
	}
	tabs, err := b.tabs(context.Background())
	if err != nil || len(tabs) != 1 || tabs[0].ID != "A" || !tabs[0].Agent {
		t.Fatalf("tabs = %+v, %v", tabs, err)
	}
	if _, ok := b.sessions["B"]; ok || b.agent["B"] || b.worlds["B"] != 0 || b.sessions["A"] != "SA" {
		t.Errorf("after the list: sessions %v worlds %v agent %v", b.sessions, b.worlds, b.agent)
	}
}

func TestHasPasswordField(t *testing.T) {
	for snap, want := range map[string]bool{
		"Title: Sign in\n[e1] input text \"Email\" = \"\"\n[e2] input password \"Password\" (empty)": true,
		"[e7] input password (filled)":                               true,
		"Title: x\n[e1] input text \"Search\" = \"input password \"": false,
		"Forgot your input password ? Write to us.":                  false,
	} {
		if got := HasPasswordField(snap); got != want {
			t.Errorf("HasPasswordField(%q) = %v", snap, got)
		}
	}
}
