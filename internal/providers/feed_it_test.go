//go:build unix

package providers

// The feed with the real agent runtime (agent/dist). Like internal/agentrt's integration tests
// it needs Node and a built agent package, so it only runs with TASKTO_AGENT_IT=1
// (make agent-it). No request reaches a model: these tests only look at GET /models and the
// scripted model.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/egress"
)

// startAgent runs the agent runtime on the feed's providers file, and returns its client.
func startAgent(t *testing.T, e *env, feed *Feed) *agentrt.Client {
	t.Helper()
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/test-entry.js") // with the pause tool the test's model calls
	if err != nil {
		t.Fatal(err)
	}
	// A short directory: Unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "taskto-feed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	if err := feed.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", entry)
	cmd.Env = append(os.Environ(),
		"TASKTO_AGENT_SOCKET="+socket,
		"TASKTO_AGENT_DB="+filepath.Join(dir, "agent.db"),
		"TASKTO_AGENT_PROVIDERS="+feed.Path(),
	)
	cmd.Stderr = e.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("output:\n%s", e.logs.String())
		}
	})
	client := agentrt.New(socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := client.Healthy(ctx)
		cancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent runtime not healthy: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func reloadVia(client *agentrt.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := client.ReloadProviders(ctx)
		if se := (*agentrt.StatusError)(nil); errors.As(err, &se) {
			return fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return err
	}
}

func providerIDs(t *testing.T, client *agentrt.Client) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	catalog, err := client.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range catalog.Providers {
		ids = append(ids, p.ID)
	}
	return ids
}

// Accounts reach the running agent runtime on a reload, a run in progress goes on through it,
// and a file the agent runtime refuses leaves its providers as they were.
func TestFeedReloadsTheAgentRuntime(t *testing.T) {
	e := newEnv(t)
	client := startAgent(t, e, e.feed)
	e.feed.reload = reloadVia(client)
	owner, ws := e.setup()
	if got := providerIDs(t, client); !slices.Equal(got, []string{"script"}) {
		t.Fatalf("providers at start = %v", got)
	}

	// A run in progress: the scripted model calls a tool that takes a while.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conv, err := client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Submit(ctx, conv, agentrt.Input{Content: "tool:pause_safe:1500"})
	if err != nil {
		t.Fatal(err)
	}

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("gateway", remoteURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	rec = owner.do(http.MethodPost, "/api/me/providers", account("local", localURL, nil))
	expect(t, rec, http.StatusCreated)
	local := decode[Account](t, rec)
	want := []string{"script", fmt.Sprintf("ws%d-gateway", ws), fmt.Sprintf("u%d-local", owner.id)}
	if got := providerIDs(t, client); !slices.Equal(got, want) {
		t.Fatalf("providers after adding two = %v, want %v", got, want)
	}

	if settled, err := client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the run across the reloads: %+v, %v; want done", settled, err)
	}

	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/me/providers/%d", local.ID), nil), http.StatusNoContent)
	want = want[:2]
	if got := providerIDs(t, client); !slices.Equal(got, want) {
		t.Fatalf("providers after deleting one = %v, want %v", got, want)
	}

	// A file the agent runtime refuses: it answers 400 and keeps what it had.
	if err := writeFile(e.feed.Path(), []byte(`{"providers":[{"id":"x","kind":"magic"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.feed.reload(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("reload of a bad file = %v, want ErrRefused", err)
	}
	if got := providerIDs(t, client); !slices.Equal(got, want) {
		t.Fatalf("providers after a refused reload = %v, want %v", got, want)
	}
	if strings.Contains(e.logs.String(), testKey) {
		t.Fatal("the output contains the key")
	}
}

// Every compat setting the API accepts is one the agent runtime accepts too. This catches a
// pi-ai upgrade that drops or changes one.
func TestAgentRuntimeAcceptsEveryCompatSetting(t *testing.T) {
	e := newEnv(t)
	client := startAgent(t, e, e.feed)
	e.feed.reload = reloadVia(client)
	owner, _ := e.setup()

	// Each setting in turn with every value it may take, as few accounts as that needs.
	var bodies []map[string]any
	add := func(name string, value any) {
		for _, b := range bodies {
			if _, used := b[name]; !used {
				b[name] = value
				return
			}
		}
		bodies = append(bodies, map[string]any{name: value})
	}
	for name, values := range compatSettings {
		switch {
		case values == nil:
			add(name, true)
			add(name, false)
		case len(values) == 1 && values[0] == "number":
			add(name, 5)
		default:
			for _, v := range values {
				add(name, v)
			}
		}
	}
	for i, compat := range bodies {
		body := account(fmt.Sprintf("compat-%d", i), localURL, nil)
		body["compat"] = compat
		expect(t, owner.do(http.MethodPost, "/api/me/providers", body), http.StatusCreated)
	}
	got := providerIDs(t, client)
	if len(got) != 1+len(bodies) {
		t.Fatalf("the agent runtime has %v; want script and %d accounts. Its output:\n%s", got, len(bodies), e.logs.String())
	}
}

// The agent runtime accepts every egress policy the server writes for the accounts.
func TestFeedEgressPolicies(t *testing.T) {
	e := newEnv(t)
	e.setEgress(Egress{
		Workspace: egress.MustParse("public, ollama.lan, 10.0.0.0/8, fd00::/8, 192.0.2.7, ::ffff:198.51.100.0/120"),
		Personal:  egress.MustParse("public"),
	})
	e.api.resolve = fakeDNS(map[string]string{"llm.example.com": "8.8.8.8"})
	client := startAgent(t, e, e.feed)
	e.feed.reload = reloadVia(client)
	owner, ws := e.setup()

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("team", remoteURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	team := decode[Account](t, rec)
	rec = owner.do(http.MethodPost, "/api/me/providers", account("mine", remoteURL, nil))
	expect(t, rec, http.StatusCreated)
	mine := decode[Account](t, rec)
	if got := providerIDs(t, client); !slices.Equal(got, []string{"script", team.Provider, mine.Provider}) {
		t.Fatalf("providers = %v; the agent runtime refused the policies?\n%s", got, e.logs.String())
	}
}
