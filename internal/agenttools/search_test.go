package agenttools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
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
