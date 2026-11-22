//go:build unix

package cliworker_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
)

// TestRealCLIsCallTaskToTools runs the installed Claude Code and Codex through the worker with
// TaskTo's tools (docs/cli-tools.md): a scripted model (a stand-in for Anthropic's Messages API
// and OpenAI's Responses API, on this machine) makes each CLI call one tool of a stand-in for
// the server's endpoint. No account, key or network is involved; it checks what the fakes
// can't, that the real CLIs read the settings the worker writes, ask their permission hook
// under the names the policy expects, and send the turn's token. It needs
//
//	TASKTO_CLI_REAL_TOOLS=1
//
// and the CLIs on the PATH, and is skipped otherwise. Grok Build can't run against a stand-in
// without its own sign-in.
func TestRealCLIsCallTaskToTools(t *testing.T) {
	if os.Getenv("TASKTO_CLI_REAL_TOOLS") != "1" {
		t.Skip("set TASKTO_CLI_REAL_TOOLS=1 to run the installed CLIs with TaskTo's tools against a scripted model")
	}
	bins := map[cliworker.CLI]string{}
	for _, cli := range []cliworker.CLI{cliworker.Claude, cliworker.Codex} {
		if path, err := exec.LookPath(string(cli)); err == nil {
			bins[cli] = path
		}
	}
	if len(bins) == 0 {
		t.Skip("neither claude nor codex is installed")
	}
	model := httptest.NewServer(scriptedModel{})
	t.Cleanup(model.Close)
	w, err := cliworker.New(cliworker.Config{
		Layout: cliworker.Layout{Root: filepath.Join(t.TempDir(), "cli")},
		Bins:   bins,
		Path:   os.Getenv("PATH"),
		Overrides: map[cliworker.CLI]cliworker.Overrides{
			cliworker.Claude: {Env: []string{
				"ANTHROPIC_BASE_URL=" + model.URL, "ANTHROPIC_AUTH_TOKEN=scripted", "ANTHROPIC_MODEL=scripted",
				"ANTHROPIC_SMALL_FAST_MODEL=scripted", "ANTHROPIC_DEFAULT_HAIKU_MODEL=scripted",
			}},
			cliworker.Codex: {
				Env: []string{"SCRIPTED_KEY=scripted"},
				CodexConfig: strings.Join([]string{
					`model = "scripted"`,
					`model_provider = "scripted"`,
					"[model_providers.scripted]",
					`name = "Scripted"`,
					"base_url = " + quote(model.URL+"/v1"),
					`env_key = "SCRIPTED_KEY"`,
					`wire_api = "responses"`,
				}, "\n"),
			},
		},
		TurnTimeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)

	for cli := range bins {
		t.Run(string(cli), func(t *testing.T) {
			// Each CLI's own endpoint: what it saw is this CLI's calls alone.
			stub := newToolsStub(t)
			// A read-only tool runs without a question.
			s := spec("real-tools-read-"+string(cli), 1, cli, "Please call list_spaces.")
			s.Tools = stub.tools(tokenA)
			events := runLong(t, w, s)
			if ps := permissionEvents(events); len(ps) != 0 {
				t.Errorf("a read-only tool asked: %+v", ps)
			}
			if !stub.saw("list_spaces Bearer " + tokenA) {
				t.Fatalf("the endpoint didn't get the call with the turn's token: %q; events %+v", stub.called(), events)
			}

			// A write asks the owner, under the name the policy reads, and runs once allowed.
			s = spec("real-tools-write-"+string(cli), 1, cli, "Please call write_doc.")
			s.Tools = stub.tools(tokenA)
			turn, _, err := w.Start(s)
			if err != nil {
				t.Fatal(err)
			}
			q := question(t, w, 1)
			if q.Tool != "mcp__taskto__write_doc" {
				t.Errorf("question = %+v", q)
			}
			if err := w.Answer(1, 7, q.ID, true); err != nil {
				t.Fatal(err)
			}
			events = waitDone(t, turn, 3*time.Minute)
			if done := last(events); done.Status != cliworker.StatusOK {
				t.Fatalf("turn: %+v", done)
			}
			if !stub.saw("write_doc Bearer " + tokenA) {
				t.Errorf("the allowed write didn't reach the endpoint: %q; events %+v", stub.called(), events)
			}
		})
	}
}

func (s *toolsStub) saw(call string) bool {
	for _, c := range s.called() {
		if c == call {
			return true
		}
	}
	return false
}

// scriptedModel answers Anthropic's Messages API and OpenAI's Responses API, streaming: the
// first answer of a turn calls the TaskTo tool its prompt names ("Please call <tool>."), and
// the next, once the tool's result is in the request, says "done".
type scriptedModel struct{}

var pleaseCall = regexp.MustCompile(`Please call ([a-z_]+)\.`)

func (scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tool := ""
	if m := pleaseCall.FindSubmatch(body); m != nil {
		tool = string(m[1])
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		w.(http.Flusher).Flush()
	}
	switch r.URL.Path {
	case "/v1/messages":
		var req struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		call := tool != "" && len(req.Tools) > 0 && !strings.Contains(string(body), `"tool_result"`)
		sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "scripted", "content": []any{},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}})
		stop := "end_turn"
		if call {
			stop = "tool_use"
			sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "mcp__taskto__" + tool, "input": map[string]any{}}})
			sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"notes.md"}`}})
		} else {
			sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "done"}})
		}
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 1}})
		sse("message_stop", map[string]any{"type": "message_stop"})
	case "/v1/responses":
		// Codex offers an MCP server's tools as a namespace, mcp__<server>.
		call := tool != "" && !strings.Contains(string(body), "function_call_output")
		item := map[string]any{"type": "message", "role": "assistant", "id": "msg_1", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
		if call {
			item = map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "namespace": "mcp__taskto", "name": tool, "arguments": `{"path":"notes.md"}`}
		}
		sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}})
		sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "usage": map[string]any{
			"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
			"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}}})
	default:
		http.NotFound(w, r)
	}
}
