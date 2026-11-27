//go:build unix

package cliworker_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestTurnThroughTheProxy(t *testing.T) {
	h := newHarness(t, nil)
	dirs, err := h.layout.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	const token = "tt1.payload-for-the-test.signature"
	proxy := &cliworker.Proxy{URL: "http://taskto:8081", Token: token}
	for _, cli := range []cliworker.CLI{cliworker.Claude, cliworker.Codex, cliworker.Grok} {
		t.Run(string(cli), func(t *testing.T) {
			s := spec("proxy-"+string(cli), 1, cli, "hello")
			s.Slot, s.Proxy = "s7", proxy
			config := dirs.ConfigOf(cli, "s7")
			if err := os.MkdirAll(config, 0o700); err != nil {
				t.Fatal(err)
			}
			turn, _, err := h.w.Start(s)
			if err != nil {
				t.Fatal(err)
			}
			if done := last(waitDone(t, turn, 20*time.Second)); done.Status != cliworker.StatusOK {
				t.Fatalf("turn: %+v", done)
			}
			calls := clitest.Calls(t, config)
			if len(calls) == 0 {
				t.Fatal("the turn didn't run in the shared account's directory")
			}
			env := calls[len(calls)-1].Env
			want := map[cliworker.CLI][]string{
				cliworker.Claude: {"ANTHROPIC_BASE_URL=http://taskto:8081/cli-proxy", "ANTHROPIC_AUTH_TOKEN=" + token},
				cliworker.Codex:  {"TASKTO_PROXY_TOKEN=" + token},
				cliworker.Grok:   {"GROK_CLI_CHAT_PROXY_BASE_URL=http://taskto:8081/cli-proxy/v1", "XAI_API_KEY=" + token},
			}[cli]
			for _, v := range want {
				if !slices.Contains(env, v) {
					t.Errorf("env lacks %s: %v", v, env)
				}
			}
			if cli == cliworker.Codex {
				b, err := os.ReadFile(filepath.Join(config, "config.toml"))
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range []string{`model_provider = "taskto"`, `base_url = "http://taskto:8081/cli-proxy/v1"`, `env_key = "TASKTO_PROXY_TOKEN"`, `wire_api = "responses"`} {
					if !strings.Contains(string(b), line) {
						t.Errorf("config.toml lacks %s:\n%s", line, b)
					}
				}
				if strings.Contains(string(b), token) {
					t.Error("the token is written into config.toml")
				}
			}
		})
	}

	s := spec("proxy-mismatch", 1, cliworker.Claude, "hello")
	s.Proxy = proxy
	if _, _, err := h.w.Start(s); err == nil {
		t.Error("a personal account's turn took the proxy")
	}
	s.Slot, s.Proxy = "s7", nil
	if _, _, err := h.w.Start(s); err == nil {
		t.Error("a shared account's turn ran without the proxy")
	}
	s.Proxy = &cliworker.Proxy{URL: "http://user:pw@taskto:8081", Token: token}
	if _, _, err := h.w.Start(s); err == nil {
		t.Error("a proxy URL with credentials was taken")
	}
	s.Proxy = &cliworker.Proxy{URL: "http://taskto:8081", Token: "short"}
	if _, _, err := h.w.Start(s); err == nil {
		t.Error("a malformed token was taken")
	}
}

func TestRemoteHidesTheProxyToken(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	s := spec("remote-proxy", 1, cliworker.Grok, "hello")
	s.Slot, s.Proxy = "s2", &cliworker.Proxy{URL: "http://taskto:8081", Token: "tt1.payload-for-the-test.signature"}
	if _, _, err := r.StartTurn(s); err != nil {
		t.Fatal(err)
	}
	found, ok := r.FindTurn(s.ID)
	if !ok {
		t.Fatal("turn not found")
	}
	if p := found.Spec().Proxy; p == nil || p.Token != "" || p.URL != "http://taskto:8081" {
		t.Fatalf("proxy = %+v", p)
	}
}
