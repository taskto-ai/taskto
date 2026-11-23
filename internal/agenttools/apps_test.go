package agenttools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apps"
)

// expenses is the bundled example, as the agent would send it.
func expenses(t *testing.T) map[string]any {
	t.Helper()
	ex, err := apps.Examples()
	if err != nil {
		t.Fatal(err)
	}
	var def map[string]any
	raw, _ := json.Marshal(ex[0].Definition)
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	return def
}

func TestAppToolsBuildAppsForTheirUser(t *testing.T) {
	w := newWorld(t)
	ref := w.ok(w.aliceConv, "app_catalog", map[string]any{})
	if _, ok := ref["components"].(map[string]any)["Stat"]; !ok {
		t.Fatalf("app_catalog = %v", ref)
	}
	if _, ok := ref["components"].(map[string]any)["Image"]; ok {
		t.Fatal("app_catalog offers Image")
	}

	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Expenses", "definition": expenses(t)})
	by := created["created_by"].(map[string]any)
	if id(by["id"]) != w.alice || by["agent"] != true || created["draft"] == nil || created["live"] != nil || created["unpublished_changes"] != true {
		t.Fatalf("create_app = %v", created)
	}
	app := id(created["id"])
	list := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.alicePrivate})["apps"].([]any)
	if len(list) != 1 || id(list[0].(map[string]any)["id"]) != app {
		t.Fatalf("list_apps = %v", list)
	}
	shown := w.ok(w.aliceConv, "show_app", map[string]any{"space_id": w.alicePrivate, "app_id": app})
	if shown["name"] != "Expenses" {
		t.Fatalf("show_app = %v", shown)
	}

	// Updating: a rename and a new definition, checked against the catalog like any other.
	def := expenses(t)
	def["components"] = append(def["components"].([]any), map[string]any{"id": "x", "component": "Script"})
	out := w.status(http.StatusBadRequest, w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "definition": def})
	if !strings.Contains(out["error"].(string), `"Script" is not a component`) {
		t.Fatalf("refusal = %v", out)
	}
	updated := w.ok(w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "name": "Spending"})
	if updated["name"] != "Spending" || id(updated["version"]) != 2 {
		t.Fatalf("update_app = %v", updated)
	}
	w.status(http.StatusConflict, w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "name": "Stale"})
	// Its data: the rows as the app shows them, without fields it no longer declares.
	w.exec(`INSERT INTO app_rows (app_id, data) VALUES (?, '{"description": "Coffee", "amount": 4.5, "dropped": "old"}')`, app)
	rows := w.ok(w.aliceConv, "list_app_rows", map[string]any{"space_id": w.alicePrivate, "app_id": app})["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["description"] != "Coffee" || rows[0].(map[string]any)["dropped"] != nil {
		t.Fatalf("list_app_rows = %v", rows)
	}

	// More rows than one reply carries: they are read a page at a time, all of them, once each.
	for i := range 300 {
		w.exec(`INSERT INTO app_rows (app_id, data) VALUES (?, ?)`, app,
			fmt.Sprintf(`{"description": "row %d %s", "amount": 1}`, i, strings.Repeat("x", 400)))
	}
	// Rows added and removed between pages shift nothing: pages go by row id.
	seen := map[int64]bool{}
	pages := 0
	for after := int64(0); ; pages++ {
		page := w.ok(w.aliceConv, "list_app_rows", map[string]any{"space_id": w.alicePrivate, "app_id": app, "after": after})
		for _, r := range page["rows"].([]any) {
			rid := id(r.(map[string]any)["id"])
			if seen[rid] {
				t.Fatalf("row %d read twice", rid)
			}
			seen[rid] = true
		}
		next, more := page["next_after"]
		if !more {
			break
		}
		after = id(next)
		if pages == 0 {
			// Meanwhile, an early row goes, which an offset would have skipped a row over.
			w.exec(`DELETE FROM app_rows WHERE id = (SELECT min(id) FROM app_rows WHERE app_id = ? AND data LIKE '%"row %')`, app)
		}
	}
	if len(seen) != 301 || pages < 1 {
		t.Fatalf("read %d rows in %d extra pages, want 301 in more than one", len(seen), pages)
	}
	w.exec(`DELETE FROM app_rows WHERE app_id = ? AND data LIKE '%"row %'`, app)
	w.status(http.StatusBadRequest, w.aliceConv, "list_app_rows", map[string]any{"space_id": w.alicePrivate, "app_id": app, "after": -1})

	// More apps than one reply lists: read a page at a time, each once.
	var definition string
	if err := w.db.QueryRow(`SELECT definition FROM apps WHERE id = ?`, app).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		w.exec(`INSERT INTO apps (space_id, name, definition) VALUES (?, ?, ?)`, w.alicePrivate,
			fmt.Sprintf("App %d %s", i, strings.Repeat("n", 150)), definition)
	}
	listed := map[int64]bool{}
	pages = 0
	for offset := 0; ; pages++ {
		page := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.alicePrivate, "offset": offset})
		for _, a := range page["apps"].([]any) {
			aid := id(a.(map[string]any)["id"])
			if listed[aid] {
				t.Fatalf("app %d listed twice", aid)
			}
			listed[aid] = true
		}
		next, more := page["next_offset"]
		if !more {
			break
		}
		offset = int(id(next))
	}
	if len(listed) != 401 || pages < 1 {
		t.Fatalf("listed %d apps in %d extra pages, want 401 in more than one", len(listed), pages)
	}
	w.exec(`DELETE FROM apps WHERE space_id = ? AND id != ?`, w.alicePrivate, app)

	// Offline, and back; its rows are kept, and still read.
	w.ok(w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 2, "archived": true})
	if rows := w.ok(w.aliceConv, "list_app_rows", map[string]any{"space_id": w.alicePrivate, "app_id": app})["rows"].([]any); len(rows) != 1 {
		t.Fatalf("an offline app's rows = %v", rows)
	}
	if list := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.alicePrivate})["apps"].([]any); len(list) != 0 {
		t.Fatalf("an offline app is listed: %v", list)
	}
	if list := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.alicePrivate, "archived": true})["apps"].([]any); len(list) != 1 {
		t.Fatalf("with archived: %v", list)
	}

	// Unknown arguments are refused, not ignored.
	w.status(http.StatusBadRequest, w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "x", "definition": expenses(t), "owner": w.bob})
}

func TestAppToolsRespectAccess(t *testing.T) {
	w := newWorld(t)
	bobs := w.ok(w.bobConv, "create_app", map[string]any{"space_id": w.bobPrivate, "name": "Bob's", "definition": expenses(t)})
	elsewhere := w.ok(w.aliceConv2, "create_app", map[string]any{"space_id": w.alicePrivate2, "name": "Elsewhere", "definition": expenses(t)})
	team := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Team", "definition": expenses(t)})

	// Alice's conversation in workspace one reaches neither bob's private space nor her own
	// space of workspace two, nor a space that doesn't exist: all "not found" alike.
	missing := w.alicePrivate2 + 1000
	for _, target := range []struct {
		space, app int64
	}{{w.bobPrivate, id(bobs["id"])}, {w.alicePrivate2, id(elsewhere["id"])}, {missing, 1}} {
		for tool, args := range map[string]map[string]any{
			"list_apps":     {},
			"show_app":      {"app_id": target.app},
			"list_app_rows": {"app_id": target.app},
			"create_app":    {"name": "x", "definition": expenses(t)},
			"update_app":    {"app_id": target.app, "version": 1, "name": "taken"},
		} {
			args["space_id"] = target.space
			if out := w.status(http.StatusNotFound, w.aliceConv, tool, args); out["error"] != "not found" {
				t.Errorf("%s in space %d: %v", tool, target.space, out)
			}
		}
	}
	// An app is found only through its own space: bob's app through alice's private space
	// is not found either.
	w.status(http.StatusNotFound, w.aliceConv, "show_app", map[string]any{"space_id": w.alicePrivate, "app_id": id(bobs["id"])})
	w.status(http.StatusNotFound, w.aliceConv, "list_app_rows", map[string]any{"space_id": w.alicePrivate, "app_id": id(bobs["id"])})
	w.status(http.StatusNotFound, w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": id(bobs["id"]), "version": 1, "name": "x"})

	// Bob views the team space: his agent reads its app and changes nothing.
	if shown := w.ok(w.bobConv, "show_app", map[string]any{"space_id": w.shared, "app_id": id(team["id"])}); shown["name"] != "Team" {
		t.Fatalf("show_app = %v", shown)
	}
	w.ok(w.bobConv, "list_app_rows", map[string]any{"space_id": w.shared, "app_id": id(team["id"])})
	w.status(http.StatusForbidden, w.bobConv, "update_app", map[string]any{"space_id": w.shared, "app_id": id(team["id"]), "version": 1, "archived": true})
	w.status(http.StatusForbidden, w.bobConv, "create_app", map[string]any{"space_id": w.shared, "name": "Bob's team app", "definition": expenses(t)})

	// Nothing changed.
	for _, check := range []struct {
		conv, space int64
		app         map[string]any
	}{{w.bobConv, w.bobPrivate, bobs}, {w.aliceConv2, w.alicePrivate2, elsewhere}, {w.aliceConv, w.shared, team}} {
		got := w.ok(check.conv, "show_app", map[string]any{"space_id": check.space, "app_id": id(check.app["id"])})
		if got["name"] != check.app["name"] || id(got["version"]) != 1 || got["archived_at"] != nil {
			t.Fatalf("changed: %v", got)
		}
	}

	// Access is checked on every call: bob leaves the team space, and his agent loses it.
	w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared, w.bob)
	w.status(http.StatusNotFound, w.bobConv, "show_app", map[string]any{"space_id": w.shared, "app_id": id(team["id"])})
	w.status(http.StatusNotFound, w.bobConv, "list_app_rows", map[string]any{"space_id": w.shared, "app_id": id(team["id"])})
}

func TestAppCatalogFits(t *testing.T) {
	w := newWorld(t)
	code, out := w.call(w.aliceConv, "app_catalog", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("app_catalog = %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > MaxResult/2 {
		t.Fatalf("app_catalog is %d bytes: keep it well under the result bound", len(raw))
	}
}

// The agent has its user's roles in an app: it can't change an app its user may only use,
// reads only the records its user reads, and nothing it writes grants roles.
func TestAppToolsKeepToTheirUsersRoles(t *testing.T) {
	w := newWorld(t)
	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Team expenses", "definition": expenses(t)})
	app := id(created["id"])
	// A record of Alice's own and one of Bob's, a viewer of the space who had added it before.
	w.exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description": "alice-row", "amount": 1}', ?)`, app, w.alice)
	w.exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description": "bob-row", "amount": 2}', ?)`, app, w.bob)
	ref := map[string]any{"space_id": w.shared, "app_id": app}

	// Unrestricted: an editor's agent reads both and shows what its user may do.
	if rows := w.ok(w.aliceConv, "list_app_rows", ref)["rows"].([]any); len(rows) != 2 {
		t.Fatalf("list_app_rows = %v", rows)
	}
	shown := w.ok(w.aliceConv, "show_app", ref)
	if acc := shown["access"].(map[string]any); acc["design"] != true || acc["read_records"] != "all" {
		t.Fatalf("show_app access = %v", acc)
	}

	// Alice becomes a plain user of the app (roles are set by people, in the web app).
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user')`, app, w.alice)
	rows := w.ok(w.aliceConv, "list_app_rows", ref)["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["description"] != "alice-row" {
		t.Fatalf("a user's agent reads %v", rows)
	}
	list := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.shared})["apps"].([]any)
	if n := list[0].(map[string]any)["rows"]; n != float64(1) {
		t.Fatalf("list_apps counts %v rows", n)
	}
	// And its changes to the app are refused, however it asks.
	out := w.status(http.StatusForbidden, w.aliceConv, "update_app", map[string]any{"space_id": w.shared, "app_id": app, "version": 1, "name": "Mine"})
	if !strings.Contains(out["error"].(string), "role") {
		t.Fatalf("refusal = %v", out)
	}
	w.status(http.StatusForbidden, w.aliceConv, "update_app", map[string]any{"space_id": w.shared, "app_id": app, "version": 1, "archived": true})
	w.status(http.StatusForbidden, w.aliceConv, "update_app", map[string]any{"space_id": w.shared, "app_id": app, "version": 1, "definition": expenses(t)})

	// A definition has no roles in it to grant: the tool refuses a field it doesn't know.
	def := expenses(t)
	def["roles"] = []string{"builder"}
	w.status(http.StatusBadRequest, w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Sneaky", "definition": def})
	// Bob, named nowhere, sees nothing of the restricted app until a default names everyone else.
	w.status(http.StatusNotFound, w.bobConv, "list_app_rows", ref)
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, NULL, 'reader')`, app)
	// Bob's agent, a viewer, reads the space's records and changes nothing.
	if rows := w.ok(w.bobConv, "list_app_rows", ref)["rows"].([]any); len(rows) != 2 {
		t.Fatalf("a viewer's agent reads %v", rows)
	}
	w.status(http.StatusForbidden, w.bobConv, "update_app", map[string]any{"space_id": w.shared, "app_id": app, "version": 1, "name": "Bob's"})
	var name string
	if err := w.db.QueryRow(`SELECT name FROM apps WHERE id = ?`, app).Scan(&name); err != nil || name != "Team expenses" {
		t.Fatalf("name = %q, %v", name, err)
	}
}

// The agent edits an app's draft and nothing more: what people use stays the release that was
// published, and it has no tool to publish or roll back, which is for a person who may publish.
func TestAgentEditsOnlyTheDraft(t *testing.T) {
	w := newWorld(t)
	for _, d := range Definitions() {
		if strings.Contains(d.Name, "publish") || strings.Contains(d.Name, "rollback") || strings.Contains(d.Name, "release") {
			t.Fatalf("the agent has a tool to %s", d.Name)
		}
	}
	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Expenses", "definition": expenses(t)})
	app := id(created["id"])
	ref := map[string]any{"space_id": w.alicePrivate, "app_id": app}
	// A person publishes it (the release is made as the server does).
	w.exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT id, 1, definition, 1, json_extract(definition, '$.requires') FROM apps WHERE id = ?`, app)
	w.exec(`UPDATE apps SET live_release = 1, live_at = updated_at, draft_differs = 0 WHERE id = ?`, app)
	var published string
	if err := w.db.QueryRow(`SELECT definition FROM app_releases WHERE app_id = ?`, app).Scan(&published); err != nil {
		t.Fatal(err)
	}

	def := expenses(t)
	def["collection"].(map[string]any)["fields"] = append(def["collection"].(map[string]any)["fields"].([]any),
		map[string]any{"name": "category", "type": "string"})
	updated := w.ok(w.aliceConv, "update_app", map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "definition": def})
	if updated["unpublished_changes"] != true || updated["draft"] == nil {
		t.Fatalf("update_app = %v", updated)
	}
	live := updated["live"].(map[string]any)
	if live["number"] != float64(1) || live["definition"] != nil {
		t.Fatalf("live in update_app = %v", live)
	}
	var after string
	if err := w.db.QueryRow(`SELECT definition FROM app_releases WHERE app_id = ?`, app).Scan(&after); err != nil || after != published {
		t.Fatalf("the release changed: %v", err)
	}
	shown := w.ok(w.aliceConv, "show_app", ref)
	if shown["unpublished_changes"] != true || shown["live"].(map[string]any)["number"] != float64(1) {
		t.Fatalf("show_app = %v", shown)
	}
	raw, _ := json.Marshal(shown)
	if len(raw) > MaxResult/2 {
		t.Fatalf("show_app is %d bytes: it must leave room in a reply", len(raw))
	}
}

// query_app_rows pages, filters and totals over all the rows its user reads, and no others.
func TestQueryAppRowsPagesFiltersAndTotals(t *testing.T) {
	w := newWorld(t)
	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Team expenses", "definition": expenses(t)})
	app := id(created["id"])
	// A person publishes it (the release is made as the server does).
	w.exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT id, 1, definition, json_extract(definition, '$.definition_version'), json_extract(definition, '$.requires') FROM apps WHERE id = ?`, app)
	w.exec(`UPDATE apps SET live_release = 1, live_at = updated_at, draft_differs = 0 WHERE id = ?`, app)
	for i := 1; i <= 9; i++ {
		owner := w.alice
		if i%3 == 0 {
			owner = w.bob
		}
		w.exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`, app,
			fmt.Sprintf(`{"description": "Coffee %d", "amount": %d}`, i, i*10), owner)
	}
	ref := func(more map[string]any) map[string]any {
		args := map[string]any{"space_id": w.shared, "app_id": app}
		for k, v := range more {
			args[k] = v
		}
		return args
	}
	sums := []any{map[string]any{"kind": "sum", "field": "amount"}, map[string]any{"kind": "max", "field": "amount"}}

	// Pages of two, in the order asked for, each with the totals of every row that matches.
	var got []float64
	cursor := ""
	for pages := 0; ; pages++ {
		args := ref(map[string]any{"limit": 2, "sort": "amount", "dir": "desc", "aggregates": sums,
			"filters": []any{map[string]any{"field": "description", "op": "contains", "value": "coffee"}}})
		if cursor != "" {
			args["cursor"] = cursor
		}
		page := w.ok(w.aliceConv, "query_app_rows", args)
		if page["total"] != float64(9) || page["aggregates"].([]any)[0].(map[string]any)["value"] != float64(450) {
			t.Fatalf("totals of page %d = %v", pages, page)
		}
		for _, r := range page["rows"].([]any) {
			got = append(got, r.(map[string]any)["amount"].(float64))
		}
		next, more := page["next_cursor"].(string)
		if !more {
			break
		}
		cursor = next
		if pages > 9 {
			t.Fatal("paging doesn't end")
		}
	}
	if fmt.Sprint(got) != "[90 80 70 60 50 40 30 20 10]" {
		t.Fatalf("rows = %v", got)
	}
	// A cursor of another query is refused, as is a field the app doesn't have.
	w.status(http.StatusBadRequest, w.aliceConv, "query_app_rows", ref(map[string]any{"limit": 2, "sort": "description", "cursor": cursor}))
	w.status(http.StatusBadRequest, w.aliceConv, "query_app_rows", ref(map[string]any{"filters": []any{map[string]any{"field": "nope", "op": "eq", "value": 1}}}))
	w.status(http.StatusBadRequest, w.aliceConv, "query_app_rows", ref(map[string]any{"unknown": true}))

	// Alice becomes a plain user of the app: her agent's totals are of her own rows alone.
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user')`, app, w.alice)
	own := w.ok(w.aliceConv, "query_app_rows", ref(map[string]any{"aggregates": sums, "limit": 200}))
	if own["total"] != float64(6) || own["aggregates"].([]any)[0].(map[string]any)["value"] != float64(270) || len(own["rows"].([]any)) != 6 {
		t.Fatalf("a user's agent gets %v", own)
	}
	// Someone the app names no role for finds no app.
	w.status(http.StatusNotFound, w.bobConv, "query_app_rows", ref(nil))
}
