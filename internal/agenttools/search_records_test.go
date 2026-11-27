package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func (w *world) publishedApp(space int64, name string) int64 {
	w.t.Helper()
	def := `{"collection":{"fields":[{"name":"note","type":"string"},{"name":"count","type":"number"}]}}`
	app := w.insert(`INSERT INTO apps (space_id, name, definition) VALUES (?, ?, ?) RETURNING id`, space, name, def)
	w.exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires) VALUES (?, 1, ?, 1, '[]')`, app, def)
	w.exec(`UPDATE apps SET live_release = 1 WHERE id = ?`, app)
	return app
}

func (w *world) record(app, creator int64, note string) {
	w.t.Helper()
	w.exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, json_object('note', ?, 'count', 1), ?)`, app, note, creator)
}

func hits(out map[string]any, field string) []string {
	var got []string
	for _, r := range out["results"].([]any) {
		got = append(got, fmt.Sprint(r.(map[string]any)[field]))
	}
	return got
}

func TestSearchRecordsIsForTheConversationsOwner(t *testing.T) {
	w := newWorld(t)
	team := w.publishedApp(w.shared, "Team notes")
	w.record(team, w.alice, "zanzibar from alice")
	w.record(team, w.bob, "zanzibar from bob")
	mine := w.publishedApp(w.alicePrivate, "Alice's private notes")
	w.record(mine, w.alice, "zanzibar alice's secret")
	theirs := w.publishedApp(w.bobPrivate, "Bob's private notes")
	w.record(theirs, w.bob, "zanzibar bob's secret")
	elsewhere := w.publishedApp(w.alicePrivate2, "Notes in another workspace")
	w.record(elsewhere, w.alice, "zanzibar in two")

	got := hits(w.ok(w.aliceConv, "search_records", map[string]any{"query": "zanzibar"}), "snippet")
	if len(got) != 3 {
		t.Fatalf("alice found %q", got)
	}
	for _, g := range got {
		if strings.Contains(g, "bob's secret") || strings.Contains(g, "in two") {
			t.Fatalf("alice found %q", g)
		}
	}
	if got := hits(w.ok(w.bobConv, "search_records", map[string]any{"query": "zanzibar"}), "app_name"); len(got) != 3 {
		t.Fatalf("bob found %q", got)
	}
	for _, g := range hits(w.ok(w.bobConv, "search_records", map[string]any{"query": "zanzibar"}), "snippet") {
		if strings.Contains(g, "alice's secret") || strings.Contains(g, "in two") {
			t.Fatalf("bob found %q", g)
		}
	}
	if got := hits(w.ok(w.aliceConv2, "search_records", map[string]any{"query": "zanzibar"}), "snippet"); len(got) != 1 || got[0] != "zanzibar in two" {
		t.Fatalf("alice in two found %q", got)
	}

	for _, space := range []int64{w.bobPrivate, w.alicePrivate2} {
		out := w.status(http.StatusNotFound, w.aliceConv, "search_records", map[string]any{"query": "zanzibar", "space_id": space})
		if out["error"] != "not found" {
			t.Fatalf("space %d: %v", space, out)
		}
	}
	one := w.ok(w.aliceConv, "search_records", map[string]any{"query": "zanzibar", "space_id": w.shared})
	if got := hits(one, "space_id"); len(got) != 2 {
		t.Fatalf("alice in the team space found %q", got)
	}
	for _, args := range []map[string]any{{"query": "x", "space_id": nil}, {"query": "x", "limit": 0}, {"query": "x", "limit": 21}} {
		w.status(http.StatusBadRequest, w.aliceConv, "search_records", args)
	}

	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder'), (?, ?, 'reader'), (?, ?, 'user')`, team, w.alice, team, w.alice, team, w.bob)
	if got := hits(w.ok(w.bobConv, "search_records", map[string]any{"query": "zanzibar", "space_id": w.shared}), "snippet"); len(got) != 1 || got[0] != "zanzibar from bob" {
		t.Fatalf("bob, a user of the app, found %q", got)
	}
	if got := hits(w.ok(w.aliceConv, "search_records", map[string]any{"query": "zanzibar", "space_id": w.shared}), "snippet"); len(got) != 2 {
		t.Fatalf("alice, its builder and reader, found %q", got)
	}
	w.exec(`DELETE FROM app_roles WHERE user_id = ?`, w.bob)
	if got := hits(w.ok(w.bobConv, "search_records", map[string]any{"query": "zanzibar", "space_id": w.shared}), "snippet"); len(got) != 0 {
		t.Fatalf("bob with no role found %q", got)
	}
}

type fakeTranscripts struct {
	mu   sync.Mutex
	text map[int64]string
}

func (f *fakeTranscripts) Transcript(_ context.Context, agentID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, ok := f.text[agentID]
	if !ok {
		return "", errors.New("unknown")
	}
	return text, nil
}

func TestSearchConversationsIsForTheConversationsOwner(t *testing.T) {
	w := newWorld(t)
	src := &fakeTranscripts{text: map[int64]string{}}
	w.tools.o.Search.SetConversations(src)
	agentOf := func(conv int64) int64 { return w.insert(`SELECT agent_id FROM conversations WHERE id = ?`, conv) }
	src.text[agentOf(w.aliceConv)] = "alice plans the zanzibar trip\n"
	src.text[agentOf(w.bobConv)] = "bob dreams of zanzibar\n"
	src.text[agentOf(w.aliceConv2)] = "alice in the other workspace: zanzibar\n"
	older := w.conversation(w.one, w.alice)
	w.exec(`UPDATE conversations SET title = 'Zanzibar notes' WHERE id = ?`, older)
	src.text[agentOf(older)] = "nothing\n"

	out := w.ok(w.aliceConv, "search_conversations", map[string]any{"query": "zanzibar"})
	found := hits(out, "conversation_id")
	if len(found) != 2 || !contains(found, fmt.Sprint(w.aliceConv)) || !contains(found, fmt.Sprint(older)) {
		t.Fatalf("alice found %v, want her two in workspace one (%d, %d)", found, w.aliceConv, older)
	}
	if got := hits(w.ok(w.bobConv, "search_conversations", map[string]any{"query": "zanzibar"}), "conversation_id"); len(got) != 1 || got[0] != fmt.Sprint(w.bobConv) {
		t.Fatalf("bob found %v", got)
	}
	if got := hits(w.ok(w.aliceConv2, "search_conversations", map[string]any{"query": "zanzibar"}), "conversation_id"); len(got) != 1 || got[0] != fmt.Sprint(w.aliceConv2) {
		t.Fatalf("alice in two found %v", got)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "search_conversations", map[string]any{"query": "zanzibar", "space_id": w.shared})
	for _, args := range []map[string]any{{"query": "x", "limit": 0}, {"query": "x", "limit": 21}, {"query": strings.Repeat("a", 501)}} {
		w.status(http.StatusBadRequest, w.aliceConv, "search_conversations", args)
	}

	_, err := w.tools.Call(context.Background(), Caller{Workspace: w.one, User: w.alice, Token: "mine"}, "search_conversations",
		json.RawMessage(`{"query":"zanzibar"}`))
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("over MCP: %v", err)
	}
	for _, d := range Definitions() {
		if d.Name == "search_conversations" && (!d.NoToken || !d.ReadOnly) {
			t.Fatalf("search_conversations is %+v", d)
		}
		if d.Name == "search_records" && d.NoToken {
			t.Fatalf("search_records is not for tokens: %+v", d)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSearchRepliesFitMaxResult(t *testing.T) {
	w := newWorld(t)
	app := w.publishedApp(w.shared, "Team notes")
	body := "zanzibar " + strings.Repeat("<", 3000)
	for range 20 {
		w.record(app, w.alice, body)
	}
	out := w.ok(w.aliceConv, "search_records", map[string]any{"query": "zanzibar", "limit": 20})
	got := out["results"].([]any)
	if len(got) == 0 || len(got) >= 20 {
		t.Fatalf("%d hits: want some, but not all twenty", len(got))
	}
	next, ok := out["next_offset"].(float64)
	if !ok || int(next) != len(got) {
		t.Fatalf("next_offset = %v after %d hits", out["next_offset"], len(got))
	}
	rest := w.ok(w.aliceConv, "search_records", map[string]any{"query": "zanzibar", "limit": 20, "offset": int(next)})["results"].([]any)
	if len(rest) == 0 {
		t.Fatalf("the next page has %d hits", len(rest))
	}

	src := &fakeTranscripts{text: map[int64]string{}}
	w.tools.o.Search.SetConversations(src)
	for _, conv := range []int64{w.aliceConv, w.bobConv, w.aliceConv2, w.aliceConv} {
		src.text[w.insert(`SELECT agent_id FROM conversations WHERE id = ?`, conv)] = "nothing"
	}
	for range 20 {
		c := w.conversation(w.one, w.alice)
		w.exec(`UPDATE conversations SET title = ? WHERE id = ?`, "Zanzibar", c)
		src.text[w.insert(`SELECT agent_id FROM conversations WHERE id = ?`, c)] = body
	}
	conv := w.ok(w.aliceConv, "search_conversations", map[string]any{"query": "zanzibar", "limit": 20})
	hits := conv["results"].([]any)
	if len(hits) == 0 || len(hits) >= 20 || conv["next_offset"] == nil {
		t.Fatalf("conversations: %d hits, next_offset %v", len(hits), conv["next_offset"])
	}
}

func TestFittedSearchReplyNextOffsetStaysWithinTheMaximum(t *testing.T) {
	type hit struct {
		Snippet string `json:"snippet"`
	}
	hitsOf := func() []hit {
		var hs []hit
		for range 20 {
			hs = append(hs, hit{Snippet: strings.Repeat("<", 3000)})
		}
		return hs
	}
	next := func(offset int) (shown int, next any) {
		t.Helper()
		reply, err := fitSearchReply(hitsOf(), offset, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		m := reply.(map[string]any)
		return reflectLen(m["results"]), m["next_offset"]
	}
	shown, got := next(0)
	if shown == 0 || shown >= 20 || got != shown {
		t.Fatalf("at offset 0: %d shown, next_offset %v", shown, got)
	}
	if _, got := next(1000 - shown); got != 1000 {
		t.Fatalf("a next_offset of exactly the maximum = %v, want 1000", got)
	}
	if _, got := next(1000 - shown + 1); got != nil {
		t.Fatalf("a next_offset past the maximum was offered: %v", got)
	}
}

func reflectLen(v any) int { return reflect.ValueOf(v).Len() }

func TestSearchOffsetIsReadStrictly(t *testing.T) {
	w := newWorld(t)
	w.publishedApp(w.shared, "Team notes")
	raw := func(offset string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/tools/search_records", strings.NewReader(`{"query":"zanzibar","offset":`+offset+`}`))
		r.Header.Set("Authorization", "Bearer "+w.capability(w.aliceConv))
		rec := httptest.NewRecorder()
		w.tools.ServeHTTP(rec, r)
		return rec.Code
	}
	for offset, want := range map[string]int{"null": http.StatusBadRequest, `"3"`: http.StatusBadRequest, "-1": http.StatusBadRequest,
		"1.5": http.StatusBadRequest, "1001": http.StatusBadRequest, "10.0": http.StatusOK, "0": http.StatusOK, "1000": http.StatusOK} {
		if got := raw(offset); got != want {
			t.Errorf("offset %s = %d, want %d", offset, got, want)
		}
	}
}
