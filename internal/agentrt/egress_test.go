package agentrt

// The agent runtime's egress policies (agent/src/egress.ts), against the fake OpenAI server on
// loopback, with DNS under the test's control: a preloaded module replaces dns.lookup for
// names under .test. Gated by TASKTO_AGENT_IT=1 like the other sidecar tests.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// refusedKey is sent by nothing: its provider's endpoint is refused before any connection.
const refusedKey = "fake-refused-key-for-tests-not-real"

// fakeDNS is a module Node preloads (NODE_OPTIONS=--import) that answers rebind.test with
// 127.0.0.1 to the first lookup and 127.0.0.2 to every later one, lan.test with 10.0.0.1, and
// azure.test with Azure's WireServer, which looks public.
const fakeDNS = `import dns from 'node:dns'
const real = dns.lookup
let lookups = 0
dns.lookup = (host, options, callback) => {
  if (typeof options === 'function') [options, callback] = [{}, options]
  let address
  if (host === 'rebind.test') address = lookups++ === 0 ? '127.0.0.1' : '127.0.0.2'
  else if (host === 'lan.test') address = '10.0.0.1'
  else if (host === 'azure.test') address = '168.63.129.16'
  else return real(host, options, callback)
  const answer = [{ address, family: 4 }]
  process.nextTick(() => (options.all ? callback(null, answer) : callback(null, address, 4)))
}
`

// newEgressOpenAI is the fake OpenAI server, closing every connection after its request, so
// every request makes a new connection, and so a new lookup. It accepts keyless requests too.
func newEgressOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer unused" { // pi-ai's placeholder without a key
			r.Header.Set("Authorization", "Bearer "+fileKey)
		}
		f.serve(w, r)
	}))
	f.Config.SetKeepAlivesEnabled(false)
	f.Start()
	t.Cleanup(f.Close)
	return f
}

func TestEgressPolicies(t *testing.T) {
	server := newEgressOpenAI(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	preload := write("fake-dns.mjs", fakeDNS)
	keyFile := write("refused.key", refusedKey)
	port := strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	provider := func(id, baseURL string, egress []string, extra map[string]any) map[string]any {
		p := map[string]any{"id": id, "kind": "openai-compatible", "baseUrl": baseURL, "models": []any{map[string]any{"id": "m"}}}
		if egress != nil {
			p["egress"] = egress
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	config := map[string]any{"providers": []any{
		map[string]any{"id": "script", "kind": "script"},
		// No policy: anywhere, this machine included.
		provider("open", server.URL+"/v1", nil, nil),
		// Public addresses only, and the server is on loopback: refused, key unsent.
		provider("public", server.URL+"/v1", []string{"public"}, map[string]any{"apiKey": map[string]string{"file": keyFile}}),
		// The same by name: the name resolves to a private address.
		provider("lan", "http://lan.test:"+port+"/v1", []string{"public"}, nil),
		// A cloud metadata service at a public-looking address.
		provider("azure", "http://azure.test:"+port+"/v1", []string{"public"}, nil),
		// An allowlist by address, and a name answering an allowed address first and a
		// loopback address it doesn't allow after: DNS rebinding.
		provider("rebind", "http://rebind.test:"+port+"/v1", []string{"public", "127.0.0.1"}, nil),
		// An allowlist by name: wherever it resolves to.
		provider("named", "http://localhost:"+port+"/v1", []string{"localhost"}, nil),
	}}
	s := startSidecar(t, "TASKTO_AGENT_PROVIDERS="+write("providers.json", mustJSON(config)),
		"NODE_OPTIONS=--import="+preload)
	ctx := ctxFor(t)

	answer := func(provider string) (Settled, int64) {
		t.Helper()
		conv, err := s.client.CreateConversationOn(ctx, ModelRef{Provider: provider, ModelID: "m"})
		if err != nil {
			t.Fatal(err)
		}
		return ask(t, s.client, conv, "hello from "+provider), conv
	}
	for _, provider := range []string{"open", "named"} {
		if settled, conv := answer(provider); settled.Status != "done" || lastAssistant(t, s.client, conv) != "m heard: hello from "+provider {
			t.Fatalf("%s: %+v; want an answer", provider, settled)
		}
	}

	refused := func(provider, want string) int64 {
		t.Helper()
		conv, err := s.client.CreateConversationOn(ctx, ModelRef{Provider: provider, ModelID: "m"})
		if err != nil {
			t.Fatal(err)
		}
		w, err := s.client.Watch(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if settled := ask(t, s.client, conv, "hello"); settled.Status != "unanswered" {
			t.Fatalf("%s: %+v; want unanswered", provider, settled)
		}
		waitForOps(t, w, "the server's egress policy doesn't allow this endpoint: "+want)
		return conv
	}
	refused("public", "127.0.0.1 is a loopback address")
	if server.sawKey(refusedKey) {
		t.Fatal("the key of a refused endpoint reached it")
	}
	refused("lan", "lan.test resolves to 10.0.0.1, a private address")
	refused("azure", "azure.test resolves to 168.63.129.16, a cloud metadata address")

	// The first connection goes to the allowed address; the next lookup answers another, which
	// is refused before connecting, so the next request fails rather than going there.
	conv, err := s.client.CreateConversationOn(ctx, ModelRef{Provider: "rebind", ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if settled := ask(t, s.client, conv, "first"); settled.Status != "done" {
		t.Fatalf("rebind, first request: %+v; want an answer", settled)
	}
	refused("rebind", "rebind.test resolves to 127.0.0.2, a loopback address")

	s.stop()
	if out := s.output.String(); !strings.Contains(out, "provider lan: the server's egress policy") {
		t.Errorf("the refusal wasn't logged:\n%s", out)
	}
}
