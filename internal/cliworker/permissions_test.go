//go:build unix

package cliworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// The permission prompts (docs/agent-safety.md): each fake CLI asks its permission hook the
// way the real one does, the hook asks the worker, and the worker asks the conversation's
// owner, here the test.

var touch = &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "touch proof.txt"}}

// question waits for the one question in user's conversation 7.
func question(t *testing.T, w *cliworker.Worker, user int64) cliworker.PermissionRequest {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if qs := w.Permissions(user, 7); len(qs) > 0 {
			if len(qs) != 1 {
				t.Fatalf("questions = %+v", qs)
			}
			return qs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no permission question came")
	return cliworker.PermissionRequest{}
}

func permissionEvents(events []cliworker.Event) []cliworker.Permission {
	var out []cliworker.Permission
	for _, e := range events {
		if e.Type == cliworker.EventPermission && e.Permission != nil {
			out = append(out, *e.Permission)
		}
	}
	return out
}

func toolOutcome(answer string) string {
	for _, line := range strings.Split(answer, "\n") {
		if rest, ok := strings.CutPrefix(line, "tool "); ok {
			_, outcome, _ := strings.Cut(rest, ": ")
			return outcome
		}
	}
	return "(no tool)"
}

func TestPermissionAskedAndAnswered(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		for _, allow := range []bool{true, false} {
			name := string(cli) + map[bool]string{true: "-allowed", false: "-denied"}[allow]
			t.Run(name, func(t *testing.T) {
				h.user(t, 1, cli, clitest.Scenario{Tool: touch}, true)
				turn, _, err := h.w.Start(spec("ask-"+name, 1, cli, "make the file"))
				if err != nil {
					t.Fatal(err)
				}
				q := question(t, h.w, 1)
				if q.Tool != "Bash" || q.Input != "touch proof.txt" || q.CLI != cli || q.Turn != "ask-"+name {
					t.Errorf("question = %+v", q)
				}
				// Nobody else answers it: another user, or the same user in another conversation.
				if err := h.w.Answer(2, 7, q.ID, true); !errors.Is(err, cliworker.ErrNoPermission) {
					t.Errorf("another user's answer: %v", err)
				}
				if err := h.w.Answer(1, 8, q.ID, true); !errors.Is(err, cliworker.ErrNoPermission) {
					t.Errorf("an answer from another conversation: %v", err)
				}
				if qs := h.w.Permissions(2, 7); len(qs) != 0 {
					t.Errorf("another user sees %+v", qs)
				}
				if err := h.w.Answer(1, 7, q.ID, allow); err != nil {
					t.Fatal(err)
				}
				events := waitDone(t, turn, 20*time.Second)
				done := last(events)
				if done.Status != cliworker.StatusOK {
					t.Fatalf("done = %+v", done)
				}
				want := "ran"
				if !allow {
					want = "denied: Not allowed: the user denied it. Don't try to get around this; ask the user if you need it."
				}
				if got := toolOutcome(done.Text); got != want {
					t.Errorf("tool: %s, want %s", got, want)
				}
				ps := permissionEvents(events)
				wantStatus := map[bool]string{true: cliworker.PermissionAllowed, false: cliworker.PermissionDenied}[allow]
				if len(ps) != 2 || ps[0].Status != cliworker.PermissionAsked || ps[0].Input != "touch proof.txt" || ps[1].Status != wantStatus || ps[1].ID != q.ID {
					t.Errorf("permission events = %+v", ps)
				}
				if qs := h.w.Permissions(1, 7); len(qs) != 0 {
					t.Errorf("still waiting: %+v", qs)
				}
				if err := h.w.Answer(1, 7, q.ID, true); !errors.Is(err, cliworker.ErrNoPermission) {
					t.Errorf("a second answer: %v", err)
				}
			})
		}
	}
}

// With allow_all nothing asks; and reads in the working directory don't ask either.
func TestPermissionNotAsked(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		name  string
		cli   cliworker.CLI
		tool  *clitest.ToolUse
		perms cliworker.Permissions
	}{
		{"claude allow_all", cliworker.Claude, touch, cliworker.AllowAll},
		{"codex allow_all", cliworker.Codex, touch, cliworker.AllowAll},
		{"grok allow_all", cliworker.Grok, touch, cliworker.AllowAll},
		{"claude reads", cliworker.Claude, &clitest.ToolUse{Name: "Read", Input: map[string]any{"file_path": "notes.md"}}, cliworker.Ask},
		{"codex lists", cliworker.Codex, &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "ls -la src"}}, cliworker.Ask},
		{"grok greps", cliworker.Grok, &clitest.ToolUse{Name: "Grep", Input: map[string]any{"pattern": "TODO", "path": "."}}, cliworker.Ask},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.user(t, 1, c.cli, clitest.Scenario{Tool: c.tool}, true)
			s := spec("free-"+strings.ReplaceAll(c.name, " ", "-"), 1, c.cli, "go")
			s.Permissions = c.perms
			events := run(t, h.w, s)
			if got := toolOutcome(last(events).Text); got != "ran" {
				t.Errorf("tool: %s", got)
			}
			if ps := permissionEvents(events); len(ps) != 0 {
				t.Errorf("asked: %+v", ps)
			}
		})
	}
}

// A question nobody answers is denied in time, and one whose turn is stopped goes with it.
func TestPermissionUnanswered(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.AskTimeout = 300 * time.Millisecond })
	h.user(t, 1, cliworker.Codex, clitest.Scenario{Tool: touch}, true)
	events := run(t, h.w, spec("unanswered", 1, cliworker.Codex, "go"))
	if got := toolOutcome(last(events).Text); !strings.Contains(got, "nobody answered in 300ms") {
		t.Errorf("tool: %s", got)
	}

	h = newHarness(t, nil)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{Tool: touch}, true)
	turn, _, err := h.w.Start(spec("stopped", 1, cliworker.Claude, "go"))
	if err != nil {
		t.Fatal(err)
	}
	question(t, h.w, 1)
	h.w.Stop("stopped")
	// The stop kills the turn's process group, its hook with it. The CLI can read the dead hook
	// as a denial, answer and exit 0 before it is killed too, and a CLI that finished is
	// reported done (TestStopRacingTheEnd). Either way the tool didn't run.
	done := last(waitDone(t, turn, 20*time.Second))
	denied := done.Status == cliworker.StatusOK && strings.HasPrefix(toolOutcome(done.Text), "denied: ")
	if done.Status != cliworker.StatusStopped && !denied {
		t.Errorf("done = %+v", done)
	}
	if qs := h.w.Permissions(1, 7); len(qs) != 0 {
		t.Errorf("still waiting after the stop: %+v", qs)
	}
}

// The hook program denies whenever it can't get an answer, in each event's format, and exits
// 2, which every CLI reads as "block".
func TestApproveFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no.sock")
	cases := []struct {
		name, input string
		args        []string
		want        string
	}{
		{"no worker, PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Bash"}`, []string{missing, "codex"}, `"permissionDecision":"deny"`},
		{"no worker, PermissionRequest", `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash"}`, []string{missing, "claude"}, `"behavior":"deny"`},
		{"bad input", `not json`, []string{missing, "grok"}, `"permissionDecision":"deny"`},
		{"no arguments", `{"hook_event_name":"PreToolUse"}`, nil, `"permissionDecision":"deny"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := cliworker.RunApproveForTest(c.args, strings.NewReader(c.input), &out, &errOut)
			// Codex ignores an exit 2 with nothing on stderr: the reason must be there too.
			if code != 2 || !strings.Contains(out.String(), c.want) || !json.Valid(bytes.TrimSpace(out.Bytes())) || !strings.Contains(errOut.String(), "Not allowed") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
			}
		})
	}

	// A running worker that knows no such session denies too.
	h := newHarness(t, nil)
	var out bytes.Buffer
	sock := h.approveSocket(t)
	code := cliworker.RunApproveForTest([]string{sock, "claude"}, strings.NewReader(`{"hook_event_name":"PermissionRequest","session_id":"nobody","tool_name":"Bash"}`), &out, &bytes.Buffer{})
	if code != 2 || !strings.Contains(out.String(), "no running turn") {
		t.Errorf("unknown session: exit %d, %s", code, out.String())
	}
}

// approveSocket finds the worker's approval socket from the hook it writes into a CLI's
// settings.
func (h *harness) approveSocket(t *testing.T) string {
	t.Helper()
	h.user(t, 1, cliworker.Codex, clitest.Scenario{}, true)
	run(t, h.w, spec("find-socket", 1, cliworker.Codex, "hi"))
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, err := os.ReadFile(filepath.Join(h.layout.User(1).Config(cliworker.Codex), "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(settings.Hooks["PreToolUse"][0].Hooks[0].Command)
	return strings.Trim(words[2], "'")
}

func TestReadOnlyPolicy(t *testing.T) {
	dir := "/homes/u1/work/c7"
	cases := []struct {
		tool, input string
		want        bool
	}{
		{"Read", `{"file_path":"/homes/u1/work/c7/notes.md"}`, true},
		{"Read", `{"file_path":"notes/a.md"}`, true},
		{"Read", `{"file_path":"/homes/u1/config/claude/.credentials.json"}`, false},
		{"Read", `{"file_path":"../../config/codex/auth.json"}`, false},
		{"Read", `{"file_path":"~/x"}`, false},
		{"Glob", `{"pattern":"**/*.go"}`, true},
		{"Glob", `{"pattern":"/etc/*"}`, false},
		{"Grep", `{"pattern":"TODO","path":"src"}`, true},
		{"LS", `{"path":"/"}`, false},
		{"TodoWrite", `{"todos":[]}`, true},
		{"Write", `{"file_path":"a.txt","content":"x"}`, false},
		{"Edit", `{"file_path":"a.txt"}`, false},
		{"WebFetch", `{"url":"https://example.com"}`, false},
		{"mcp__x__y", `{}`, false},
		{"Bash", `{"command":"ls -la"}`, true},
		{"Bash", `{"command":"cat README.md"}`, true},
		{"Bash", `{"command":"rg -n 'func main' --type go"}`, true},
		{"Bash", `{"command":"git status"}`, false}, // .git/config can run commands
		{"Bash", `{"command":"git log --oneline -5"}`, false},
		{"Bash", `{"command":"git diff HEAD~1"}`, false}, // ~ is refused anywhere
		{"Bash", `{"command":"git branch -a"}`, false},
		{"Bash", `{"command":"git branch evil"}`, false},
		{"Bash", `{"command":"git -c core.pager=sh status"}`, false},
		{"Bash", `{"command":"git log --output=x"}`, false},
		{"Bash", `{"command":"git push"}`, false},
		{"Bash", `{"command":"cat /etc/passwd"}`, false},
		{"Bash", `{"command":"cat ../../config/claude/.credentials.json"}`, false},
		{"Bash", `{"command":"cat notes.md > copy.md"}`, false},
		{"Bash", `{"command":"cat a; rm -rf b"}`, false},
		{"Bash", `{"command":"cat a && rm b"}`, false},
		{"Bash", `{"command":"cat a | sh"}`, false},
		{"Bash", `{"command":"cat $(whoami)"}`, false},
		{"Bash", "{\"command\":\"cat `whoami`\"}", false},
		{"Bash", `{"command":"cat \"$HOME/x\""}`, false},
		{"Bash", `{"command":"echo 'a; b'"}`, true},
		{"Bash", `{"command":"cat *.md"}`, false},
		{"Bash", `{"command":"cat ~/x"}`, false},
		{"Bash", `{"command":"cat a\nrm b"}`, false},
		{"Bash", `{"command":"find . -name x -exec rm {} +"}`, false},
		{"Bash", `{"command":"find . -delete"}`, false},
		{"Bash", `{"command":"find . -name x.go"}`, true},
		{"Bash", `{"command":"rg --pre=sh x"}`, false},
		{"Bash", `{"command":"rg -z x"}`, false},
		{"Bash", `{"command":"rg -zi x"}`, false},
		{"Bash", `{"command":"rg -inz x"}`, false},
		{"Bash", `{"command":"rg -in x"}`, true},
		{"Bash", `{"command":"tree -ao out.txt"}`, false},
		{"Bash", `{"command":"find . -name x.go -type f"}`, true},
		{"Bash", `{"command":"tree -o out.txt"}`, false},
		{"Bash", `{"command":"head --file=/etc/shadow"}`, false},
		{"Bash", `{"command":"rm -rf ."}`, false},
		{"Bash", `{"command":"curl https://example.com"}`, false},
		{"Bash", `{"command":"sed -n 1p a"}`, false},
		{"Bash", `{"command":"date"}`, true},
		{"Bash", `{"command":"date -s 2020-01-01"}`, false},
		{"Bash", `{"command":"date --set=2020-01-01"}`, false},
		{"Bash", `{"command":"env"}`, false},
		{"Bash", `{"command":"ls","workdir":"/tmp"}`, false},
		{"Bash", `{"command":"ls","workdir":"/homes/u1/work/c7/src"}`, true},
		{"Bash", `{"command":["ls"]}`, false},
		{"shell", `{"command":"pwd"}`, true},
		{"Bash", `not json`, false},
	}
	for _, c := range cases {
		if got := cliworker.ReadOnlyForTest(c.tool, json.RawMessage(c.input), dir); got != c.want {
			t.Errorf("%s %s: %t, want %t", c.tool, c.input, got, c.want)
		}
	}
}

// The server lists and answers a conversation's questions through the worker's API too.
func TestPermissionsThroughTheRemote(t *testing.T) {
	h := newHarness(t, nil)
	remote := serve(t, h)
	h.user(t, 1, cliworker.Grok, clitest.Scenario{Tool: touch}, true)
	turn, _, err := h.w.Start(spec("remote-ask", 1, cliworker.Grok, "go"))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, h.w, 1)
	ctx := context.Background()
	qs, err := remote.Permissions(ctx, 1, 7)
	if err != nil || len(qs) != 1 || qs[0].ID != q.ID || qs[0].Tool != "Bash" {
		t.Fatalf("remote questions = %+v, %v", qs, err)
	}
	if err := remote.Answer(ctx, 2, 7, q.ID, true); !errors.Is(err, cliworker.ErrNoPermission) {
		t.Errorf("another user's answer through the remote: %v", err)
	}
	if err := remote.Answer(ctx, 1, 7, q.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := toolOutcome(last(waitDone(t, turn, 20*time.Second)).Text); got != "ran" {
		t.Errorf("tool: %s", got)
	}
}

// A hook's question reaches a turn only if both its session and its CLI are the turn's.
func TestApproveNeedsTheTurnsCLI(t *testing.T) {
	h := newHarness(t, nil)
	sock := h.approveSocket(t)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{Tool: touch}, true)
	s := spec("cli-check", 1, cliworker.Claude, "go")
	turn, _, err := h.w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, h.w, 1)
	var out bytes.Buffer
	input := `{"hook_event_name":"PreToolUse","session_id":"` + s.Session + `","tool_name":"Bash","tool_input":{"command":"ls"}}`
	if code := cliworker.RunApproveForTest([]string{sock, "codex"}, strings.NewReader(input), &out, &bytes.Buffer{}); code != 2 || !strings.Contains(out.String(), "no running turn") {
		t.Errorf("another CLI's hook: exit %d, %s", code, out.String())
	}
	if err := h.w.Answer(1, 7, q.ID, true); err != nil {
		t.Fatal(err)
	}
	waitDone(t, turn, 20*time.Second)
}

// A layout root too long for a socket path puts the socket in a directory of its own, which
// every CLI user can pass through when each has a Unix user of their own.
func TestApprovalSocketFallback(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if path, dir, err := cliworker.ApprovalSocketForTest("/homes", true); err != nil || dir != "" || filepath.Base(path) != ".approve.sock" {
		t.Errorf("short root: %s %s %v", path, dir, err)
	}
	for shared, want := range map[bool]os.FileMode{true: 0o711, false: 0o700} {
		path, dir, err := cliworker.ApprovalSocketForTest(long, shared)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		info, err := os.Stat(dir)
		if err != nil || len(path) > 100 || info.Mode().Perm() != want {
			t.Errorf("shared %t: %s in %s, mode %v, %v", shared, path, dir, info.Mode().Perm(), err)
		}
	}
}

// A turn on a user's second account asks too: its hook is written into that account's own
// config directory, where the CLI reads it.
func TestPermissionOnAnotherAccountSlot(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			dirs, err := h.layout.EnsureSlot(1, cli, "2")
			if err != nil {
				t.Fatal(err)
			}
			clitest.SignIn(t, string(cli), dirs.ConfigOf(cli, "2"))
			clitest.WriteScenario(t, dirs.ConfigOf(cli, "2"), clitest.Scenario{Tool: touch})
			s := spec("slot-ask-"+string(cli), 1, cli, "go")
			s.Slot = "2"
			turn, _, err := h.w.Start(s)
			if err != nil {
				t.Fatal(err)
			}
			q := question(t, h.w, 1)
			if err := h.w.Answer(1, 7, q.ID, false); err != nil {
				t.Fatal(err)
			}
			if got := toolOutcome(last(waitDone(t, turn, 20*time.Second)).Text); !strings.HasPrefix(got, "denied") {
				t.Errorf("tool: %s", got)
			}
		})
	}
}
