//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func waitLogin(t *testing.T, l *cliworker.Login) cliworker.LoginInfo {
	t.Helper()
	select {
	case <-l.Done():
	case <-time.After(20 * time.Second):
		t.Fatalf("the login didn't end: %+v", l.Info())
	}
	return l.Info()
}

func status(t *testing.T, w *cliworker.Worker, user int64, cli cliworker.CLI) cliworker.AuthStatus {
	t.Helper()
	st, err := w.Status(context.Background(), user, cli, "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Claude Code: the URL it prints is shown, and the code the sign-in page gives is pasted back.
func TestClaudeCodeLogin(t *testing.T) {
	h := newHarness(t, nil)
	dirs, _ := h.layout.Ensure(1)
	if st := status(t, h.w, 1, cliworker.Claude); st.LoggedIn {
		t.Fatal("signed in before the login")
	}
	l, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	info := l.Info()
	if info.State != cliworker.LoginWaiting || !info.NeedsCode || !info.URLTrusted || !strings.HasPrefix(info.URL, "https://claude.com/cai/oauth/authorize?") || !strings.HasSuffix(info.URL, "state=fake") {
		t.Fatalf("info = %+v", info)
	}
	// Starting again while one waits returns the same login.
	if again, _ := h.w.StartLogin(1, cliworker.Claude, ""); again != l {
		t.Error("a second start began another login")
	}
	for _, bad := range []string{"", "two words", strings.Repeat("x", 513), "line\nbreak"} {
		if err := h.w.SubmitLoginCode(1, cliworker.Claude, "", bad); !errors.Is(err, cliworker.ErrBadCode) {
			t.Errorf("code %q: %v", bad, err)
		}
	}
	if err := h.w.SubmitLoginCode(1, cliworker.Claude, "", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	if info := waitLogin(t, l); info.State != cliworker.LoginConnected {
		t.Fatalf("info = %+v", info)
	}
	if st := status(t, h.w, 1, cliworker.Claude); !st.LoggedIn || st.Method != "claude.ai" {
		t.Errorf("status = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")); err != nil {
		t.Errorf("the CLI didn't write its credentials into the user's config directory: %v", err)
	}
}

func TestClaudeCodeLoginWrongCode(t *testing.T) {
	h := newHarness(t, nil)
	l, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.w.SubmitLoginCode(1, cliworker.Claude, "", "wrong-code"); err != nil {
		t.Fatal(err)
	}
	info := waitLogin(t, l)
	if info.State != cliworker.LoginFailed || !strings.Contains(info.Message, "Invalid code") {
		t.Fatalf("info = %+v", info)
	}
	if err := h.w.SubmitLoginCode(1, cliworker.Claude, "", clitest.GoodCode); !errors.Is(err, cliworker.ErrNoLogin) {
		t.Errorf("a code for a finished login: %v", err)
	}
}

// Codex and Grok Build: the URL and the one-time code are shown; the CLI polls until the user
// confirms on the provider's page.
func TestDeviceCodeLogins(t *testing.T) {
	h := newHarness(t, nil)
	for cli, want := range map[cliworker.CLI][2]string{
		cliworker.Codex: {"https://auth.openai.com/codex/device", "FAKE-C0DE1"},
		cliworker.Grok:  {"https://accounts.x.ai/oauth2/device?user_code=GR0K-FAKE", "GR0K-FAKE"},
	} {
		t.Run(string(cli), func(t *testing.T) {
			dirs, _ := h.layout.Ensure(1)
			l, err := h.w.StartLogin(1, cli, "")
			if err != nil {
				t.Fatal(err)
			}
			info := l.Info()
			if info.State != cliworker.LoginWaiting || info.URL != want[0] || !info.URLTrusted || info.Code != want[1] || info.NeedsCode {
				t.Fatalf("info = %+v", info)
			}
			if err := h.w.SubmitLoginCode(1, cli, "", "x"); !errors.Is(err, cliworker.ErrNoCodeNeeded) {
				t.Errorf("a code for a device login: %v", err)
			}
			clitest.Approve(t, dirs.Config(cli))
			if info := waitLogin(t, l); info.State != cliworker.LoginConnected {
				t.Fatalf("info = %+v", info)
			}
			if st := status(t, h.w, 1, cli); !st.LoggedIn {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

func TestDeviceCodeDeclined(t *testing.T) {
	h := newHarness(t, nil)
	dirs, _ := h.layout.Ensure(1)
	l, err := h.w.StartLogin(1, cliworker.Codex, "")
	if err != nil {
		t.Fatal(err)
	}
	clitest.Deny(t, dirs.Config(cliworker.Codex))
	if info := waitLogin(t, l); info.State != cliworker.LoginFailed || !strings.Contains(info.Message, "declined") {
		t.Fatalf("info = %+v", info)
	}
}

func TestCancelLogin(t *testing.T) {
	h := newHarness(t, nil)
	l, err := h.w.StartLogin(1, cliworker.Grok, "")
	if err != nil {
		t.Fatal(err)
	}
	h.w.CancelLogin(1, cliworker.Grok, "")
	if info := waitLogin(t, l); info.State != cliworker.LoginCancelled {
		t.Fatalf("info = %+v", info)
	}
	// A new start begins a new login.
	again, err := h.w.StartLogin(1, cliworker.Grok, "")
	if err != nil || again == l || again.Info().State != cliworker.LoginWaiting {
		t.Fatalf("restart: %v %+v", err, again.Info())
	}
	h.w.CancelLogin(1, cliworker.Grok, "")
}

// One user's login is theirs: another user never sees it, can't answer it and can't cancel it.
func TestLoginsAreTheUsers(t *testing.T) {
	h := newHarness(t, nil)
	l, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.w.Login(2, cliworker.Claude, ""); ok {
		t.Error("user 2 sees user 1's login")
	}
	if err := h.w.SubmitLoginCode(2, cliworker.Claude, "", clitest.GoodCode); !errors.Is(err, cliworker.ErrNoLogin) {
		t.Errorf("user 2 answered user 1's login: %v", err)
	}
	h.w.CancelLogin(2, cliworker.Claude, "")
	if l.Info().State != cliworker.LoginWaiting {
		t.Errorf("user 2 cancelled user 1's login: %+v", l.Info())
	}
	if err := h.w.SubmitLoginCode(1, cliworker.Claude, "", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	if info := waitLogin(t, l); info.State != cliworker.LoginConnected {
		t.Fatalf("info = %+v", info)
	}
	if status(t, h.w, 2, cliworker.Claude).LoggedIn {
		t.Error("user 2 is signed in with user 1's login")
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			dirs := h.user(t, 1, cli, clitest.Scenario{}, true)
			run(t, h.w, spec("before-logout-"+string(cli), 1, cli, "hi")) // leaves a session
			if !status(t, h.w, 1, cli).LoggedIn {
				t.Fatal("not signed in")
			}
			if err := h.w.Logout(context.Background(), 1, cli, ""); err != nil {
				t.Fatal(err)
			}
			if status(t, h.w, 1, cli).LoggedIn {
				t.Error("still signed in")
			}
			// Emptied, but for the settings files the worker writes for every run (the status
			// check above was one).
			entries, err := os.ReadDir(dirs.Config(cli))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if !slices.Contains([]string{"config.toml", "settings.json", "hooks.json"}, e.Name()) {
					t.Errorf("the config directory wasn't emptied: %s is left", e.Name())
				}
			}
		})
	}
}

// The URL shown is the CLI's own sign-in host; another link the CLI prints doesn't win, and a
// URL on an unknown host is marked untrusted.
func TestSignInURL(t *testing.T) {
	for _, tc := range []struct {
		cli     cliworker.CLI
		text    string
		url     string
		trusted bool
	}{
		{cliworker.Codex, "See https://docs.example.com/codex for help.\nOpen https://auth.openai.com/codex/device now.", "https://auth.openai.com/codex/device", true},
		{cliworker.Claude, "visit: https://evil.example.net/oauth?x=1", "https://evil.example.net/oauth?x=1", false},
		{cliworker.Grok, "nothing here", "", false},
	} {
		url, trusted := cliworker.SignInURLForTest(tc.cli, tc.text)
		if url != tc.url || trusted != tc.trusted {
			t.Errorf("%s %q: got %q %v", tc.cli, tc.text, url, trusted)
		}
	}
}
