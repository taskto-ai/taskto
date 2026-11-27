//go:build unix

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func realHost(t *testing.T, needDisplay bool) *LocalHost {
	t.Helper()
	if os.Getenv("TASKTO_BROWSER_IT") != "1" {
		t.Skip("a real browser test: set TASKTO_BROWSER_IT=1 to run it")
	}
	bins := FindBins(os.Getenv("TASKTO_BROWSER_CHROMIUM"))
	if bins[Chromium] == "" || (needDisplay && (bins[Xvfb] == "" || bins[X11vnc] == "")) {
		t.Fatalf("TASKTO_BROWSER_IT=1 but the programs aren't all installed: %v", bins)
	}
	return &LocalHost{Root: t.TempDir(), Bins: bins, Path: os.Getenv("PATH")}
}

func newRealManager(t *testing.T, h *LocalHost, cfg Config) *Manager {
	t.Helper()
	cfg.Host = h
	cfg.NoSandbox = os.Geteuid() == 0
	cfg.StartTimeout = 60 * time.Second
	m := NewManager(cfg)
	t.Cleanup(m.Close)
	return m
}

func testSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, title, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>%s</title><body>%s</body>", title, body)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		page(w, "Home", `<h1>Home page</h1><p>Some <b>text</b> here.</p>
			<a href="/next">Next page</a>
			<button onclick="document.title='clicked'">Press me</button>
			<div style="display:none"><a href="/hidden">Hidden link</a></div>
			<iframe title="Captcha" src="/next"></iframe>`)
	})
	mux.HandleFunc("GET /next", func(w http.ResponseWriter, _ *http.Request) {
		page(w, "Next", `<p>The next page.</p>`)
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		page(w, "Sign in", `<form method="post" action="/login">
			<label>Email <input name="email"></label>
			<label>Password <input type="password" name="password"></label>
			<button type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("password") != "correct horse" {
			page(w, "Sign in", `<p>Wrong password.</p>`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session",
			Value: r.FormValue("email"), Path: "/", MaxAge: 3600, HttpOnly: true})
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil {
			page(w, "Account", "<p>Signed in as "+c.Value+"</p>")
			return
		}
		page(w, "Account", "<p>Not signed in</p>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var refLine = regexp.MustCompile(`^\[(e[0-9]+)\] (.*)$`)

func refFor(t *testing.T, snapshot, text string) string {
	t.Helper()
	for _, line := range strings.Split(snapshot, "\n") {
		if m := refLine.FindStringSubmatch(line); m != nil && strings.Contains(m[2], text) {
			return m[1]
		}
	}
	t.Fatalf("no element with %q in:\n%s", text, snapshot)
	return ""
}

func do(t *testing.T, m *Manager, user int64, a Action) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := m.Do(ctx, user, a)
	if err != nil {
		t.Fatalf("%s: %v", a.Op, err)
	}
	return res
}

func status(t *testing.T, m *Manager, user int64) map[Kind]State {
	t.Helper()
	st, err := m.Status(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func snapshot(t *testing.T, m *Manager, user int64, kind Kind, tab string) string {
	t.Helper()
	return do(t, m, user, Action{Op: OpSnapshot, Browser: kind, Tab: tab}).Snapshot
}

func signIn(t *testing.T, m *Manager, user int64, kind Kind, site string) string {
	t.Helper()
	tab := do(t, m, user, Action{Op: OpOpen, Browser: kind, URL: site + "/login"}).Tab.ID
	snap := snapshot(t, m, user, kind, tab)
	if !HasPasswordField(snap) {
		t.Fatalf("HasPasswordField doesn't see the login page's password field:\n%s", snap)
	}
	do(t, m, user, Action{Op: OpType, Browser: kind, Tab: tab, Ref: refFor(t, snap, "Email"), Text: "a@example.com"})
	do(t, m, user, Action{Op: OpType, Browser: kind, Tab: tab, Ref: refFor(t, snap, "Password"), Text: "correct horse"})
	snap = snapshot(t, m, user, kind, tab)
	if strings.Contains(snap, "correct horse") || !strings.Contains(snap, `input password "Password" (filled)`) {
		t.Fatalf("the password field in the snapshot:\n%s", snap)
	}
	res := do(t, m, user, Action{Op: OpClick, Browser: kind, Tab: tab, Ref: refFor(t, snap, `button "Sign in"`)})
	if !strings.HasSuffix(res.Tab.URL, "/account") {
		t.Fatalf("after signing in: %+v", res.Tab)
	}
	if snap := snapshot(t, m, user, kind, tab); !strings.Contains(snap, "Signed in as a@example.com") {
		t.Fatalf("the account page:\n%s", snap)
	}
	return tab
}

func TestRealTestingBrowser(t *testing.T) {
	h := realHost(t, false)
	site := testSite(t)
	m := newRealManager(t, h, Config{})

	if res := do(t, m, 1, Action{Op: OpTabs, Browser: Testing}); len(res.Tabs) != 0 || status(t, m, 1)[Testing].Running {
		t.Fatalf("tabs before start: %+v", res)
	}
	tab := do(t, m, 1, Action{Op: OpOpen, Browser: Testing, URL: site.URL + "/"}).Tab
	if !tab.Agent || tab.Title != "Home" {
		t.Fatalf("opened %+v", tab)
	}
	snap := snapshot(t, m, 1, Testing, tab.ID)
	for _, want := range []string{"Title: Home", "URL: " + site.URL + "/", "Home page", "Some text here.", `link "Next page" -> /next`, `button "Press me"`, "[frame] Captcha"} {
		if !strings.Contains(snap, want) {
			t.Errorf("the snapshot lacks %q:\n%s", want, snap)
		}
	}
	if strings.Contains(snap, "Hidden link") {
		t.Errorf("a hidden element is in the snapshot:\n%s", snap)
	}
	if res := do(t, m, 1, Action{Op: OpClick, Browser: Testing, Tab: tab.ID, Ref: refFor(t, snap, "Press me")}); res.Tab.Title != "clicked" {
		t.Errorf("after the click: %+v", res.Tab)
	}
	res := do(t, m, 1, Action{Op: OpClick, Browser: Testing, Tab: tab.ID, Ref: refFor(t, snap, "Next page")})
	if !strings.HasSuffix(res.Tab.URL, "/next") {
		t.Fatalf("after the link: %+v", res.Tab)
	}
	ctx := context.Background()
	if _, err := m.Do(ctx, 1, Action{Op: OpClick, Browser: Testing, Tab: tab.ID, Ref: "e1"}); !errors.Is(err, ErrStaleRef) {
		t.Errorf("a ref from the page before: %v", err)
	}
	if _, err := m.Do(ctx, 1, Action{Op: OpSnapshot, Browser: Testing, Tab: "NOSUCHTAB"}); !errors.Is(err, ErrNoTab) {
		t.Errorf("an unknown tab: %v", err)
	}

	signIn(t, m, 1, Testing, site.URL)

	s, err := m.session(ctx, 1, Testing, false)
	if err != nil {
		t.Fatal(err)
	}
	var mine struct {
		TargetID string `json:"targetId"`
	}
	if err := s.inst.cdp.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &mine); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, 1, Action{Op: OpClose, Browser: Testing, Tab: mine.TargetID}); !errors.Is(err, ErrNotAgentTab) {
		t.Errorf("closing the user's tab: %v", err)
	}
	do(t, m, 1, Action{Op: OpClose, Browser: Testing, Tab: tab.ID})
	for _, open := range do(t, m, 1, Action{Op: OpTabs, Browser: Testing}).Tabs {
		if open.ID == tab.ID {
			t.Error("the closed tab is still listed")
		}
	}

	if err := m.Stop(context.Background(), 1, Testing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Paths(1).Testing()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the testing profile is still there: %v", err)
	}
	again := do(t, m, 1, Action{Op: OpOpen, Browser: Testing, URL: site.URL + "/account"}).Tab
	if snap := snapshot(t, m, 1, Testing, again.ID); !strings.Contains(snap, "Not signed in") {
		t.Errorf("the testing browser kept a login:\n%s", snap)
	}
}

func TestRealPersonalBrowser(t *testing.T) {
	h := realHost(t, true)
	site := testSite(t)
	m := newRealManager(t, h, Config{Idle: 2 * time.Second, Tick: 100 * time.Millisecond})
	ctx := context.Background()

	if _, err := m.OpenLive(ctx, 1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a live view of no browser: %v", err)
	}
	signIn(t, m, 1, Personal, site.URL)

	live, err := m.OpenLive(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	banner := make([]byte, 12)
	if _, err := io.ReadFull(live, banner); err != nil || string(banner) != "RFB 003.008\n" {
		t.Fatalf("the live view said %q, %v", banner, err)
	}
	time.Sleep(3 * time.Second)
	if st := status(t, m, 1)[Personal]; !st.Running || !st.Live || st.StopsAt != nil {
		t.Fatalf("with a live view open: %+v", st)
	}
	live.Close()
	deadline := time.Now().Add(20 * time.Second)
	for status(t, m, 1)[Personal].Running {
		if time.Now().After(deadline) {
			t.Fatal("the idle browser didn't stop")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tab := do(t, m, 1, Action{Op: OpOpen, Browser: Personal, URL: site.URL + "/account"}).Tab
	if snap := snapshot(t, m, 1, Personal, tab.ID); !strings.Contains(snap, "Signed in as a@example.com") {
		t.Errorf("the login wasn't kept:\n%s", snap)
	}
	do(t, m, 1, Action{Op: OpFront, Browser: Personal, Tab: tab.ID})
	for _, open := range do(t, m, 1, Action{Op: OpTabs, Browser: Personal}).Tabs {
		if open.Agent {
			do(t, m, 1, Action{Op: OpClose, Browser: Personal, Tab: open.ID})
		} else if _, err := m.Do(ctx, 1, Action{Op: OpClose, Browser: Personal, Tab: open.ID}); !errors.Is(err, ErrNotAgentTab) {
			t.Errorf("closing the first tab, which the browser opened: %v", err)
		}
	}
	if st := status(t, m, 1)[Personal]; !st.Running || len(st.Tabs) == 0 {
		t.Errorf("after closing the agent's tabs: %+v", st)
	}
}

func TestRealCap(t *testing.T) {
	h := realHost(t, false)
	m := newRealManager(t, h, Config{Max: 1})
	ctx := context.Background()
	if err := m.Start(ctx, 1, Testing); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, 2, Testing); err != nil {
		t.Fatal(err)
	}
	if status(t, m, 1)[Testing].Running || !status(t, m, 2)[Testing].Running {
		t.Errorf("user 1: %+v, user 2: %+v", status(t, m, 1)[Testing], status(t, m, 2)[Testing])
	}
}
