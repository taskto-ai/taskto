package providers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type modelServer struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string
}

func newModelServer(t *testing.T, models ...string) *modelServer {
	t.Helper()
	m := &modelServer{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		m.mu.Lock()
		m.keys = append(m.keys, key)
		m.mu.Unlock()
		switch {
		case r.URL.Path == "/elsewhere/v1/models":
			http.Redirect(w, r, "https://llm.example.com/v1/models", http.StatusFound)
		case r.Method != http.MethodGet || r.URL.Path != "/v1/models":
			http.NotFound(w, r)
		case key != testKey:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error": "invalid key %s"}`, key)
		default:
			var data []string
			for _, id := range models {
				data = append(data, fmt.Sprintf(`{"id": %q, "object": "model"}`, id))
			}
			fmt.Fprintf(w, `{"object": "list", "data": [%s]}`, strings.Join(data, ","))
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *modelServer) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.keys...)
}

func TestTestingAnAccount(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	srv := newModelServer(t, "model-a", "other")

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("local", srv.URL+"/v1", ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	id := decode[Account](t, rec).ID
	test := wsPath(ws, fmt.Sprintf("providers/%d/test", id))

	rec = owner.do(http.MethodPost, test, nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Check](t, rec); !got.OK || got.Error != "" || strings.Join(got.Missing, ",") != "model-b" {
		t.Fatalf("check = %+v", got)
	}
	if keys := srv.sent(); len(keys) != 1 || keys[0] != testKey {
		t.Fatalf("the endpoint was sent %q", keys)
	}

	expect(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("providers/%d", id)), map[string]any{"api_key": otherKey}), http.StatusOK)
	rec = owner.do(http.MethodPost, test, nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Check](t, rec); got.OK || got.Error != "the endpoint answered 401: check the API key" {
		t.Fatalf("check with a wrong key = %+v", got)
	}

	expect(t, member.do(http.MethodPost, test, nil), http.StatusForbidden)
	expect(t, owner.do(http.MethodDelete, wsPath(ws, fmt.Sprintf("members/%d", member.id)), nil), http.StatusNoContent)
	expect(t, member.do(http.MethodPost, test, nil), http.StatusNotFound)
	if n := len(srv.sent()); n != 2 {
		t.Fatalf("the endpoint was called %d times, want 2", n)
	}

	rec = owner.do(http.MethodPost, "/api/me/providers", account("moved", srv.URL+"/elsewhere/v1", nil))
	expect(t, rec, http.StatusCreated)
	moved := decode[Account](t, rec).ID
	rec = owner.do(http.MethodPost, fmt.Sprintf("/api/me/providers/%d/test", moved), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Check](t, rec); got.OK || got.Error != "the endpoint answered 302 to GET /models" {
		t.Fatalf("check of a redirect = %+v", got)
	}

	srv.Close()
	rec = owner.do(http.MethodPost, test, nil)
	if got := decode[Check](t, rec); got.OK || got.Error != "no answer from the endpoint" {
		t.Fatalf("check without an endpoint = %+v", got)
	}

	e.mu.Lock()
	bodies := e.bodies.String()
	e.mu.Unlock()
	for _, text := range []string{bodies, e.logs.String()} {
		if strings.Contains(text, otherKey) || strings.Contains(text, testKey) {
			t.Fatal("a key reached a response or the log")
		}
	}
}

func TestTestingAPersonalAccount(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	alice := e.invite(owner, ws, "alice@example.com", "member")
	srv := newModelServer(t, "model-a", "model-b")

	rec := alice.do(http.MethodPost, "/api/me/providers", account("mine", srv.URL+"/v1", ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	test := fmt.Sprintf("/api/me/providers/%d/test", decode[Account](t, rec).ID)

	expect(t, owner.do(http.MethodPost, test, nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, wsPath(ws, strings.TrimPrefix(test, "/api/me/")), nil), http.StatusNotFound)
	expect(t, (&client{e: e}).do(http.MethodPost, test, nil), http.StatusUnauthorized)
	if n := len(srv.sent()); n != 0 {
		t.Fatalf("refused tests called the endpoint %d times", n)
	}

	rec = alice.do(http.MethodPost, test, nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Check](t, rec); !got.OK || got.Missing != nil {
		t.Fatalf("alice's check = %+v", got)
	}
}
