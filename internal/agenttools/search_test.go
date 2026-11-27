package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/search"
)

func TestSearchRefusesNullAndNonPositiveArguments(t *testing.T) {
	w := newWorld(t)
	for _, tool := range []string{"search_docs", "search_cards", "search_records"} {
		for _, v := range []any{nil, 0, -1, "1", 1.5} {
			w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": "zanzibar", "space_id": v})
		}
		for _, v := range []any{nil, 0, -1, 21, "5", 1.5} {
			w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": "zanzibar", "limit": v})
		}
		for _, raw := range []string{"10.0", "1e1"} {
			code, out := w.callWith("Bearer "+w.capability(w.aliceConv), tool, json.RawMessage(`{"query":"zanzibar","limit":`+raw+`,"space_id":`+fmt.Sprint(w.shared)+`.0}`))
			if code != http.StatusOK {
				t.Fatalf("%s with limit %s = %d %v", tool, raw, code, out)
			}
		}
		w.ok(w.aliceConv, tool, map[string]any{"query": "zanzibar"})
		w.ok(w.aliceConv, tool, map[string]any{"query": "zanzibar", "space_id": w.shared, "limit": 20})
	}
}

func TestSearchQueryLimitIsInBytes(t *testing.T) {
	w := newWorld(t)
	for _, tool := range []string{"search_docs", "search_cards", "search_records"} {
		w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": strings.Repeat("é", 300)})
		w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": strings.Repeat("a", 501)})
		w.ok(w.aliceConv, tool, map[string]any{"query": strings.Repeat("a", 500)})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSearchDocsReleasesNothingOfASpaceTheCallerLostWhileItRan(t *testing.T) {
	var onEmbed func()
	w := newWorldWith(t, search.Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if onEmbed != nil {
			onEmbed()
		}
		body := `{"data":[{"index":0,"embedding":[1,0,0,0]}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}})
	account := w.insert(`INSERT INTO provider_accounts (workspace_id, name, display_name, kind, base_url, models)
		VALUES (?, 'embed', 'Embed', 'openai-compatible', 'https://embed.example.test/v1', '[{"id":"m","name":"m"}]') RETURNING id`, w.one)
	w.exec(`INSERT INTO search_settings (workspace_id, account_id, model) VALUES (?, ?, 'm')`, w.one, account)
	w.exec(`INSERT INTO search_embedding_consent (space_id, account_id, model, base_url) VALUES (?, ?, 'm', 'https://embed.example.test/v1')`, w.shared, account)
	shared := access.Space{ID: w.shared, WorkspaceID: w.one, Kind: access.Shared, Access: access.Write}
	if _, err := w.kb.Put(context.Background(), shared, "secret.md", []byte("confidential kumquat payroll memo"), "",
		knowledge.Author{Name: "Alice", Email: "alice@example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	if res := w.ok(w.bobConv, "search_docs", map[string]any{"query": "kumquat"})["results"].([]any); len(res) != 1 {
		t.Fatalf("fixture: bob's search found %d documents", len(res))
	}

	onEmbed = func() {
		onEmbed = nil
		w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared, w.bob)
	}
	res := w.ok(w.bobConv, "search_docs", map[string]any{"query": "kumquat"})["results"].([]any)
	if len(res) != 0 {
		t.Fatalf("a search_docs call that lost access while it ran returned %v", res)
	}
}
