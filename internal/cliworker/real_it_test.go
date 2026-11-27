//go:build unix

package cliworker_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
)

// TestRealCLIs runs the installed Claude Code and Codex (and Grok Build, if installed) through
// the worker against a model gateway that speaks Anthropic Messages and OpenAI Responses under
// /v1, as the spike did (docs/spikes/cli-workers.md): no subscription is involved. It needs
//
//	TASKTO_CLI_IT_BASE_URL  the gateway's root (no /v1)
//	TASKTO_CLI_IT_KEY_FILE  a file holding a low-value key for it
//	TASKTO_CLI_IT_MODEL     a model that calls tools well there
//
// and is skipped without them. The key reaches the CLIs only through their environment,
// from the file, and only in this test; a production worker never passes a key.
func TestRealCLIs(t *testing.T) {
	base, keyFile, model := os.Getenv("TASKTO_CLI_IT_BASE_URL"), os.Getenv("TASKTO_CLI_IT_KEY_FILE"), os.Getenv("TASKTO_CLI_IT_MODEL")
	if base == "" || keyFile == "" || model == "" {
		t.Skip("set TASKTO_CLI_IT_BASE_URL, TASKTO_CLI_IT_KEY_FILE and TASKTO_CLI_IT_MODEL to run the real CLIs")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(b))
	base = strings.TrimRight(base, "/")

	bins := map[cliworker.CLI]string{}
	for _, cli := range cliworker.CLIs {
		if path, err := exec.LookPath(string(cli)); err == nil {
			bins[cli] = path
		}
	}
	path := os.Getenv("PATH")
	overrides := func(key string) map[cliworker.CLI]cliworker.Overrides {
		return map[cliworker.CLI]cliworker.Overrides{
			cliworker.Claude: {Env: []string{
				"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_AUTH_TOKEN=" + key, "ANTHROPIC_MODEL=" + model,
				"ANTHROPIC_SMALL_FAST_MODEL=" + model, "ANTHROPIC_DEFAULT_HAIKU_MODEL=" + model,
			}},
			cliworker.Codex: {
				Env: []string{"GATEWAY_KEY=" + key},
				CodexConfig: strings.Join([]string{
					"model = " + quote(model),
					`model_provider = "gateway"`,
					"[model_providers.gateway]",
					`name = "Gateway"`,
					"base_url = " + quote(base+"/v1"),
					`env_key = "GATEWAY_KEY"`,
					`wire_api = "responses"`,
				}, "\n"),
			},
			cliworker.Grok: {Env: []string{"XAI_API_KEY=" + key, "GROK_CLI_CHAT_PROXY_BASE_URL=" + base + "/v1"}},
		}
	}
	newWorker := func(key string) *cliworker.Worker {
		w, err := cliworker.New(cliworker.Config{
			Layout:      cliworker.Layout{Root: filepath.Join(t.TempDir(), "cli")},
			Bins:        bins,
			Path:        path,
			Overrides:   overrides(key),
			TurnTimeout: 4 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Close)
		return w
	}
	w := newWorker(key)

	for cli := range bins {
		t.Run(string(cli), func(t *testing.T) {
			s := spec("real-"+string(cli), 1, cli, "Run the shell command `echo worker-ok > proof.txt`, then reply with exactly: done")
			s.Permissions = cliworker.AllowAll
			events := runLong(t, w, s)
			done := last(events)
			if done.Status != cliworker.StatusOK {
				t.Fatalf("turn: %+v", done)
			}
			if !strings.Contains(strings.ToLower(done.Text), "done") {
				t.Logf("answer %q (the model's wording, not checked strictly)", done.Text)
			}
			if _, ok := find(events, cliworker.EventTool); !ok {
				t.Errorf("no tool call: %+v", events)
			}
			session, ok := find(events, cliworker.EventSession)
			if !ok {
				t.Fatalf("no session: %+v", events)
			}

			r := spec("real-resume-"+string(cli), 1, cli, "What did you write into proof.txt? Reply with just its content.")
			r.Session, r.Resume, r.Permissions = session.Session, true, cliworker.AllowAll
			resumed := runLong(t, w, r)
			if done := last(resumed); done.Status != cliworker.StatusOK || !strings.Contains(done.Text, "worker-ok") {
				t.Errorf("resumed turn: %+v", done)
			}
		})
	}

	// Asking (docs/agent-safety.md): the CLI's hook asks the worker before the command runs,
	// and the answer decides whether it does.
	for cli := range bins {
		for _, allow := range []bool{true, false} {
			name := fmt.Sprintf("%s-ask-%t", cli, allow)
			t.Run(name, func(t *testing.T) {
				file := fmt.Sprintf("asked-%t.txt", allow)
				s := spec("real-"+name, 1, cli, "Run the shell command `echo asked > "+file+"` with your shell tool, then reply with exactly: done")
				turn, _, err := w.Start(s)
				if err != nil {
					t.Fatal(err)
				}
				// Answer every question the turn asks, the same way, as the owner would.
				asked := make(chan cliworker.PermissionRequest, 16)
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					seen := map[string]bool{}
					for {
						select {
						case <-stop:
							return
						case <-time.After(100 * time.Millisecond):
						}
						for _, q := range w.Permissions(acct(1), 7) {
							if !seen[q.ID] {
								seen[q.ID] = true
								asked <- q
								_ = w.Answer(acct(1), 7, q.ID, allow)
								// A model told no may try again and again: a minute after the first
								// denial, the owner stops the turn.
								if !allow && len(seen) == 1 {
									time.AfterFunc(time.Minute, func() { w.Stop(s.ID) })
								}
							}
						}
					}
				}()
				events := waitDone(t, turn, 5*time.Minute)
				if done := last(events); done.Status != cliworker.StatusOK && (allow || done.Status != cliworker.StatusStopped) {
					t.Fatalf("turn: %+v", done)
				}
				select {
				case q := <-asked:
					t.Logf("%s asked: %s %q", cli, q.Tool, q.Input)
				default:
					t.Fatalf("%s didn't ask: %+v", cli, events)
				}
				_, statErr := os.Stat(filepath.Join(w.LayoutForTest().Home(w.HomeForTest(cliworker.AccountForTest(1))).Conversation(7), file))
				if allow && statErr != nil {
					t.Errorf("allowed, but the command didn't run: %v", statErr)
				}
				if !allow && statErr == nil {
					t.Errorf("denied, but the command ran: %+v", events)
				}
			})
		}
	}

	// A refused key looks like an expired login, and is reported at once.
	bad := newWorker("invalid-key-for-the-test")
	for cli := range bins {
		t.Run(string(cli)+"-refused", func(t *testing.T) {
			start := time.Now()
			events := runLong(t, bad, spec("refused-"+string(cli), 1, cli, "hi"))
			if done := last(events); done.Status != cliworker.StatusLoginRequired {
				t.Fatalf("turn with a refused key: %+v", done)
			}
			// Refused by the gateway, not by something else that looks like it.
			if e, _ := find(events, cliworker.EventLoginRequired); !strings.Contains(e.Text, "401") && !strings.Contains(e.Text, "authentication_failed") {
				t.Errorf("login_required for another reason: %+v", events)
			}
			t.Logf("refused after %s: %+v", time.Since(start), events)
			if took := time.Since(start); took > time.Minute {
				t.Errorf("took %s", took)
			}
		})
	}
}

func runLong(t *testing.T, w *cliworker.Worker, s cliworker.TurnSpec) []cliworker.Event {
	t.Helper()
	turn, _, err := w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	return waitDone(t, turn, 5*time.Minute)
}

func quote(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
