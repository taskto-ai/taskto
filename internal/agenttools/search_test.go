package agenttools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A space_id or limit that is there but null, 0 or negative is refused, for both searches:
// only leaving space_id out searches every space, so a null from a model that fills every
// field never widens a call, and a limit of 0 is not the default.
func TestSearchRefusesNullAndNonPositiveArguments(t *testing.T) {
	w := newWorld(t)
	for _, tool := range []string{"search_docs", "search_cards", "search_records"} {
		for _, v := range []any{nil, 0, -1, "1", 1.5} {
			w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": "zanzibar", "space_id": v})
		}
		for _, v := range []any{nil, 0, -1, 21, "5", 1.5} {
			w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": "zanzibar", "limit": v})
		}
		// A whole number written with a fraction part is an integer to JSON Schema: accepted.
		for _, raw := range []string{"10.0", "1e1"} {
			code, out := w.callWith("Bearer "+w.capability(w.aliceConv), tool, json.RawMessage(`{"query":"zanzibar","limit":`+raw+`,"space_id":`+fmt.Sprint(w.shared)+`.0}`))
			if code != http.StatusOK {
				t.Fatalf("%s with limit %s = %d %v", tool, raw, code, out)
			}
		}
		// Left out, or valid, they are the usual search.
		w.ok(w.aliceConv, tool, map[string]any{"query": "zanzibar"})
		w.ok(w.aliceConv, tool, map[string]any{"query": "zanzibar", "space_id": w.shared, "limit": 20})
	}
}

// The schema's limit on a query is in characters and the server's in bytes: both are
// refused over the bytes, and ASCII up to the limit is searched.
func TestSearchQueryLimitIsInBytes(t *testing.T) {
	w := newWorld(t)
	for _, tool := range []string{"search_docs", "search_cards", "search_records"} {
		// 300 characters, 600 bytes: within the schema's count, over the bytes.
		w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": strings.Repeat("é", 300)})
		w.status(http.StatusBadRequest, w.aliceConv, tool, map[string]any{"query": strings.Repeat("a", 501)})
		w.ok(w.aliceConv, tool, map[string]any{"query": strings.Repeat("a", 500)})
	}
}
