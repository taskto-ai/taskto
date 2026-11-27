package agentrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	fileKey = "fake-file-key-for-tests-not-real"
	envKey  = "fake-env-key-for-tests-not-real"
	keyEnv  = "TASKTO_TEST_FAKE_PROVIDER_KEY"
)

type fakeOpenAI struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOpenAI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	if key != fileKey {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"message": "Incorrect API key provided: " + key,
			"type":    "invalid_request_error",
			"code":    "invalid_api_key",
		}})
		return
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	heard := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			heard = contentText(m.Content)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finish any) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 0, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}))
		w.(http.Flusher).Flush()
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	for _, part := range strings.SplitAfter(req.Model+" heard: "+heard, " ") {
		chunk(map[string]any{"content": part}, nil)
	}
	chunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (f *fakeOpenAI) sawKey(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k == want {
			return true
		}
	}
	return false
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeProviders(t *testing.T, server *fakeOpenAI) []string {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "fake.key")
	if err := os.WriteFile(keyFile, []byte(fileKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"providers": []any{
		map[string]any{
			"id": "fake", "kind": "openai-compatible", "name": "Fake",
			"baseUrl": server.URL + "/v1",
			"apiKey":  map[string]string{"file": keyFile},
			"models":  []any{map[string]any{"id": "fake-small"}, map[string]any{"id": "fake-large", "name": "Fake Large"}},
		},
		map[string]any{
			"id": "wrong-key", "kind": "openai-compatible",
			"baseUrl": server.URL + "/v1",
			"apiKey":  map[string]string{"env": keyEnv},
			"models":  []any{map[string]any{"id": "fake-small"}},
		},
		map[string]any{"id": "script", "kind": "script"},
	}}
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"TASKTO_AGENT_PROVIDERS=" + path, keyEnv + "=" + envKey}
}

func ask(t *testing.T, c *Client, conv int64, content string) Settled {
	t.Helper()
	ctx := ctxFor(t)
	sub, err := c.Submit(ctx, conv, Input{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := c.Wait(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	return settled
}

func TestOpenAICompatibleProvider(t *testing.T) {
	server := newFakeOpenAI(t)
	s := startSidecar(t, writeProviders(t, server)...)
	ctx := ctxFor(t)

	catalog, err := s.client.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := ModelRef{Provider: "fake", ModelID: "fake-small"}
	if catalog.Default != want || len(catalog.Providers) != 3 || catalog.Providers[0].Kind != "openai-compatible" ||
		len(catalog.Providers[0].Models) != 2 || catalog.Providers[0].Models[1].Name != "Fake Large" ||
		catalog.Providers[2].Kind != "script" {
		t.Fatalf("models = %+v", catalog)
	}

	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got != want {
		t.Fatalf("model = %+v, %v; want %+v", got, err, want)
	}
	if settled := ask(t, s.client, conv, "hello there"); settled.Status != "done" {
		t.Fatalf("wait = %+v; want done", settled)
	}
	if got := lastAssistant(t, s.client, conv); got != "fake-small heard: hello there" {
		t.Fatalf("answer = %q", got)
	}
	if !server.sawKey(fileKey) {
		t.Fatal("the fake server never received the key from the key file")
	}

	large := ModelRef{Provider: "fake", ModelID: "fake-large"}
	if err := s.client.SetModel(ctx, conv, large); err != nil {
		t.Fatal(err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got != large {
		t.Fatalf("model after switch = %+v, %v; want %+v", got, err, large)
	}
	ask(t, s.client, conv, "and now?")
	if got := lastAssistant(t, s.client, conv); got != "fake-large heard: and now?" {
		t.Fatalf("answer after switch = %q", got)
	}
	if err := s.client.SetModel(ctx, conv, ModelRef{Provider: "script", ModelID: "script-1"}); err != nil {
		t.Fatal(err)
	}
	ask(t, s.client, conv, "offline")
	if got := lastAssistant(t, s.client, conv); got != "echo: offline" {
		t.Fatalf("answer from the scripted model = %q", got)
	}

	chosen, err := s.client.CreateConversationOn(ctx, large)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.client.Model(ctx, chosen); err != nil || got != large {
		t.Fatalf("model of a conversation created on %+v = %+v, %v", large, got, err)
	}

	for _, bad := range []ModelRef{
		{Provider: "fake", ModelID: "nope"},
		{Provider: "nope", ModelID: "fake-small"},
		{Provider: "openai", ModelID: "gpt-4o"},
		{Provider: "fake"},
	} {
		if _, err := s.client.CreateConversationOn(ctx, bad); statusCode(err) != http.StatusBadRequest {
			t.Errorf("create on %+v: err = %v, want a 400", bad, err)
		}
		err := s.client.SetModel(ctx, conv, bad)
		if statusCode(err) != http.StatusBadRequest {
			t.Errorf("switch to %+v: err = %v, want a 400", bad, err)
		}
		if se := (*StatusError)(nil); errors.As(err, &se) && se.Reason != "unknown_model" {
			t.Errorf("switch to %+v: reason = %q, want unknown_model", bad, se.Reason)
		}
	}
	if err := s.client.SetModel(ctx, 999_999, large); statusCode(err) != http.StatusNotFound {
		t.Errorf("switch of an unknown conversation: err = %v, want a 404", err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.Provider != "script" {
		t.Fatalf("model after refused switches = %+v, %v; want it unchanged", got, err)
	}
}

func statusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

type recorder struct {
	next http.RoundTripper
	out  *lockedBuffer
}

func (r recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	res.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(res.Body, r.out), res.Body}
	return res, nil
}

func TestAPIKeysNeverLeave(t *testing.T) {
	server := newFakeOpenAI(t)
	s := startSidecar(t, writeProviders(t, server)...)
	var responses lockedBuffer
	s.client.http.Transport = recorder{next: s.client.http.Transport, out: &responses}
	ctx := ctxFor(t)

	if _, err := s.client.Models(ctx); err != nil {
		t.Fatal(err)
	}
	good, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s.client, good, "hi")
	lastAssistant(t, s.client, good)

	bad, err := s.client.CreateConversationOn(ctx, ModelRef{Provider: "wrong-key", ModelID: "fake-small"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.client.Watch(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	settled := ask(t, s.client, bad, "hi")
	if settled.Status != "unanswered" {
		t.Fatalf("wait with a wrong key = %+v; want unanswered", settled)
	}
	if !server.sawKey(envKey) {
		t.Fatal("the fake server never received the env key, so the test proves nothing")
	}
	waitForOps(t, w, "Incorrect API key provided")
	if _, err := s.client.Messages(ctx, bad); err != nil {
		t.Fatal(err)
	}
	t.Logf("wait with a wrong key: %+v", settled)

	s.stop()
	for name, text := range map[string]string{"a response": responses.String(), "the sidecar's output": s.output.String()} {
		for _, key := range []string{fileKey, envKey} {
			if strings.Contains(text, key) {
				t.Errorf("%s contains the key %q", name, key)
			}
		}
	}
	if !strings.Contains(responses.String(), "Incorrect API key provided: [redacted]") {
		t.Error("the upstream's error didn't come back redacted; the redaction path went untested")
	}
}

func TestBadProvidersConfig(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	const inline = "fake-inline-key-for-tests-not-real"
	provider := func(extra string) string {
		return `{"providers":[{"id":"p","kind":"openai-compatible","baseUrl":"http://127.0.0.1:1/v1",` +
			`"models":[{"id":"m"}]` + extra + `}]}`
	}
	cases := map[string]struct{ config, want string }{
		"inline key":           {provider(`,"apiKey":"` + inline + `"`), "must name a file or env var"},
		"key in a field":       {provider(`,"key":"` + inline + `"`), `unknown field "key"`},
		"key in the URL":       {strings.Replace(provider(""), "http://", "http://user:"+inline+"@", 1), "must not contain credentials"},
		"missing key file":     {provider(`,"apiKey":{"file":"/nonexistent/fake.key"}`), "cannot read file"},
		"unset key env var":    {provider(`,"apiKey":{"env":"TASKTO_TEST_UNSET_KEY"}`), "is empty or unset"},
		"unknown kind":         {`{"providers":[{"id":"p","kind":"magic"}]}`, "kind must be"},
		"unknown CLI":          {`{"providers":[{"id":"cli-x","kind":"cli","cli":"bash","models":[{"id":"default"}]}]}`, ".cli must be one of"},
		"CLI with a key":       {`{"providers":[{"id":"cli-claude","kind":"cli","cli":"claude","apiKey":{"env":"PATH"},"models":[{"id":"default"}]}]}`, `unknown field "apiKey"`},
		"compat typo":          {provider(`,"compat":{"supportsDeveloperrole":true}`), `unknown field "supportsDeveloperrole"`},
		"key over remote http": {strings.Replace(provider(`,"apiKey":{"env":"PATH"}`), "http://127.0.0.1:1", "http://models.example.invalid", 1), "must use https"},
		"compat wrong type":    {provider(`,"compat":{"supportsStore":"yes"}`), ".compat"},
		"not JSON":             {`{"providers":` + inline, "not valid JSON"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "providers.json")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("node", entry)
			cmd.Env = append(os.Environ(),
				"TASKTO_AGENT_SOCKET="+filepath.Join(dir, "agent.sock"),
				"TASKTO_AGENT_DB="+filepath.Join(dir, "agent.db"),
				"TASKTO_AGENT_PROVIDERS="+path,
			)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("err = %v, output %q; want exit 2", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output %q, want it to say %q", out, tc.want)
			}
			if strings.Contains(string(out), inline) {
				t.Errorf("output quotes the key: %q", out)
			}
		})
	}
}

func TestKeyFileRemovedWhileRunning(t *testing.T) {
	server := newFakeOpenAI(t)
	env := writeProviders(t, server)
	s := startSidecar(t, env...)
	ctx := ctxFor(t)

	var config struct {
		Providers []struct {
			APIKey struct {
				File string `json:"file"`
			} `json:"apiKey"`
		} `json:"providers"`
	}
	raw, err := os.ReadFile(strings.TrimPrefix(env[0], "TASKTO_AGENT_PROVIDERS="))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	keyFile := config.Providers[0].APIKey.File

	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(t, s.client, conv, "hello"); got.Status != "done" {
		t.Fatalf("first answer = %+v, want done", got)
	}

	if err := os.Rename(keyFile, keyFile+".away"); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, s.client, conv, "again"); got.Status != "unanswered" {
		t.Fatalf("answer without a key file = %+v, want unanswered", got)
	}
	if err := s.client.Healthy(ctx); err != nil {
		t.Fatalf("sidecar after a missing key file: %v", err)
	}

	if err := os.Rename(keyFile+".away", keyFile); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, s.client, conv, "and now"); got.Status != "done" {
		t.Fatalf("answer after restoring the key file = %+v, want done", got)
	}
}

func TestModelRouteReturnsOnlyTheReference(t *testing.T) {
	server := newFakeOpenAI(t)
	s := startSidecar(t, writeProviders(t, server)...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/conversations/"+itoa(conv)+"/model", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["provider"] != "fake" || body["modelId"] != "fake-small" {
		t.Fatalf("model route = %v, want exactly provider and modelId", body)
	}
}
