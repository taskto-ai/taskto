//go:build unix

package cliworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// taskto's tools in a turn (docs/cli-tools.md): the worker writes each CLI's MCP settings for
// the turn's endpoint and token, and the fakes call a tool the way each real CLI does, asking
// their permission hook first.

// toolsStub is a stand-in for the server's MCP endpoint for turns: list_spaces (read-only) and
// write_doc, each answering which token called it.
type toolsStub struct {
	url   string
	mu    sync.Mutex
	calls []string // "<tool> <Authorization>"
}

func newToolsStub(t *testing.T) *toolsStub {
	t.Helper()
	stub := &toolsStub{}
	s := mcp.NewServer(&mcp.Implementation{Name: "taskto", Version: "test"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`)
	for _, name := range []string{"list_spaces", "write_doc"} {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: name == "list_spaces"}},
			func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				auth, _ := ctx.Value(authKey{}).(string)
				stub.mu.Lock()
				stub.calls = append(stub.calls, name+" "+auth)
				stub.mu.Unlock()
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " for " + auth}}}, nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cli-mcp" {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, r.Header.Get("Authorization"))))
	}))
	t.Cleanup(srv.Close)
	stub.url = srv.URL + "/cli-mcp"
	return stub
}

type authKey struct{}

func (s *toolsStub) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *toolsStub) tools(token string) *cliworker.Tools {
	return &cliworker.Tools{URL: s.url, Token: token, ReadOnly: []string{"list_spaces", "read_doc"}}
}

// mcpOutcome is what the fake reported for its call of tool.
func mcpOutcome(answer, tool string) string {
	for _, line := range strings.Split(answer, "\n") {
		if rest, ok := strings.CutPrefix(line, "mcp "+tool+": "); ok {
			return rest
		}
	}
	return "(no call)"
}

const (
	tokenA = "ttm1.token-of-user-one.signature" //nolint:gosec // G101: made up for the test
	tokenB = "ttm1.token-of-user-two.signature" //nolint:gosec // G101: made up for the test
)

// Each CLI gets the endpoint and the token from settings written for the run, with the token
// only in its environment, and a run without tools leaves none behind.
func TestTurnWithTaskToTools(t *testing.T) {
	h := newHarness(t, nil)
	stub := newToolsStub(t)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			dirs := h.user(t, 1, cli, clitest.Scenario{MCP: []clitest.MCPCall{{Tool: "list_spaces"}}}, true)
			config := dirs.Config(cli)
			s := spec("tools-"+string(cli), 1, cli, "list my spaces")
			s.Tools = stub.tools(tokenA)
			done := last(run(t, h.w, s))
			if done.Status != cliworker.StatusOK {
				t.Fatalf("done = %+v", done)
			}
			if got, want := mcpOutcome(done.Text, "list_spaces"), "ok list_spaces for Bearer "+tokenA; got != want {
				t.Errorf("call: %s, want %s", got, want)
			}
			calls := clitest.Calls(t, config)
			call := calls[len(calls)-1]
			if !slices.Contains(call.Env, "TASKTO_TOOLS_TOKEN="+tokenA) {
				t.Errorf("env lacks the token: %v", call.Env)
			}
			settings := map[cliworker.CLI]string{cliworker.Claude: "taskto-mcp.json", cliworker.Codex: "config.toml", cliworker.Grok: "config.toml"}[cli]
			b, err := os.ReadFile(filepath.Join(config, settings))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), tokenA) {
				t.Errorf("the token is written into %s", settings)
			}
			if !strings.Contains(string(b), stub.url) {
				t.Errorf("%s lacks the endpoint:\n%s", settings, b)
			}
			switch cli {
			case cliworker.Claude:
				if !slices.Contains(call.Args, "--strict-mcp-config") || !slices.Contains(call.Args, filepath.Join(config, "taskto-mcp.json")) {
					t.Errorf("args = %q", call.Args)
				}
			case cliworker.Codex:
				for _, line := range []string{"[mcp_servers.taskto]", `bearer_token_env_var = "TASKTO_TOOLS_TOKEN"`, `default_tools_approval_mode = "approve"`} {
					if !strings.Contains(string(b), line) {
						t.Errorf("config.toml lacks %s:\n%s", line, b)
					}
				}
			case cliworker.Grok:
				if !strings.Contains(string(b), `headers = { Authorization = "Bearer ${TASKTO_TOOLS_TOKEN}" }`) {
					t.Errorf("config.toml lacks the header:\n%s", b)
				}
			}

			// The next run has no tools: nothing of the last one's is left.
			s = spec("no-tools-"+string(cli), 1, cli, "list my spaces")
			done = last(run(t, h.w, s))
			if got := mcpOutcome(done.Text, "list_spaces"); !strings.HasPrefix(got, "error ") {
				t.Errorf("a run without tools called one: %s", got)
			}
			calls = clitest.Calls(t, config)
			call = calls[len(calls)-1]
			for _, v := range call.Env {
				if strings.HasPrefix(v, "TASKTO_TOOLS_TOKEN=") {
					t.Errorf("env has %s", v)
				}
			}
			if cli == cliworker.Claude {
				if _, err := os.Stat(filepath.Join(config, "taskto-mcp.json")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Claude Code's MCP settings outlived their run: %v", err)
				}
				if !slices.Contains(call.Args, "--strict-mcp-config") || slices.Contains(call.Args, "--mcp-config") {
					t.Errorf("args = %q", call.Args)
				}
			} else if b, _ := os.ReadFile(filepath.Join(config, "config.toml")); strings.Contains(string(b), "mcp_servers") {
				t.Errorf("config.toml kept the tools:\n%s", b)
			}
		})
	}
}

// In ask, taskto's read-only tools don't ask and the others ask the owner; allow_all asks
// nothing. A denied call never reaches the endpoint.
func TestTaskToToolsPermissions(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			stub := newToolsStub(t)
			write := []clitest.MCPCall{{Tool: "write_doc", Args: map[string]any{"path": "notes.md"}}}
			for _, allow := range []bool{true, false} {
				h.user(t, 1, cli, clitest.Scenario{MCP: write}, true)
				id := "tools-ask-" + string(cli) + map[bool]string{true: "-yes", false: "-no"}[allow]
				s := spec(id, 1, cli, "write it")
				s.Tools = stub.tools(tokenA)
				turn, _, err := h.w.Start(s)
				if err != nil {
					t.Fatal(err)
				}
				q := question(t, h.w, 1)
				wantTool := map[cliworker.CLI]string{cliworker.Claude: "mcp__taskto__write_doc", cliworker.Codex: "mcp__taskto__write_doc", cliworker.Grok: "use_tool"}[cli]
				if q.Tool != wantTool || !strings.Contains(q.Input, "notes.md") {
					t.Errorf("question = %+v", q)
				}
				if err := h.w.Answer(1, 7, q.ID, allow); err != nil {
					t.Fatal(err)
				}
				done := last(waitDone(t, turn, 20*time.Second))
				got := mcpOutcome(done.Text, "write_doc")
				if allow && got != "ok write_doc for Bearer "+tokenA {
					t.Errorf("allowed: %s", got)
				}
				if !allow && !strings.HasPrefix(got, "denied: ") {
					t.Errorf("denied: %s", got)
				}
			}
			if n := len(stub.called()); n != 1 {
				t.Errorf("the endpoint saw %d calls, want the allowed one: %q", n, stub.called())
			}

			// A read-only tool, and with allow_all a write, run without a question.
			for _, c := range []struct {
				tool  string
				perms cliworker.Permissions
			}{{"list_spaces", cliworker.Ask}, {"write_doc", cliworker.AllowAll}} {
				h.user(t, 1, cli, clitest.Scenario{MCP: []clitest.MCPCall{{Tool: c.tool}}}, true)
				s := spec("tools-free-"+string(cli)+"-"+c.tool, 1, cli, "go")
				s.Tools, s.Permissions = stub.tools(tokenA), c.perms
				events := run(t, h.w, s)
				if ps := permissionEvents(events); len(ps) != 0 {
					t.Errorf("%s asked: %+v", c.tool, ps)
				}
				if got := mcpOutcome(last(events).Text, c.tool); !strings.HasPrefix(got, "ok ") {
					t.Errorf("%s: %s", c.tool, got)
				}
			}
		})
	}
}

// Two users' turns at once each get their own token, in their own environment, and nothing
// of the other's.
func TestTaskToToolsPerUser(t *testing.T) {
	h := newHarness(t, nil)
	stub := newToolsStub(t)
	read := clitest.Scenario{MCP: []clitest.MCPCall{{Tool: "list_spaces"}}, SleepMS: 300}
	var turns []*cliworker.Turn
	for user, token := range map[int64]string{1: tokenA, 2: tokenB} {
		h.user(t, user, cliworker.Claude, read, true)
		s := spec("tools-user", user, cliworker.Claude, "list")
		s.ID = "tools-user-" + token[len("ttm1.token-of-user-"):len("ttm1.token-of-user-")+3]
		s.Tools = stub.tools(token)
		turn, _, err := h.w.Start(s)
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	for _, turn := range turns {
		done := last(waitDone(t, turn, 20*time.Second))
		token := map[int64]string{1: tokenA, 2: tokenB}[turn.Spec().User]
		other := map[int64]string{1: tokenB, 2: tokenA}[turn.Spec().User]
		if got := mcpOutcome(done.Text, "list_spaces"); got != "ok list_spaces for Bearer "+token {
			t.Errorf("user %d: %s", turn.Spec().User, got)
		}
		config := h.layout.User(turn.Spec().User).Config(cliworker.Claude)
		calls := clitest.Calls(t, config)
		env := strings.Join(calls[len(calls)-1].Env, "\n")
		if !strings.Contains(env, token) || strings.Contains(env, other) {
			t.Errorf("user %d's environment: %s", turn.Spec().User, env)
		}
	}
}

// The tools' URL and token are checked like the proxy's, and the read-only names too.
func TestTaskToToolsChecked(t *testing.T) {
	h := newHarness(t, nil)
	for name, tools := range map[string]*cliworker.Tools{
		"credentials in the URL": {URL: "http://user:pw@127.0.0.1:8081/cli-mcp", Token: tokenA}, //nolint:gosec // G101: made up, to be refused
		"another scheme":         {URL: "file:///etc/passwd", Token: tokenA},
		"a query":                {URL: "http://127.0.0.1:8081/cli-mcp?x=1", Token: tokenA},
		"a short token":          {URL: "http://127.0.0.1:8081/cli-mcp", Token: "short"},
		"a token with a space":   {URL: "http://127.0.0.1:8081/cli-mcp", Token: "ttm1.has a space.signature"}, //nolint:gosec // G101: made up, to be refused
		"a bad tool name":        {URL: "http://127.0.0.1:8081/cli-mcp", Token: tokenA, ReadOnly: []string{"Bash"}},
	} {
		s := spec("tools-bad", 1, cliworker.Claude, "hello")
		s.Tools = tools
		if _, _, err := h.w.Start(s); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

// The worker's API never hands a turn's tools token back.
func TestRemoteHidesTheToolsToken(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1, 2)
	r := serve(t, h)
	s := spec("remote-tools", 1, cliworker.Grok, "hello")
	s.Tools = &cliworker.Tools{URL: "http://127.0.0.1:8081/cli-mcp", Token: tokenA, ReadOnly: []string{"list_spaces"}}
	if _, _, err := r.StartTurn(s); err != nil {
		t.Fatal(err)
	}
	found, ok := r.FindTurn(s.ID)
	if !ok {
		t.Fatal("turn not found")
	}
	if tools := found.Spec().Tools; tools == nil || tools.Token != "" || tools.URL != s.Tools.URL {
		t.Fatalf("tools = %+v", tools)
	}
}

// taskto's read-only tools are recognised in each CLI's naming, and nothing else is.
func TestTaskToToolsReadOnlyNames(t *testing.T) {
	tools := &cliworker.Tools{URL: "http://127.0.0.1:8081/cli-mcp", Token: tokenA, ReadOnly: []string{"list_spaces", "read_doc"}}
	cases := []struct {
		cli         cliworker.CLI
		tool, input string
		want        bool
	}{
		{cliworker.Claude, "mcp__taskto__read_doc", `{"space_id":1,"path":"a.md"}`, true},
		{cliworker.Codex, "mcp__taskto__list_spaces", `{}`, true},
		{cliworker.Grok, "taskto__list_spaces", `{}`, true},
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__read_doc","tool_input":{"space_id":1,"path":"a.md"}}`, true},
		{cliworker.Grok, "search_tool", `{"query":"docs"}`, true},
		{cliworker.Claude, "mcp__taskto__write_doc", `{"space_id":1,"path":"a.md","content":"x"}`, false},
		{cliworker.Grok, "taskto__write_doc", `{}`, false},
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__write_doc","tool_input":{}}`, false},
		// Each CLI's own form only: another CLI's form, or Grok Build's tools, from it ask.
		{cliworker.Claude, "taskto__read_doc", `{}`, false},
		{cliworker.Codex, "use_tool", `{"tool_name":"taskto__read_doc","tool_input":{}}`, false},
		{cliworker.Claude, "search_tool", `{}`, false},
		{cliworker.Codex, "search_tool", `{}`, false},
		{cliworker.Grok, "mcp__taskto__read_doc", `{}`, false},
		// The file forms read a file first: they ask.
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__read_doc","tool_input_file":"/tmp/args.json"}`, false},
		{cliworker.Grok, "use_tool", `{"file":"/tmp/call.json"}`, false},
		// Without its input inline, or with one that isn't an object.
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__read_doc"}`, false},
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__read_doc","tool_input":"a.md"}`, false},
		{cliworker.Grok, "use_tool", `{"tool_name":"taskto__read_doc","tool_input":null}`, false},
		// Another server's tool of the same name, or a look-alike.
		{cliworker.Claude, "mcp__other__read_doc", `{}`, false},
		{cliworker.Grok, "use_tool", `{"tool_name":"other__read_doc","tool_input":{}}`, false},
		{cliworker.Claude, "mcp__taskto__read_doc_and_more", `{}`, false},
		{cliworker.Claude, "read_doc", `{}`, false},
		{cliworker.Codex, "Bash", `{"command":"ls"}`, false},
	}
	for _, c := range cases {
		if got := cliworker.ToolsReadOnlyForTest(tools, c.cli, c.tool, []byte(c.input)); got != c.want {
			t.Errorf("%s %s %s: %v, want %v", c.cli, c.tool, c.input, got, c.want)
		}
	}
	// A turn without taskto's tools has no read-only ones.
	if cliworker.ToolsReadOnlyForTest(nil, cliworker.Claude, "mcp__taskto__read_doc", []byte(`{}`)) || cliworker.ToolsReadOnlyForTest(nil, cliworker.Grok, "search_tool", []byte(`{}`)) {
		t.Error("a turn without tools let one through")
	}

}
