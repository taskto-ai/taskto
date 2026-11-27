package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apps"
)

func ledger(t *testing.T, board int64, space int64) map[string]any {
	t.Helper()
	def := expenses(t)
	def["definition_version"] = 2
	def["actions"] = map[string]any{
		"add_expense": map[string]any{"kind": "insert", "effects": []any{map[string]any{
			"kind": "create_card", "name": "review", "space_id": space, "board_id": board, "title": "Check {{description}}",
		}}},
		"edit_expense":   map[string]any{"kind": "update"},
		"delete_expense": map[string]any{"kind": "delete"},
	}
	return def
}

func (w *world) publish(app int64) {
	w.t.Helper()
	w.exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT id, 1, definition, json_extract(definition, '$.definition_version'), json_extract(definition, '$.requires') FROM apps WHERE id = ?`, app)
	w.exec(`UPDATE apps SET live_release = 1, live_at = updated_at, draft_differs = 0 WHERE id = ?`, app)
}

type team struct {
	*world
	app, board         int64
	dave, erin         int64
	daveConv, erinConv int64
	ref                map[string]any
}

func newTeam(t *testing.T) *team {
	t.Helper()
	w := newWorld(t)
	tm := &team{world: w}
	for _, p := range []struct {
		name string
		id   *int64
		conv *int64
	}{{"dave", &tm.dave, &tm.daveConv}, {"erin", &tm.erin, &tm.erinConv}} {
		*p.id = w.insert(`INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x') RETURNING id`, p.name+"@example.com", p.name)
		w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.one, *p.id)
		w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, w.shared, *p.id)
		*p.conv = w.conversation(w.one, *p.id)
	}
	w.exec(`UPDATE space_members SET role = 'editor' WHERE space_id = ? AND user_id = ?`, w.shared, w.bob)
	tm.board = w.insert(`INSERT INTO boards (space_id, kind, name) VALUES (?, 'team', 'Team') RETURNING id`, w.shared)
	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Ledger", "definition": ledger(t, tm.board, w.shared)})
	tm.app = id(created["id"])
	w.publish(tm.app)
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'data_manager'), (?, ?, 'user'), (?, ?, 'reader'), (?, ?, 'builder')`,
		tm.app, w.alice, tm.app, w.bob, tm.app, tm.dave, tm.app, tm.erin)
	tm.ref = map[string]any{"space_id": w.shared, "app_id": tm.app}
	return tm
}

func (tm *team) args(more map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range tm.ref {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func (tm *team) run(more map[string]any) map[string]any {
	out := tm.args(more)
	if _, has := out["expected_release"]; !has {
		out["expected_release"] = 1
	}
	return out
}

func (tm *team) add(conv int64, description string, amount float64) map[string]any {
	tm.t.Helper()
	return tm.ok(conv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": description, "amount": amount}}))
}

func TestRunAppActionHonoursTheUsersRoles(t *testing.T) {
	tm := newTeam(t)

	added := tm.add(tm.bobConv, "Bob's coffee", 4.5)
	bobsRow := id(added["id"])
	if added["action"] != "add_expense" || added["kind"] != "insert" || id(added["revision"]) != 1 || added["changed"] != true || id(added["effects_queued"]) != 1 || bobsRow == 0 {
		t.Fatalf("insert = %v", added)
	}
	if _, has := added["data"]; has {
		t.Fatalf("the answer carries the record: %v", added)
	}
	var creator, agent int64
	var action string
	if err := tm.db.QueryRow(`SELECT created_by, created_by_agent, created_action FROM app_rows WHERE id = ?`, bobsRow).Scan(&creator, &agent, &action); err != nil ||
		creator != tm.bob || agent != 1 || action != "add_expense" {
		t.Fatalf("row = %d %d %q, %v", creator, agent, action, err)
	}
	var history int
	if err := tm.db.QueryRow(`SELECT count(*) FROM app_row_history WHERE row_id = ? AND operation = 'insert' AND actor = ? AND actor_agent = 1 AND action = 'add_expense'`, bobsRow, tm.bob).Scan(&history); err != nil || history != 1 {
		t.Fatalf("history = %d, %v", history, err)
	}
	var effects int
	if err := tm.db.QueryRow(`SELECT count(*) FROM app_effects WHERE record_id = ? AND user_id = ? AND acted_by_agent = 1`, bobsRow, tm.bob).Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("effects = %d, %v", effects, err)
	}
	aliceRow := id(tm.add(tm.aliceConv, "Team lunch", 60)["id"])

	updated := tm.ok(tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": bobsRow, "expected_revision": 1, "context": map[string]any{"amount": 5}}))
	if id(updated["revision"]) != 2 || updated["changed"] != true || id(updated["id"]) != bobsRow {
		t.Fatalf("update = %v", updated)
	}
	stale := tm.status(http.StatusConflict, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": bobsRow, "expected_revision": 1, "context": map[string]any{"amount": 6}}))
	if !strings.Contains(stale["error"].(string), "changed since you read it") {
		t.Fatalf("stale = %v", stale)
	}
	same := tm.ok(tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": bobsRow, "context": map[string]any{"amount": 5}}))
	if same["changed"] != false || id(same["revision"]) != 2 {
		t.Fatalf("an update that changes nothing = %v", same)
	}

	for _, args := range []map[string]any{
		{"action": "edit_expense", "id": aliceRow, "context": map[string]any{"amount": 1}},
		{"action": "delete_expense", "id": aliceRow},
	} {
		out := tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(args))
		if !strings.Contains(out["error"].(string), "has no row") {
			t.Fatalf("another's record: %v", out)
		}
	}
	if got := tm.amountOf(aliceRow); got != 60 {
		t.Fatalf("another's record changed to %v", got)
	}

	if out := tm.ok(tm.aliceConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": bobsRow, "context": map[string]any{"amount": 7}})); id(out["revision"]) != 3 {
		t.Fatalf("data manager's update = %v", out)
	}
	deleted := tm.ok(tm.aliceConv, "run_app_action", tm.run(map[string]any{"action": "delete_expense", "id": bobsRow}))
	if deleted["kind"] != "delete" || id(deleted["revision"]) != 4 {
		t.Fatalf("delete = %v", deleted)
	}
	var deletedAt any
	var deletedBy int64
	if err := tm.db.QueryRow(`SELECT deleted_at, deleted_by FROM app_rows WHERE id = ?`, bobsRow).Scan(&deletedAt, &deletedBy); err != nil || deletedAt == nil || deletedBy != tm.alice {
		t.Fatalf("deleted = %v %v, %v", deletedAt, deletedBy, err)
	}
	tm.status(http.StatusBadRequest, tm.aliceConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": bobsRow, "context": map[string]any{"amount": 8}}))

	rows := tm.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, tm.app)
	for _, conv := range []int64{tm.daveConv, tm.erinConv} {
		tm.status(http.StatusForbidden, conv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}}))
		tm.status(http.StatusForbidden, conv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": aliceRow, "context": map[string]any{"amount": 1}}))
		tm.status(http.StatusForbidden, conv, "run_app_action", tm.run(map[string]any{"action": "delete_expense", "id": aliceRow}))
	}
	if n := tm.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, tm.app); n != rows || tm.amountOf(aliceRow) != 60 {
		t.Fatalf("a refused call wrote: %d rows, was %d", n, rows)
	}
	if n := tm.count(`SELECT count(*) FROM app_effects WHERE record_id NOT IN (?, ?)`, bobsRow, aliceRow); n != 0 {
		t.Fatalf("a refused call queued %d effects", n)
	}

	tm.status(http.StatusBadRequest, tm.daveConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "role": "data_manager"}))
}

func (tm *team) count(q string, args ...any) int {
	tm.t.Helper()
	return int(tm.insert(q, args...))
}

func (tm *team) amountOf(row int64) float64 {
	tm.t.Helper()
	var data string
	if err := tm.db.QueryRow(`SELECT data FROM app_rows WHERE id = ?`, row).Scan(&data); err != nil {
		tm.t.Fatal(err)
	}
	var fields struct {
		Amount float64 `json:"amount"`
	}
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		tm.t.Fatal(err)
	}
	return fields.Amount
}

func TestRunAppActionKeepsTheCollectionsRules(t *testing.T) {
	tm := newTeam(t)
	for name, context := range map[string]map[string]any{
		"a required field left out": {"description": "x"},
		"an unknown field":          {"description": "x", "amount": 1, "note": "y"},
		"the wrong type":            {"description": "x", "amount": "lots"},
		"too long":                  {"description": strings.Repeat("x", 201), "amount": 1},
	} {
		out := tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": context}))
		if out["error"] == "" {
			t.Errorf("%s: %v", name, out)
		}
	}
	if n := tm.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, tm.app); n != 0 {
		t.Fatalf("%d rows written", n)
	}
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "nope", "context": map[string]any{}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"context": map[string]any{}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "id": 1, "context": map[string]any{"description": "x", "amount": 1}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "id": 1, "context": map[string]any{"id": 1}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "edit_expense", "context": map[string]any{"amount": 1}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "delete_expense", "id": 1, "context": map[string]any{"amount": 1}}))
	tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}, "operation": "short"}))
}

func TestRunAppActionOperationIsIdempotent(t *testing.T) {
	tm := newTeam(t)
	args := tm.run(map[string]any{"action": "add_expense", "operation": "agent-op-0001", "context": map[string]any{"description": "Taxi", "amount": 12}})
	first := tm.ok(tm.bobConv, "run_app_action", args)
	again := tm.ok(tm.bobConv, "run_app_action", args)
	if again["replayed"] != true || again["id"] != nil || id(first["id"]) == 0 {
		t.Fatalf("first %v, again %v", first, again)
	}
	if n := tm.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, tm.app); n != 1 {
		t.Fatalf("%d rows", n)
	}
	if n := tm.count(`SELECT count(*) FROM app_effects WHERE app_id = ?`, tm.app); n != 1 {
		t.Fatalf("%d effects", n)
	}
	args["context"] = map[string]any{"description": "Taxi home", "amount": 13}
	tm.status(http.StatusConflict, tm.bobConv, "run_app_action", args)
	other := tm.run(map[string]any{"action": "add_expense", "operation": "agent-op-0001", "context": map[string]any{"description": "Taxi", "amount": 12}})
	if out := tm.ok(tm.aliceConv, "run_app_action", other); out["replayed"] != nil || id(out["id"]) == 0 {
		t.Fatalf("alice's = %v", out)
	}
}

func TestListAppActionsSaysWhatTheUserMayRun(t *testing.T) {
	tm := newTeam(t)
	allowed := func(conv int64) map[string]bool {
		out := tm.ok(conv, "list_app_actions", tm.ref)
		if id(out["release"]) != 1 || out["collection"] == nil || out["access"] == nil {
			t.Fatalf("list_app_actions = %v", out)
		}
		got := map[string]bool{}
		for _, a := range out["actions"].([]any) {
			a := a.(map[string]any)
			got[a["name"].(string)] = a["allowed"] == true
			if a["name"] == "add_expense" {
				if fx := a["effects"].([]any); len(fx) != 1 || fx[0] != "create_card" {
					t.Fatalf("effects = %v", fx)
				}
			}
		}
		return got
	}
	for conv, want := range map[int64]map[string]bool{
		tm.aliceConv: {"add_expense": true, "edit_expense": true, "delete_expense": true},
		tm.bobConv:   {"add_expense": true, "edit_expense": true, "delete_expense": true},
		tm.daveConv:  {"add_expense": false, "edit_expense": false, "delete_expense": false},
		tm.erinConv:  {"add_expense": false, "edit_expense": false, "delete_expense": false},
	} {
		got := allowed(conv)
		for name, ok := range want {
			if got[name] != ok {
				t.Errorf("conversation %d, %s: allowed = %v, want %v (%v)", conv, name, got[name], ok, got)
			}
		}
	}
	tm.exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, tm.shared, tm.alice)
	for name, ok := range allowed(tm.aliceConv) {
		if ok {
			t.Errorf("a viewer of the space may run %s", name)
		}
	}
	tm.status(http.StatusForbidden, tm.aliceConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}}))
}

func TestAppActionToolsAreIsolated(t *testing.T) {
	tm := newTeam(t)
	row := id(tm.add(tm.aliceConv, "Lunch", 10)["id"])

	bobs := id(tm.ok(tm.bobConv, "create_app", map[string]any{"space_id": tm.bobPrivate, "name": "Bob's", "definition": expenses(t)})["id"])
	tm.publish(bobs)
	missing := tm.bobPrivate + 1000
	for _, target := range []struct{ space, app int64 }{{tm.bobPrivate, bobs}, {tm.alicePrivate2, 1}, {missing, 1}} {
		for tool, args := range map[string]map[string]any{
			"list_app_actions": {},
			"run_app_action":   {"expected_release": 1, "action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}},
		} {
			args["space_id"], args["app_id"] = target.space, target.app
			if out := tm.status(http.StatusNotFound, tm.aliceConv, tool, args); out["error"] != "not found" {
				t.Errorf("%s in space %d: %v", tool, target.space, out)
			}
		}
	}
	tm.status(http.StatusNotFound, tm.aliceConv, "run_app_action", map[string]any{"space_id": tm.alicePrivate, "app_id": bobs, "expected_release": 1, "action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}})
	tm.status(http.StatusNotFound, tm.aliceConv2, "run_app_action", tm.run(map[string]any{"action": "delete_expense", "id": row}))
	tm.status(http.StatusNotFound, tm.aliceConv2, "list_app_actions", tm.ref)
	tm.exec(`DELETE FROM app_roles WHERE app_id = ? AND user_id = ?`, tm.app, tm.dave)
	tm.status(http.StatusNotFound, tm.daveConv, "list_app_actions", tm.ref)
	tm.status(http.StatusNotFound, tm.daveConv, "run_app_action", tm.run(map[string]any{"action": "delete_expense", "id": row}))
	if n := tm.count(`SELECT count(*) FROM app_rows WHERE app_id IN (?, ?) AND deleted_at IS NULL`, tm.app, bobs); n != 1 {
		t.Fatalf("%d live rows, want alice's one", n)
	}
}

func TestRunAppActionNeedsALiveApp(t *testing.T) {
	tm := newTeam(t)
	draft := id(tm.ok(tm.aliceConv, "create_app", map[string]any{"space_id": tm.shared, "name": "Draft", "definition": expenses(t)})["id"])
	args := map[string]any{"space_id": tm.shared, "app_id": draft, "expected_release": 1, "action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}}
	tm.status(http.StatusConflict, tm.aliceConv, "run_app_action", args)
	tm.status(http.StatusConflict, tm.aliceConv, "list_app_actions", map[string]any{"space_id": tm.shared, "app_id": draft})
	tm.exec(`UPDATE apps SET archived_at = updated_at WHERE id = ?`, tm.app)
	tm.status(http.StatusConflict, tm.aliceConv, "run_app_action", tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}}))
	if n := tm.count(`SELECT count(*) FROM app_rows`); n != 0 {
		t.Fatalf("%d rows", n)
	}
}

func TestRunAppActionLeavesCaptureFieldsToPeople(t *testing.T) {
	w := newWorld(t)
	def := map[string]any{
		"definition_version": 2,
		"components":         []any{map[string]any{"id": "root", "component": "Text", "text": "x"}},
		"collection": map[string]any{"fields": []any{
			map[string]any{"name": "equipment", "type": "string", "required": true},
			map[string]any{"name": "place", "type": "location"},
		}},
		"actions": map[string]any{
			"check_out": map[string]any{"kind": "insert"},
			"fix":       map[string]any{"kind": "update"},
		},
	}
	app := id(w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Gear", "definition": def})["id"])
	w.publish(app)
	ref := map[string]any{"space_id": w.alicePrivate, "app_id": app}
	place := map[string]any{"lat": 1.5, "lon": 2.5, "accuracy_m": 5, "captured_at": "2026-01-01T10:00:00Z", "source": "manual"}
	out := w.status(http.StatusBadRequest, w.aliceConv, "run_app_action", map[string]any{"space_id": ref["space_id"], "app_id": app, "expected_release": 1, "action": "check_out", "context": map[string]any{"equipment": "drill", "place": place}})
	if !strings.Contains(out["error"].(string), "only a person can capture") {
		t.Fatalf("refusal = %v", out)
	}
	row := id(w.ok(w.aliceConv, "run_app_action", map[string]any{"space_id": ref["space_id"], "app_id": app, "expected_release": 1, "action": "check_out", "context": map[string]any{"equipment": "drill"}})["id"])
	w.exec(`UPDATE app_rows SET data = json_set(data, '$.place', json(?)) WHERE id = ?`, `{"lat":1.5,"lon":2.5,"accuracy_m":5,"captured_at":"2026-01-01T10:00:00Z","source":"device"}`, row)
	w.status(http.StatusBadRequest, w.aliceConv, "run_app_action", map[string]any{"space_id": ref["space_id"], "app_id": app, "expected_release": 1, "action": "fix", "id": row, "context": map[string]any{"place": place}})
	w.ok(w.aliceConv, "run_app_action", map[string]any{"space_id": ref["space_id"], "app_id": app, "expected_release": 1, "action": "fix", "id": row, "context": map[string]any{"equipment": "big drill"}})
	var kept string
	if err := w.db.QueryRow(`SELECT json_extract(data, '$.place.source') FROM app_rows WHERE id = ?`, row).Scan(&kept); err != nil || kept != "device" {
		t.Fatalf("the captured position = %q, %v", kept, err)
	}
}

func TestRunAppActionOverMCPNamesTheToken(t *testing.T) {
	tm := newTeam(t)
	args, _ := json.Marshal(tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "Train", "amount": 20}}))
	out, err := tm.tools.Call(context.Background(), Caller{Workspace: tm.one, User: tm.bob, Token: "my laptop"}, "run_app_action", args)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	var via string
	var agent bool
	if err := tm.db.QueryRow(`SELECT created_via, created_by_agent FROM app_rows WHERE id = ?`, id(res["id"])).Scan(&via, &agent); err != nil || via != "my laptop" || !agent {
		t.Fatalf("via = %q, agent = %v, %v", via, agent, err)
	}
	if _, err := tm.tools.Call(context.Background(), Caller{Workspace: tm.one, User: tm.dave, Token: "t"}, "run_app_action", args); err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("a reader over MCP: %v", err)
	}
}

func TestAppActionToolsAskOnlyToWrite(t *testing.T) {
	for _, d := range Definitions() {
		switch d.Name {
		case "run_app_action":
			if d.ReadOnly || d.ConversationOnly {
				t.Errorf("run_app_action: read_only = %v, conversation_only = %v", d.ReadOnly, d.ConversationOnly)
			}
		case "list_app_actions":
			if !d.ReadOnly {
				t.Error("list_app_actions changes nothing and should be read_only")
			}
		}
	}
}

func (tm *team) publishTwo() {
	tm.t.Helper()
	tm.exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT id, 2, json_remove(definition, '$.actions.add_expense.effects'), json_extract(definition, '$.definition_version'), json_extract(definition, '$.requires') FROM apps WHERE id = ?`, tm.app)
	tm.exec(`UPDATE apps SET live_release = 2, live_at = updated_at WHERE id = ?`, tm.app)
}

func TestRunAppActionIsBoundToTheListedRelease(t *testing.T) {
	tm := newTeam(t)
	if out := tm.ok(tm.bobConv, "list_app_actions", tm.ref); id(out["release"]) != 1 {
		t.Fatalf("listed = %v", out)
	}
	first := tm.run(map[string]any{"action": "add_expense", "operation": "agent-op-0001", "context": map[string]any{"description": "Taxi", "amount": 12}})
	if out := tm.ok(tm.bobConv, "run_app_action", first); id(out["effects_queued"]) != 1 {
		t.Fatalf("first = %v", out)
	}
	tm.publishTwo()
	rows, effects := tm.count(`SELECT count(*) FROM app_rows`), tm.count(`SELECT count(*) FROM app_effects`)

	if again := tm.ok(tm.bobConv, "run_app_action", first); again["replayed"] != true {
		t.Fatalf("retry = %v", again)
	}
	stale := tm.run(map[string]any{"action": "add_expense", "operation": "agent-op-0002", "context": map[string]any{"description": "Bus", "amount": 3}})
	out := tm.status(http.StatusConflict, tm.bobConv, "run_app_action", stale)
	if !strings.Contains(out["error"].(string), "list_app_actions") {
		t.Fatalf("stale = %v", out)
	}
	delete(stale, "operation")
	tm.status(http.StatusConflict, tm.bobConv, "run_app_action", stale)
	stale["expected_release"] = 7
	tm.status(http.StatusConflict, tm.bobConv, "run_app_action", stale)
	other := tm.run(map[string]any{"action": "add_expense", "operation": "agent-op-0001", "context": map[string]any{"description": "Taxi home", "amount": 13}})
	tm.status(http.StatusConflict, tm.bobConv, "run_app_action", other)
	if n := tm.count(`SELECT count(*) FROM app_rows`); n != rows {
		t.Fatalf("%d rows written by refused calls", n-rows)
	}
	if n := tm.count(`SELECT count(*) FROM app_effects`); n != effects {
		t.Fatalf("%d effects queued by refused calls", n-effects)
	}

	if out := tm.ok(tm.bobConv, "list_app_actions", tm.ref); id(out["release"]) != 2 {
		t.Fatalf("listed again = %v", out)
	}
	fresh := tm.ok(tm.bobConv, "run_app_action", tm.run(map[string]any{"expected_release": 2, "action": "add_expense", "context": map[string]any{"description": "Bus", "amount": 3}}))
	if id(fresh["id"]) == 0 || id(fresh["effects_queued"]) != 0 {
		t.Fatalf("fresh = %v", fresh)
	}
	if n := tm.count(`SELECT count(*) FROM app_effects`); n != effects {
		t.Fatalf("%d effects queued by release 2", n-effects)
	}
}

func TestRunAppActionRefusesAmbiguousArguments(t *testing.T) {
	tm := newTeam(t)
	good := map[string]any{"action": "add_expense", "context": map[string]any{"description": "x", "amount": 1}}
	with := func(k string, v any) map[string]any {
		out := tm.args(map[string]any{"expected_release": 1})
		for key, val := range good {
			out[key] = val
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	for name, args := range map[string]map[string]any{
		"no expected_release":   with("expected_release", nil),
		"expected_release zero": with("expected_release", 0),
		"an empty operation":    with("operation", ""),
		"id in context, insert": with("context", map[string]any{"description": "x", "amount": 1, "id": 4}),
		"id in context, no id":  {"space_id": tm.shared, "app_id": tm.app, "expected_release": 1, "action": "edit_expense", "context": map[string]any{"id": 4}},
		"a null id in context":  with("context", map[string]any{"description": "x", "amount": 1, "id": nil}),
	} {
		tm.status(http.StatusBadRequest, tm.bobConv, "run_app_action", args)
		_ = name
	}
	if n := tm.count(`SELECT count(*) FROM app_rows`); n != 0 {
		t.Fatalf("%d rows written", n)
	}
	tm.ok(tm.bobConv, "run_app_action", with("operation", nil))
	for _, d := range Definitions() {
		if d.Name != "run_app_action" {
			continue
		}
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(schema.Required, ","), "expected_release") || !strings.Contains(string(schema.Properties["context"]), `"not":{"required":["id"]}`) &&
			!strings.Contains(strings.Join(strings.Fields(string(schema.Properties["context"])), ""), `"not":{"required":["id"]}`) {
			t.Fatalf("schema = %s", d.InputSchema)
		}
	}
}

func (tm *team) freezeTokenBudget() {
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	tm.apps.WithClock(func() time.Time { return at })
}

func TestRunAppActionOverATokensBudgetSaysWhenToRetry(t *testing.T) {
	tm := newTeam(t)
	tm.freezeTokenBudget()
	caller := Caller{Workspace: tm.one, User: tm.bob, Token: "laptop"}
	raw, err := json.Marshal(tm.run(map[string]any{"action": "add_expense", "context": map[string]any{"description": "coffee", "amount": 1}}))
	if err != nil {
		t.Fatal(err)
	}
	for i := range apps.TokenActionBurst {
		if _, err := tm.tools.Call(context.Background(), caller, "run_app_action", raw); err != nil {
			t.Fatalf("action %d of the burst: %v", i+1, err)
		}
	}
	_, err = tm.tools.Call(context.Background(), caller, "run_app_action", raw)
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
		t.Fatalf("over the budget: %v", err)
	}
	if !regexp.MustCompile(`try again in \d+ seconds?`).MatchString(refused.Message) {
		t.Errorf("the refusal %q doesn't say when to try again", refused.Message)
	}
}

func TestRunAppActionReplaysACommittedOperationOverTheBudget(t *testing.T) {
	tm := newTeam(t)
	tm.freezeTokenBudget()
	caller := Caller{Workspace: tm.one, User: tm.bob, Token: "laptop"}
	call := func(operation string) (map[string]any, error) {
		raw, err := json.Marshal(tm.run(map[string]any{"action": "add_expense", "operation": operation, "context": map[string]any{"description": "coffee", "amount": 1}}))
		if err != nil {
			t.Fatal(err)
		}
		out, err := tm.tools.Call(context.Background(), caller, "run_app_action", raw)
		var res map[string]any
		if err == nil {
			if jerr := json.Unmarshal(out, &res); jerr != nil {
				t.Fatal(jerr)
			}
		}
		return res, err
	}
	if _, err := call("op-first-0001"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < apps.TokenActionBurst; i++ {
		if _, err := call(fmt.Sprintf("op-fill-%04d", i)); err != nil {
			t.Fatalf("action %d of the burst: %v", i+1, err)
		}
	}
	var refused *Refusal
	if _, err := call("op-new-0001"); !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
		t.Fatalf("a new operation over the budget: %v", err)
	}
	res, err := call("op-first-0001")
	if err != nil || res["replayed"] != true || res["kind"] != "insert" {
		t.Fatalf("the committed operation over the budget = %v, %v; want a replay", res, err)
	}
	var rows int
	if err := tm.db.QueryRow(`SELECT count(*) FROM app_rows WHERE app_id = ?`, tm.app).Scan(&rows); err != nil || rows != apps.TokenActionBurst {
		t.Errorf("%d rows, %v; want one per operation that was allowed (%d)", rows, err, apps.TokenActionBurst)
	}
}
