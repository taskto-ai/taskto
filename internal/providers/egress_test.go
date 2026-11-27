package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/egress"
)

func (e *env) setEgress(eg Egress) {
	e.api.SetEgress(eg)
	e.feed.egress = eg
}

func fakeDNS(names map[string]string) egress.Resolver {
	return func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if a, ok := names[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
}

func TestEgressPolicy(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	alice := e.invite(owner, ws, "alice@example.com", "member")
	srv := newModelServer(t, "model-a", "model-b")
	e.api.resolve = fakeDNS(map[string]string{"private.example": "10.0.0.1", "public.example": "8.8.8.8"})

	rec := alice.do(http.MethodPost, "/api/me/providers", account("mine", srv.URL+"/v1", ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	mine := decode[Account](t, rec)
	rec = owner.do(http.MethodPost, wsPath(ws, "providers"), account("team", srv.URL+"/v1", ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	team := decode[Account](t, rec)

	e.setEgress(Egress{Personal: egress.MustParse("public")})
	for url, want := range map[string]string{
		srv.URL + "/v1":              "127.0.0.1 is a loopback address",
		"http://169.254.169.254/v1":  "169.254.169.254 is a link-local address",
		"http://[fd00:ec2::254]/v1":  "fd00:ec2::254 is a unique-local address",
		"https://private.example/v1": "private.example resolves to 10.0.0.1, a private address",
	} {
		rec := alice.do(http.MethodPost, "/api/me/providers", account("refused", url, nil))
		expect(t, rec, http.StatusBadRequest)
		if body := rec.Body.String(); !strings.Contains(body, "base_url: the server's egress policy doesn't allow this endpoint") || !strings.Contains(body, want) ||
			!strings.Contains(body, `"code":"egress_refused","params":{"host":`) {
			t.Errorf("creating %s: %s; want it to say %q, with its code and host", url, body, want)
		}
	}
	expect(t, alice.do(http.MethodPost, "/api/me/providers", account("public", "https://public.example/v1", nil)), http.StatusCreated)
	expect(t, alice.do(http.MethodPost, "/api/me/providers", account("later", "https://unknown.example/v1", nil)), http.StatusCreated)

	sent := len(srv.sent())
	rec = alice.do(http.MethodPost, fmt.Sprintf("/api/me/providers/%d/test", mine.ID), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Check](t, rec); got.OK || got.Error != "the server's egress policy doesn't allow this endpoint: 127.0.0.1 is a loopback address" ||
		got.Code != apierr.CodeEgressRefused || got.Params["host"] != "127.0.0.1" {
		t.Fatalf("test of a refused account = %+v", got)
	}
	if len(srv.sent()) != sent {
		t.Fatal("a refused test reached the endpoint")
	}
	rec = owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("providers/%d/test", team.ID)), nil)
	if got := decode[Check](t, rec); !got.OK {
		t.Fatalf("test of the workspace's account = %+v", got)
	}

	one := fmt.Sprintf("/api/me/providers/%d", mine.ID)
	expect(t, alice.do(http.MethodPatch, one, map[string]any{"display_name": "Mine"}), http.StatusOK)
	expect(t, alice.do(http.MethodPatch, one, map[string]any{"base_url": "http://10.1.1.1/v1"}), http.StatusBadRequest)
	expect(t, alice.do(http.MethodPatch, one, map[string]any{"base_url": "https://public.example/v1"}), http.StatusOK)

	e.setEgress(Egress{Workspace: egress.MustParse("127.0.0.1, ollama.lan"), Personal: egress.MustParse("public")})
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("lan", "http://ollama.lan:11434/v1", nil)), http.StatusCreated)
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("other", "https://public.example/v1", nil)), http.StatusBadRequest)
	rec = owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("providers/%d/test", team.ID)), nil)
	if got := decode[Check](t, rec); !got.OK {
		t.Fatalf("test of the allowlisted workspace account = %+v", got)
	}

	e.setEgress(Egress{Workspace: egress.MustParse("public, 10.0.0.0/8"), Personal: egress.MustParse("public")})
	expect(t, alice.do(http.MethodPatch, one, map[string]any{"display_name": "Mine again"}), http.StatusOK)
	raw, err := os.ReadFile(e.feed.Path())
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Providers []struct {
			ID     string   `json:"id"`
			Egress []string `json:"egress"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range file.Providers {
		got[p.ID] = strings.Join(p.Egress, ",")
	}
	if got["script"] != "" || got[team.Provider] != "public,10.0.0.0/8" || got[mine.Provider] != "public" {
		t.Fatalf("egress in the providers file = %v", got)
	}
}
