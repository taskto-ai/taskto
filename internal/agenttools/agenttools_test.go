package agenttools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/serverkey"
)

// world is two workspaces and three people:
//
//   - one: alice and bob. shared, a shared space alice edits and bob views; each has a
//     private space; aliceConv and bobConv are their conversations there.
//   - two: alice and carol. aliceConv2 is alice's conversation there.
type world struct {
	t                          *testing.T
	db                         *sql.DB
	kb                         *knowledge.Store
	tools                      *Service
	alice, bob, carol          int64
	one, two                   int64
	alicePrivate, bobPrivate   int64
	alicePrivate2, shared      int64
	aliceConv, bobConv         int64
	aliceConv2                 int64
	alicePersonal, bobPersonal int64 // their personal boards in one
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	key, err := serverkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	kb := knowledge.New(dir, knowledge.Options{})
	index := search.New(sqlDB, kb, providers.NewStore(sqlDB, key), search.Options{})
	checker := access.New(sqlDB, nil)
	w := &world{t: t, db: sqlDB, kb: kb, tools: New(Options{
		DB: sqlDB, Key: key, Access: checker, Boards: boards.New(sqlDB, checker), Apps: apps.New(sqlDB, checker),
		Knowledge: kb, Search: index,
	})}
	w.alice = w.insert(`INSERT INTO users (email, name, password_hash) VALUES ('alice@example.com', 'Alice', 'x') RETURNING id`)
	w.bob = w.insert(`INSERT INTO users (email, name, password_hash) VALUES ('bob@example.com', 'Bob', 'x') RETURNING id`)
	w.carol = w.insert(`INSERT INTO users (email, name, password_hash) VALUES ('carol@example.com', 'Carol', 'x') RETURNING id`)
	w.one = w.insert(`INSERT INTO workspaces (name) VALUES ('One') RETURNING id`)
	w.two = w.insert(`INSERT INTO workspaces (name) VALUES ('Two') RETURNING id`)
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin'), (?, ?, 'member'), (?, ?, 'admin'), (?, ?, 'member')`,
		w.one, w.alice, w.one, w.bob, w.two, w.alice, w.two, w.carol)
	w.alicePrivate = w.private(w.one, w.alice)
	w.bobPrivate = w.private(w.one, w.bob)
	w.alicePrivate2 = w.private(w.two, w.alice)
	w.shared = w.insert(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Team') RETURNING id`, w.one)
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor'), (?, ?, 'viewer')`, w.shared, w.alice, w.shared, w.bob)
	w.aliceConv = w.conversation(w.one, w.alice)
	w.bobConv = w.conversation(w.one, w.bob)
	w.aliceConv2 = w.conversation(w.two, w.alice)
	w.alicePersonal = w.insert(`SELECT id FROM boards WHERE space_id = ?`, w.alicePrivate)
	w.bobPersonal = w.insert(`SELECT id FROM boards WHERE space_id = ?`, w.bobPrivate)
	return w
}

func (w *world) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.db.Exec(q, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) insert(q string, args ...any) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.QueryRow(q, args...).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *world) private(ws, user int64) int64 {
	return w.insert(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, ws, user)
}

var agentIDs int64

func (w *world) conversation(ws, user int64) int64 {
	agentIDs++
	return w.insert(`INSERT INTO conversations (workspace_id, owner_id, agent_id) VALUES (?, ?, ?) RETURNING id`, ws, user, agentIDs)
}

// call calls tool for conversation conv with args, and returns the status and reply.
func (w *world) call(conv int64, tool string, args any) (int, map[string]any) {
	w.t.Helper()
	return w.callWith("Bearer "+w.capability(conv), tool, args)
}

// capability is conv's, as the server hands it out.
func (w *world) capability(conv int64) string {
	w.t.Helper()
	return w.tools.Capability(conv, w.insert(`SELECT agent_id FROM conversations WHERE id = ?`, conv))
}

func (w *world) callWith(authorization, tool string, args any) (int, map[string]any) {
	w.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		w.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/tools/"+tool, bytes.NewReader(raw))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	w.tools.ServeHTTP(rec, r)
	if rec.Body.Len() > MaxResult+1024 {
		w.t.Fatalf("%s: a reply of %d bytes", tool, rec.Body.Len())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		w.t.Fatalf("%s: reply %q: %v", tool, rec.Body.String(), err)
	}
	return rec.Code, out
}

// ok calls a tool that must succeed.
func (w *world) ok(conv int64, tool string, args any) map[string]any {
	w.t.Helper()
	code, out := w.call(conv, tool, args)
	if code != http.StatusOK {
		w.t.Fatalf("%s %v = %d %v", tool, args, code, out)
	}
	return out
}

// status calls a tool that must answer with code.
func (w *world) status(code int, conv int64, tool string, args any) map[string]any {
	w.t.Helper()
	got, out := w.call(conv, tool, args)
	if got != code {
		w.t.Fatalf("%s %v = %d %v, want %d", tool, args, got, out, code)
	}
	return out
}

func id(v any) int64 { return int64(v.(float64)) }

func TestCapabilities(t *testing.T) {
	w := newWorld(t)
	good := w.capability(w.aliceConv)
	if !strings.HasPrefix(good, "ttc1."+strconv.FormatInt(w.aliceConv, 10)+".") {
		t.Fatalf("capability = %q", good)
	}
	if code, out := w.callWith("Bearer "+good, "list_spaces", map[string]any{}); code != http.StatusOK {
		t.Fatalf("a genuine capability: %d %v", code, out)
	}
	// Another conversation's MAC, a changed ID, a changed MAC, another server's key, and
	// no capability at all are all refused alike.
	other := w.capability(w.bobConv)
	_, otherMAC, _ := strings.Cut(strings.TrimPrefix(other, "ttc1."), ".")
	dir := t.TempDir()
	key, err := serverkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	foreign := New(Options{Key: key}).Capability(w.aliceConv, w.insert(`SELECT agent_id FROM conversations WHERE id = ?`, w.aliceConv))
	// The conversation's ID with another agent runtime conversation, as if the ID had been
	// used again.
	reused := w.tools.Capability(w.aliceConv, 9999)
	// The MAC's first character changed, always to a different one: with a random key, a
	// fixed replacement can be what the MAC already has.
	at := strings.LastIndex(good, ".") + 1
	swap := map[bool]string{true: "B", false: "A"}[good[at] == 'A']
	changed := good[:at] + swap + good[at+1:]
	for name, auth := range map[string]string{
		"another conversation's MAC": "Bearer ttc1." + strconv.FormatInt(w.aliceConv, 10) + "." + otherMAC,
		"a changed MAC":              "Bearer " + changed,
		"another key":                "Bearer " + foreign,
		"another agent conversation": "Bearer " + reused,
		"a padded ID":                "Bearer ttc1.0" + strings.TrimPrefix(good, "ttc1."),
		"no prefix":                  "Bearer " + strings.TrimPrefix(good, "ttc1."),
		"not a bearer":               good,
		"nothing":                    "",
	} {
		code, out := w.callWith(auth, "list_spaces", map[string]any{})
		if code != http.StatusUnauthorized {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestRequests(t *testing.T) {
	w := newWorld(t)
	w.status(http.StatusNotFound, w.aliceConv, "no_such_tool", map[string]any{})
	w.status(http.StatusBadRequest, w.aliceConv, "list_boards", map[string]any{"space_id": w.alicePrivate, "spaceid": 1})
	w.status(http.StatusBadRequest, w.aliceConv, "list_boards", []int{1})
	r := httptest.NewRequest(http.MethodGet, "/tools/list_spaces", nil)
	r.Header.Set("Authorization", "Bearer "+w.capability(w.aliceConv))
	rec := httptest.NewRecorder()
	w.tools.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
}

// A conversation reaches exactly its owner's spaces in its own workspace: never another
// user's, never the owner's in another workspace, and answers alike for those and for
// spaces that don't exist.
func TestConversationsCannotReachOtherUsersSpaces(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.ok(w.bobConv, "write_doc", map[string]any{"space_id": w.bobPrivate, "path": "secret.md", "content": "# Bob's plan\n\nzanzibar"})
	w.ok(w.aliceConv2, "write_doc", map[string]any{"space_id": w.alicePrivate2, "path": "elsewhere.md", "content": "zanzibar elsewhere"})
	bobCard, err := w.tools.o.Boards.CreateCard(ctx, boards.Actor{UserID: w.bob}, w.bobPrivate, w.bobPersonal, boards.NewCard{Title: "Bob's card"})
	if err != nil {
		t.Fatal(err)
	}

	spaces := w.ok(w.aliceConv, "list_spaces", map[string]any{})["spaces"].([]any)
	var ids []int64
	for _, s := range spaces {
		ids = append(ids, id(s.(map[string]any)["id"]))
	}
	if fmt.Sprint(ids) != fmt.Sprint([]int64{w.alicePrivate, w.shared}) {
		t.Fatalf("alice's spaces in one = %v, want %v", ids, []int64{w.alicePrivate, w.shared})
	}

	missing := w.alicePrivate2 + 1000
	for _, space := range []int64{w.bobPrivate, w.alicePrivate2, missing} {
		for tool, args := range map[string]map[string]any{
			"list_docs":    {},
			"read_doc":     {"path": "secret.md"},
			"write_doc":    {"path": "x.md", "content": "x"},
			"search_docs":  {"query": "zanzibar"},
			"list_boards":  {},
			"list_cards":   {"board_id": w.bobPersonal, "labels": []string{"x"}, "overdue": true, "sort": "due"},
			"show_card":    {"board_id": w.bobPersonal, "card_id": bobCard.ID},
			"create_card":  {"board_id": w.bobPersonal, "title": "x", "due": "2026-01-01", "labels": []string{"x"}},
			"update_card":  {"board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1, "title": "x", "due": nil, "labels": []string{}},
			"move_card":    {"board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1, "column": "done"},
			"comment_card": {"board_id": w.bobPersonal, "card_id": bobCard.ID, "body": "x"},
			"set_blocked":  {"board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1, "reason": "x"},
		} {
			args["space_id"] = space
			out := w.status(http.StatusNotFound, w.aliceConv, tool, args)
			if out["error"] != "not found" {
				t.Fatalf("%s in space %d: %v", tool, space, out)
			}
		}
	}
	// Bob's board through alice's own space is no way round.
	w.status(http.StatusNotFound, w.aliceConv, "show_card", map[string]any{"space_id": w.alicePrivate, "board_id": w.bobPersonal, "card_id": bobCard.ID})

	// Searching everything alice can read finds nothing of bob's, nor of her other workspace.
	res := w.ok(w.aliceConv, "search_docs", map[string]any{"query": "zanzibar"})["results"].([]any)
	if len(res) != 0 {
		t.Fatalf("alice's search found %v", res)
	}
	if res := w.ok(w.bobConv, "search_docs", map[string]any{"query": "zanzibar"})["results"].([]any); len(res) != 1 {
		t.Fatalf("bob's search = %v", res)
	}

	// Nothing was written or changed for alice's calls.
	card, err := w.tools.o.Boards.ShowCard(ctx, boards.Actor{UserID: w.bob}, w.bobPrivate, w.bobPersonal, bobCard.ID)
	if err != nil || card.Version != 1 || card.Title != "Bob's card" || len(card.Comments) != 0 {
		t.Fatalf("bob's card = %+v, %v", card, err)
	}
	if page, err := w.kb.List(ctx, access.Space{ID: w.bobPrivate, WorkspaceID: w.one, Access: access.Read}, "", "", 0); err != nil || len(page.Docs) != 1 {
		t.Fatalf("bob's documents = %+v, %v", page, err)
	}
}

// What the agent writes is its user's, marked as the agent's.
func TestWritesAreTheUsersMadeByTheAgent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	doc := w.ok(w.aliceConv, "write_doc", map[string]any{"space_id": w.shared, "path": "notes/plan.md", "content": "# Plan", "message": "Add the plan"})
	if doc["commit"] == "" {
		t.Fatalf("write_doc = %v", doc)
	}
	h, err := w.kb.History(ctx, access.Space{ID: w.shared, WorkspaceID: w.one, Access: access.Read}, "notes/plan.md", "", 0)
	if err != nil || len(h.Commits) != 1 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if c := h.Commits[0]; c.Author.Name != "Alice" || c.Author.Email != "alice@example.com" || c.Agent != AgentName || c.Message != "Add the plan" {
		t.Fatalf("commit = %+v", c)
	}

	board := id(w.ok(w.aliceConv, "list_boards", map[string]any{"space_id": w.alicePrivate})["boards"].([]any)[0].(map[string]any)["id"])
	card := w.ok(w.aliceConv, "create_card", map[string]any{"space_id": w.alicePrivate, "board_id": board, "title": "Draft", "docs": []string{"notes/plan.md"}})
	by := card["created_by"].(map[string]any)
	if id(by["id"]) != w.alice || by["agent"] != true {
		t.Fatalf("created_by = %v", by)
	}
	ref := map[string]any{"space_id": w.alicePrivate, "board_id": board, "card_id": id(card["id"])}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range ref {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	card = w.ok(w.aliceConv, "move_card", with(map[string]any{"version": 1, "column": "doing"}))
	w.status(http.StatusConflict, w.aliceConv, "update_card", with(map[string]any{"version": 1, "title": "Stale"}))
	card = w.ok(w.aliceConv, "update_card", with(map[string]any{"version": card["version"], "body": "Working on it", "conversations": []string{strconv.FormatInt(w.aliceConv, 10)}}))
	card = w.ok(w.aliceConv, "set_blocked", with(map[string]any{"version": card["version"], "reason": "waiting for review"}))
	if card["blocked"] != true || card["blocked_reason"] != "waiting for review" || card["column"] != "doing" {
		t.Fatalf("card = %v", card)
	}
	card = w.ok(w.aliceConv, "set_blocked", with(map[string]any{"version": card["version"], "reason": nil}))
	if card["blocked"] != false {
		t.Fatalf("unblocked card = %v", card)
	}
	comment := w.ok(w.aliceConv, "comment_card", with(map[string]any{"body": "Done drafting."}))
	if a := comment["author"].(map[string]any); id(a["id"]) != w.alice || a["agent"] != true {
		t.Fatalf("comment author = %v", a)
	}
	thread := w.ok(w.aliceConv, "show_card", ref)
	if u := thread["updated_by"].(map[string]any); id(u["id"]) != w.alice || u["agent"] != true || len(thread["comments"].([]any)) != 1 {
		t.Fatalf("thread = %v", thread)
	}

	// A card links conversations by the conversation API's ID, and only the user's own.
	w.status(http.StatusBadRequest, w.aliceConv, "update_card", with(map[string]any{"version": thread["version"], "conversations": []string{strconv.FormatInt(w.bobConv, 10)}}))
	w.status(http.StatusBadRequest, w.aliceConv, "update_card", with(map[string]any{"version": thread["version"], "conversations": []string{strconv.FormatInt(w.aliceConv2, 10)}}))
}

// The agent sets, reads, filters and sorts by a card's due date and labels, with the same
// checks as the API.
func TestCardDueDateAndLabels(t *testing.T) {
	w := newWorld(t)
	board := id(w.ok(w.aliceConv, "list_boards", map[string]any{"space_id": w.alicePrivate})["boards"].([]any)[0].(map[string]any)["id"])
	space := map[string]any{"space_id": w.alicePrivate, "board_id": board}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for _, m := range []map[string]any{space, extra} {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	a := w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "a", "due": "2026-03-10", "labels": []string{"Bug", "urgent"}}))
	b := w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "b", "due": "2026-02-01", "labels": []string{"bug"}}))
	w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "c"}))
	if a["due"] != "2026-03-10" || fmt.Sprint(a["labels"]) != "[bug urgent]" {
		t.Fatalf("created card = %v", a)
	}
	titles := func(out map[string]any) string {
		var got []string
		for _, c := range out["cards"].([]any) {
			got = append(got, c.(map[string]any)["title"].(string))
		}
		return fmt.Sprint(got)
	}
	for _, tt := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"labels": []string{"BUG"}}, "[a b]"},
		{map[string]any{"labels": []string{"bug", "urgent"}}, "[a]"},
		{map[string]any{"due_before": "2026-02-28"}, "[b]"},
		{map[string]any{"due_after": "2026-02-28"}, "[a]"},
		{map[string]any{"overdue": true, "today": "2026-03-01"}, "[b]"},
		{map[string]any{"sort": "due"}, "[b a c]"},
	} {
		if got := titles(w.ok(w.aliceConv, "list_cards", with(tt.args))); got != tt.want {
			t.Errorf("list_cards %v = %s, want %s", tt.args, got, tt.want)
		}
	}
	w.status(http.StatusBadRequest, w.aliceConv, "list_cards", with(map[string]any{"due_before": "soon"}))
	w.status(http.StatusBadRequest, w.aliceConv, "list_cards", with(map[string]any{"sort": "rank"}))
	// The store's limit on filter labels holds for the agent as for the API.
	many := []string{}
	for i := 0; i < 11; i++ {
		many = append(many, fmt.Sprint("l", i))
	}
	w.status(http.StatusBadRequest, w.aliceConv, "list_cards", with(map[string]any{"labels": many}))
	w.ok(w.aliceConv, "list_cards", with(map[string]any{"labels": many[:10]}))
	w.status(http.StatusBadRequest, w.aliceConv, "create_card", with(map[string]any{"title": "x", "due": "2026-02-30"}))
	w.status(http.StatusBadRequest, w.aliceConv, "create_card", with(map[string]any{"title": "x", "labels": []string{""}}))

	ref := with(map[string]any{"card_id": id(b["id"])})
	update := func(extra map[string]any) map[string]any {
		out := map[string]any{"version": b["version"]}
		for k, v := range ref {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return w.ok(w.aliceConv, "update_card", out)
	}
	b = update(map[string]any{"due": nil, "labels": []string{"docs"}})
	if b["due"] != nil || fmt.Sprint(b["labels"]) != "[docs]" {
		t.Fatalf("updated card = %v", b)
	}
	shown := w.ok(w.aliceConv, "show_card", ref)
	if shown["due"] != nil || fmt.Sprint(shown["labels"]) != "[docs]" {
		t.Fatalf("show_card = %v", shown)
	}
	// A stale version still fails, whatever the change.
	w.status(http.StatusConflict, w.aliceConv, "update_card", with(map[string]any{"card_id": id(b["id"]), "version": 1, "due": "2027-01-01"}))
}

// Access is asked again on every call, so a change applies to the next one.
func TestAccessIsCheckedOnEveryCall(t *testing.T) {
	w := newWorld(t)
	put := func(conv int64, path string) (int, map[string]any) {
		return w.call(conv, "write_doc", map[string]any{"space_id": w.shared, "path": path, "content": "x"})
	}
	if code, out := put(w.aliceConv, "a.md"); code != http.StatusOK {
		t.Fatalf("alice, an editor: %d %v", code, out)
	}
	// bob only views the shared space: he reads, and can't write.
	w.ok(w.bobConv, "read_doc", map[string]any{"space_id": w.shared, "path": "a.md"})
	if code, _ := put(w.bobConv, "b.md"); code != http.StatusForbidden {
		t.Fatalf("bob, a viewer: %d", code)
	}
	board := w.insert(`INSERT INTO boards (space_id, kind, name) VALUES (?, 'team', 'Team') RETURNING id`, w.shared)
	w.status(http.StatusForbidden, w.bobConv, "create_card", map[string]any{"space_id": w.shared, "board_id": board, "title": "x"})

	// The space archived: read-only for everyone, at once.
	w.exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.shared)
	if code, _ := put(w.aliceConv, "c.md"); code != http.StatusForbidden {
		t.Fatalf("archived space: %d", code)
	}
	w.status(http.StatusForbidden, w.aliceConv, "create_card", map[string]any{"space_id": w.shared, "board_id": board, "title": "x"})
	w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": w.shared, "path": "a.md"})
	w.exec(`UPDATE spaces SET archived_at = NULL WHERE id = ?`, w.shared)

	// Alice taken off the space: not found, at once.
	w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared, w.alice)
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", map[string]any{"space_id": w.shared, "path": "a.md"})

	// Her conversation archived: no tools at all; back once unarchived.
	w.exec(`UPDATE conversations SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.aliceConv)
	w.status(http.StatusForbidden, w.aliceConv, "list_spaces", map[string]any{})
	w.exec(`UPDATE conversations SET archived_at = NULL WHERE id = ?`, w.aliceConv)
	w.ok(w.aliceConv, "list_spaces", map[string]any{})

	// Alice out of the workspace: her conversations there can't do anything, those in her
	// other workspace still can.
	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.one, w.alice)
	w.status(http.StatusForbidden, w.aliceConv, "list_spaces", map[string]any{})
	w.status(http.StatusForbidden, w.aliceConv, "read_doc", map[string]any{"space_id": w.alicePrivate, "path": "a.md"})
	w.ok(w.aliceConv2, "list_spaces", map[string]any{})
}

func TestResultsAreBounded(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// A long document is read in pieces that add up to it.
	var long strings.Builder
	for long.Len() < 100<<10 {
		long.WriteString("Ünïcode line, to cut at a character boundary.\n")
	}
	w.ok(w.aliceConv, "write_doc", map[string]any{"space_id": w.alicePrivate, "path": "long.md", "content": long.String()})
	var read strings.Builder
	offset, pieces := 0, 0
	for {
		out := w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": w.alicePrivate, "path": "long.md", "offset": offset})
		piece := out["content"].(string)
		if len(piece) > readChunk {
			t.Fatalf("a piece of %d bytes", len(piece))
		}
		read.WriteString(piece)
		pieces++
		next, more := out["next_offset"]
		if !more {
			break
		}
		offset = int(next.(float64))
	}
	if read.String() != long.String() || pieces < 4 {
		t.Fatalf("read %d bytes in %d pieces, want %d", read.Len(), pieces, long.Len())
	}

	// A card with a long thread shows the newest comments that fit, each cut.
	a := boards.Actor{UserID: w.alice}
	card, err := w.tools.o.Boards.CreateCard(ctx, a, w.alicePrivate, w.alicePersonal, boards.NewCard{Title: "Long", Body: strings.Repeat("b", 30<<10)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if _, err := w.tools.o.Boards.Comment(ctx, a, w.alicePrivate, w.alicePersonal, card.ID, fmt.Sprintf("comment %d %s", i, strings.Repeat("c", 20<<10))); err != nil {
			t.Fatal(err)
		}
	}
	thread := w.ok(w.aliceConv, "show_card", map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal, "card_id": card.ID})
	comments := thread["comments"].([]any)
	omitted := int(thread["older_comments_omitted"].(float64))
	if len(comments)+omitted != 60 || len(comments) == 0 || omitted == 0 {
		t.Fatalf("%d comments shown, %d omitted", len(comments), omitted)
	}
	if last := comments[len(comments)-1].(map[string]any)["body"].(string); !strings.HasPrefix(last, "comment 59 ") || len(last) > commentLen+len(truncatedNotes) {
		t.Fatalf("the last comment shown is %.20q, %d bytes", last, len(last))
	}
	if body := thread["body"].(string); len(body) > threadBodyLen+len(truncatedNotes) || !strings.HasSuffix(body, truncatedNotes) {
		t.Fatalf("the body is %d bytes", len(body))
	}

	// Many cards: bodies cut, and the list shortened to fit.
	for i := range 150 {
		if _, err := w.tools.o.Boards.CreateCard(ctx, a, w.alicePrivate, w.alicePersonal, boards.NewCard{
			Title: fmt.Sprintf("Card %d %s", i, strings.Repeat("t", 150)), Body: strings.Repeat("b", 2000),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list := w.ok(w.aliceConv, "list_cards", map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal})
	if cards := list["cards"].([]any); list["truncated"] != true || len(cards) == 0 || len(cards) >= 151 {
		t.Fatalf("list_cards: %d cards, truncated %v", len(cards), list["truncated"])
	}
	for _, c := range list["cards"].([]any) {
		if body := c.(map[string]any)["body"].(string); len(body) > listBodyLen+len(truncatedNotes) {
			t.Fatalf("a body of %d bytes in the list", len(body))
		}
	}
}

func TestOversizedResultsAreRefused(t *testing.T) {
	w := newWorld(t)
	w.tools.tools["big"] = func(context.Context, *call, json.RawMessage) (any, error) {
		return map[string]string{"text": strings.Repeat("x", MaxResult)}, nil
	}
	out := w.status(http.StatusRequestEntityTooLarge, w.aliceConv, "big", map[string]any{})
	if !strings.Contains(out["error"].(string), "ask for less") {
		t.Fatalf("error = %v", out)
	}
}
