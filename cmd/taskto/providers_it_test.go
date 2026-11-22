//go:build unix

package main

// A provider account added through the API reaches the running agent runtime, without
// restarting it or the server. The model behind it is a fake OpenAI-compatible server on
// loopback (as in internal/agentrt/providers_test.go), so nothing reaches a real provider.
// Gated by TASKTO_AGENT_IT=1 like the other integration tests.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

// Obviously fake keys: the fake server takes either, and reports which one it got.
const (
	firstKey   = "fake-first-key-for-tests-not-real"
	rotatedKey = "fake-rotated-key-for-tests-not-real"
)

// fakeModel answers every chat completion with "<key name> says hi", streamed.
type fakeModel struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string
}

func newFakeModel(t *testing.T) *fakeModel {
	f := &fakeModel{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.keys = append(f.keys, key)
		f.mu.Unlock()
		said := map[string]string{firstKey: "first", rotatedKey: "rotated"}[key]
		if said == "" {
			http.Error(w, `{"error":{"message":"unknown key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []map[string]any{{"role": "assistant", "content": ""}, {"content": said + " says hi"}} {
			chunk, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 0, "model": "fake",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(f.Close)
	return f
}

// api is a logged-in client of the server's API.
type api struct {
	t    *testing.T
	base string
	http *http.Client
}

func (a *api) do(method, path string, body any) (int, []byte) {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, r)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	return res.StatusCode, out
}

func (a *api) expect(status int, method, path string, body any) []byte {
	a.t.Helper()
	got, out := a.do(method, path, body)
	if got != status {
		a.t.Fatalf("%s %s = %d %s, want %d", method, path, got, out, status)
	}
	return out
}

func TestProviderAccountReachesTheAgentRuntime(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDataDir(t)
	bin := filepath.Join(dir, "taskto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dataDir := filepath.Join(dir, "data")
	// insecure-cookies: the test talks plain http to the server on loopback.
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry, "-insecure-cookies")
	_, socketDir, _ := agentPaths(dataDir)
	sidecar := agentrt.New(filepath.Join(socketDir, "agent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	before, err := sidecar.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := &api{t: t, base: "http://" + addr, http: &http.Client{Jar: jar}}
	var me struct {
		Workspaces []struct {
			ID int64 `json:"id"`
		} `json:"workspaces"`
	}
	// The setup token, from the server's log, as an operator reads it.
	token := setupTokenLine.FindStringSubmatch(server.output.String())
	if token == nil {
		t.Fatal("the server logged no setup token")
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &me); err != nil {
		t.Fatal(err)
	}
	ws := me.Workspaces[0].ID

	model := newFakeModel(t)
	var created struct {
		ID       int64  `json:"id"`
		Provider string `json:"provider"`
	}
	out := owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/providers", ws), map[string]any{
		"name": "fake", "display_name": "Fake", "base_url": model.URL + "/v1", "api_key": firstKey,
		"models": []map[string]any{{"id": "fake-small"}},
	})
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}

	// The sidecar has it now, without a restart.
	catalog, err := sidecar.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Providers) != 2 || catalog.Providers[0].ID != "script" || catalog.Providers[1].ID != created.Provider ||
		catalog.Providers[1].Name != "Fake" || catalog.Providers[1].Models[0].ID != "fake-small" {
		t.Fatalf("the sidecar's models = %+v", catalog)
	}
	if h, err := sidecar.Health(ctx); err != nil || h.PID != before.PID {
		t.Fatalf("sidecar after adding the account: %+v, %v; want the same process %d", h, err, before.PID)
	}

	// And a conversation runs on it, with the key from its key file.
	ask := func(conv int64, content, want string) {
		t.Helper()
		sub, err := sidecar.Submit(ctx, conv, agentrt.Input{Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := sidecar.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("wait = %+v, %v; want done", settled, err)
		}
		msgs, err := sidecar.Messages(ctx, conv)
		if err != nil || len(msgs) == 0 || msgs[len(msgs)-1].Text != want {
			t.Fatalf("messages = %+v, %v; want the last to be %q", msgs, err, want)
		}
	}
	conv, err := sidecar.CreateConversationOn(ctx, agentrt.ModelRef{Provider: created.Provider, ModelID: "fake-small"})
	if err != nil {
		t.Fatal(err)
	}
	ask(conv, "hello", "first says hi")

	// A new key takes effect from the next request.
	one := fmt.Sprintf("/api/workspaces/%d/providers/%d", ws, created.ID)
	owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"api_key": rotatedKey})
	ask(conv, "again", "rotated says hi")

	// Deleted, it leaves the sidecar, which is still the same process.
	owner.expect(http.StatusNoContent, http.MethodDelete, one, nil)
	if catalog, err = sidecar.Models(ctx); err != nil || len(catalog.Providers) != 1 {
		t.Fatalf("the sidecar's models after the delete = %+v, %v", catalog, err)
	}
	if h, err := sidecar.Health(ctx); err != nil || h.PID != before.PID {
		t.Fatalf("sidecar after the changes: %+v, %v; want the same process %d", h, err, before.PID)
	}

	if err := server.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = server.cmd.Wait()
	log := server.output.String()
	for _, key := range []string{firstKey, rotatedKey} {
		if strings.Contains(log, key) {
			t.Errorf("the server's output contains the key %q", key)
		}
	}
	if !strings.Contains(log, "provider account created") {
		t.Error("the server's output doesn't have the account's creation; was it captured?")
	}
}
