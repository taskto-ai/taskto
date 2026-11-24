package agenttools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// templateWith returns the id of a bundled template that does (or doesn't) need a board.
func templateWith(t *testing.T, w *world, needsBoard bool) string {
	t.Helper()
	list := w.ok(w.aliceConv, "list_app_templates", map[string]any{})["templates"].([]any)
	for _, v := range list {
		tpl := v.(map[string]any)
		if tpl["needs_board"] == needsBoard {
			return tpl["id"].(string)
		}
	}
	t.Fatalf("no template with needs_board %v in %v", needsBoard, list)
	return ""
}

// Installing a template and copying an app make drafts only: nothing is published, no rows,
// releases or roles of the source come along, and the user's own access decides where.
func TestTemplatesAndCopiesAreDraftsOnly(t *testing.T) {
	w := newWorld(t)
	plain, boarded := templateWith(t, w, false), templateWith(t, w, true)

	got := w.ok(w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": plain, "name": "Mine"})
	if got["name"] != "Mine" || got["draft"] == nil || got["live"] != nil || got["unpublished_changes"] != true {
		t.Fatalf("install_app_template = %v", got)
	}
	if by := got["created_by"].(map[string]any); id(by["id"]) != w.alice || by["agent"] != true {
		t.Fatalf("created_by = %v", by)
	}
	var live int
	if err := w.db.QueryRow(`SELECT count(*) FROM app_releases WHERE app_id = ?`, id(got["id"])).Scan(&live); err != nil || live != 0 {
		t.Fatalf("releases = %d, %v", live, err)
	}
	// The default name, and the template's own rules: a template needing a board names one
	// the user may write, and one that doesn't refuses a board.
	if def := w.ok(w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": plain}); def["name"] == "" {
		t.Fatalf("install_app_template = %v", def)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": boarded})
	w.status(http.StatusBadRequest, w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": plain,
		"board": map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal}})
	w.status(http.StatusBadRequest, w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": "no-such-template"})
	ok := w.ok(w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": boarded,
		"board": map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal}})
	if ok["live"] != nil {
		t.Fatalf("install with a board = %v", ok)
	}
	// Bob's board is not alice's to aim an app at, and a viewer installs nothing.
	w.status(http.StatusBadRequest, w.aliceConv, "install_app_template", map[string]any{"space_id": w.alicePrivate, "template_id": boarded,
		"board": map[string]any{"space_id": w.bobPrivate, "board_id": w.bobPersonal}})
	w.status(http.StatusForbidden, w.bobConv, "install_app_template", map[string]any{"space_id": w.shared, "template_id": plain})

	// A copy: the draft only, with no rows, releases or roles, and its own life.
	src := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Team expenses", "definition": expenses(t)})
	app := id(src["id"])
	w.exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description": "row", "amount": 1}', ?)`, app, w.alice)
	w.status(http.StatusBadRequest, w.aliceConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app, "source": "live"}) // not published
	w.publish(app)
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder')`, app, w.alice)
	cp := w.ok(w.aliceConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app, "name": "Copy", "destination_space_id": w.alicePrivate})
	cid := id(cp["id"])
	if cp["name"] != "Copy" || cid == app || cp["live"] != nil || cp["draft"] == nil || id(cp["space_id"]) != w.alicePrivate {
		t.Fatalf("duplicate_app = %v", cp)
	}
	for q, table := range map[string]string{"app_rows": "app_id", "app_releases": "app_id", "app_roles": "app_id"} {
		var n int
		if err := w.db.QueryRow(`SELECT count(*) FROM `+q+` WHERE `+table+` = ?`, cid).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the copy has %d rows of %s, %v", n, q, err)
		}
	}
	same := w.ok(w.aliceConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app})
	if same["name"] != "Team expenses" || id(same["space_id"]) != w.shared {
		t.Fatalf("duplicate_app in the same space = %v", same)
	}
	// The source is untouched.
	if shown := w.ok(w.aliceConv, "show_app", map[string]any{"space_id": w.shared, "app_id": app}); id(shown["version"]) != 1 {
		t.Fatalf("source = %v", shown)
	}
	// Bob views the space: he may read the live release, but not write a copy there; and his
	// own private space is his to copy into.
	w.status(http.StatusForbidden, w.bobConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app})
	w.status(http.StatusForbidden, w.bobConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app, "source": "draft"})
	w.exec(`DELETE FROM app_roles WHERE app_id = ?`, app)
	mine := w.ok(w.bobConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app, "destination_space_id": w.bobPrivate})
	if id(mine["space_id"]) != w.bobPrivate || mine["live"] != nil {
		t.Fatalf("bob's copy = %v", mine)
	}
	w.status(http.StatusForbidden, w.bobConv, "duplicate_app", map[string]any{"space_id": w.shared, "app_id": app, "destination_space_id": w.bobPrivate, "source": "draft"})
}

// Templates, copies and diagnostics reach only what the user reaches: other people's private
// spaces, other workspaces and missing ones are "not found" alike, and nothing is made.
func TestAppManagementToolsCannotReachOtherUsersApps(t *testing.T) {
	w := newWorld(t)
	plain := templateWith(t, w, false)
	bobs := w.ok(w.bobConv, "create_app", map[string]any{"space_id": w.bobPrivate, "name": "Bob's", "definition": expenses(t)})
	elsewhere := w.ok(w.aliceConv2, "create_app", map[string]any{"space_id": w.alicePrivate2, "name": "Elsewhere", "definition": expenses(t)})
	mine := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Mine", "definition": expenses(t)})

	var before int
	if err := w.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	missing := w.alicePrivate2 + 1000
	for _, target := range []struct{ space, app int64 }{{w.bobPrivate, id(bobs["id"])}, {w.alicePrivate2, id(elsewhere["id"])}, {missing, 1}} {
		for tool, args := range map[string]map[string]any{
			"install_app_template": {"template_id": plain},
			"duplicate_app":        {"app_id": target.app},
			"list_app_effects":     {"app_id": target.app},
			"list_app_releases":    {"app_id": target.app},
			"show_app_roles":       {"app_id": target.app},
		} {
			args["space_id"] = target.space
			if out := w.status(http.StatusNotFound, w.aliceConv, tool, args); out["error"] != "not found" {
				t.Errorf("%s in space %d: %v", tool, target.space, out)
			}
		}
	}
	// An app is found only through its own space; a copy goes only where the user may write,
	// in the call's workspace.
	for tool, args := range map[string]map[string]any{
		"duplicate_app":    {"app_id": id(bobs["id"])},
		"list_app_effects": {"app_id": id(bobs["id"])},
		"show_app_roles":   {"app_id": id(bobs["id"])},
	} {
		args["space_id"] = w.alicePrivate
		w.status(http.StatusNotFound, w.aliceConv, tool, args)
	}
	for _, dest := range []int64{w.bobPrivate, w.alicePrivate2, missing} {
		w.status(http.StatusNotFound, w.aliceConv, "duplicate_app", map[string]any{"space_id": w.alicePrivate, "app_id": id(mine["id"]), "destination_space_id": dest})
	}
	// Her conversation in workspace two can't copy workspace one's app either.
	w.status(http.StatusNotFound, w.aliceConv2, "duplicate_app", map[string]any{"space_id": w.alicePrivate, "app_id": id(mine["id"])})
	// The templates list is the server's, the same for everyone.
	if list := w.ok(w.bobConv, "list_app_templates", map[string]any{})["templates"].([]any); len(list) == 0 {
		t.Fatal("no templates")
	}

	var after int
	if err := w.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&after); err != nil || after != before {
		t.Fatalf("apps: %d before, %d after, %v", before, after, err)
	}
}

// The diagnostic tools show what the user's roles in the app show, and only read.
func TestAppDiagnosticTools(t *testing.T) {
	w := newWorld(t)
	for _, d := range Definitions() {
		switch d.Name {
		case "list_app_templates", "list_app_effects", "list_app_releases", "show_app_roles":
			if !d.ReadOnly {
				t.Errorf("%s writes", d.Name)
			}
		case "install_app_template", "duplicate_app":
			if d.ReadOnly {
				t.Errorf("%s is read-only", d.Name)
			}
		}
		if strings.Contains(d.Name, "role") && !d.ReadOnly {
			t.Errorf("%s changes roles", d.Name)
		}
	}

	created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Team expenses", "definition": expenses(t)})
	app := id(created["id"])
	ref := map[string]any{"space_id": w.shared, "app_id": app}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for _, m := range []map[string]any{ref, extra} {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}

	// Releases: none yet, then the one a person published, without its definition.
	if rel := w.ok(w.aliceConv, "list_app_releases", ref)["releases"].([]any); len(rel) != 0 {
		t.Fatalf("releases = %v", rel)
	}
	w.publish(app)
	rel := w.ok(w.aliceConv, "list_app_releases", ref)["releases"].([]any)
	if len(rel) != 1 || rel[0].(map[string]any)["number"] != float64(1) || rel[0].(map[string]any)["live"] != true || rel[0].(map[string]any)["definition"] != nil {
		t.Fatalf("releases = %v", rel)
	}
	// Bob, a viewer who can't design or publish, is refused; once he is a reader of the app
	// he is still refused, though he sees its data.
	w.status(http.StatusForbidden, w.bobConv, "list_app_releases", ref)

	// Roles: unrestricted, no grants; then the grants a person set. A viewer can't manage them.
	roles := w.ok(w.aliceConv, "show_app_roles", ref)
	if g, _ := roles["grants"].([]any); len(g) != 0 || len(roles["roles"].([]any)) == 0 {
		t.Fatalf("show_app_roles = %v", roles)
	}
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder'), (?, ?, 'reader'), (?, NULL, 'reader')`, app, w.alice, app, w.alice, app)
	roles = w.ok(w.aliceConv, "show_app_roles", ref)
	if g := roles["grants"].([]any); len(g) != 2 {
		t.Fatalf("show_app_roles = %v", roles)
	}
	w.status(http.StatusForbidden, w.bobConv, "show_app_roles", ref)
	w.status(http.StatusForbidden, w.bobConv, "list_app_releases", ref)

	// Effects: those of records the user may read, as the API shows them.
	aliceRow := w.insert(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description": "a", "amount": 1}', ?) RETURNING id`, app, w.alice)
	bobRow := w.insert(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description": "b", "amount": 2}', ?) RETURNING id`, app, w.bob)
	effect := func(row, user int64, op, state string) {
		opID := w.insert(`INSERT INTO app_operations (app_id, user_id, operation, action, request_hash, record_id) VALUES (?, ?, ?, 'add', 'h', ?) RETURNING id`, app, user, op, row)
		errCode := any(nil)
		if state == "failed" {
			errCode = "app_effect_target"
		}
		w.exec(`INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, state, attempts, next_attempt_at, error_code)
			VALUES (?, ?, 0, 'create_card', ?, 1, ?, 'insert', '{"space_id": 0, "board_id": 0}', ?, 1, '2026-01-01T00:00:00.000Z', ?)`, app, opID, row, user, state, errCode)
	}
	effect(aliceRow, w.alice, "op1", "done")
	effect(aliceRow, w.alice, "op2", "failed")
	effect(bobRow, w.bob, "op3", "failed")
	list := w.ok(w.aliceConv, "list_app_effects", ref)
	if eff := list["effects"].([]any); len(eff) != 3 || list["failed"] != float64(2) || list["more"] != false {
		t.Fatalf("list_app_effects = %v", list)
	}
	failed := w.ok(w.aliceConv, "list_app_effects", with(map[string]any{"status": "failed", "limit": 1}))
	if eff := failed["effects"].([]any); len(eff) != 1 || failed["more"] != true {
		t.Fatalf("failed effects = %v", failed)
	}
	if e := failed["effects"].([]any)[0].(map[string]any); e["status"] != "failed" || e["error"].(map[string]any)["code"] != "app_effect_target" {
		t.Fatalf("effect = %v", e)
	}
	older := w.ok(w.aliceConv, "list_app_effects", with(map[string]any{"status": "failed", "before": failed["effects"].([]any)[0].(map[string]any)["id"]}))
	if len(older["effects"].([]any)) != 1 {
		t.Fatalf("older effects = %v", older)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "list_app_effects", with(map[string]any{"status": "bogus"}))
	w.status(http.StatusBadRequest, w.aliceConv, "list_app_effects", with(map[string]any{"limit": 1000}))

	// Alice, a plain user of the app, sees only the effects of her own records.
	w.exec(`DELETE FROM app_roles WHERE app_id = ?`, app)
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user'), (?, NULL, 'user')`, app, w.alice, app)
	own := w.ok(w.aliceConv, "list_app_effects", ref)
	raw, _ := json.Marshal(own)
	if len(own["effects"].([]any)) != 2 || strings.Contains(string(raw), `"record":`+jsonInt(bobRow)+`,`) {
		t.Fatalf("a user's effects = %s", raw)
	}
	// Bob, with the same role, sees his own and not alice's.
	bobs := w.ok(w.bobConv, "list_app_effects", ref)
	if eff := bobs["effects"].([]any); len(eff) != 1 || id(eff[0].(map[string]any)["record"]) != bobRow {
		t.Fatalf("bob's effects = %v", bobs)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A long list of releases comes in pages that each fit the bound, whatever their size: the
// keys that say a page is cut short count toward it, and next is the before of the next page.
func TestListAppReleasesPages(t *testing.T) {
	w := newWorld(t)
	const total = 400
	var app int64
	var ref map[string]any
	// Every release is a little longer than the last variant, so some page ends within the
	// few bytes the "truncated" and "next" keys take, which a size measured without them
	// would let through.
	for pad := range 30 {
		// A release stays as long as its app does, so each variant is an app of its own.
		created := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Many releases", "definition": expenses(t)})
		app = id(created["id"])
		ref = map[string]any{"space_id": w.shared, "app_id": app}
		w.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
			INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
			SELECT ?, i, '{}', 1, json_array('component.'||printf('%.*c', ?, 'x')) FROM n`, total, app, 40+pad)
		w.exec(`UPDATE apps SET live_release = ? WHERE id = ?`, total, app)

		var seen []int
		args := ref
		for pages := 0; ; pages++ {
			if pages > total {
				t.Fatalf("pad %d: paging never ends", pad)
			}
			page := w.ok(w.aliceConv, "list_app_releases", args)
			rel := page["releases"].([]any)
			raw, _ := json.Marshal(page)
			if len(raw) > MaxResult || len(rel) == 0 {
				t.Fatalf("pad %d: page of %d releases is %d bytes", pad, len(rel), len(raw))
			}
			for _, r := range rel {
				seen = append(seen, int(r.(map[string]any)["number"].(float64)))
			}
			next, more := page["next"]
			if (page["truncated"] == true) != more || (more && next != float64(seen[len(seen)-1])) {
				t.Fatalf("pad %d: truncated %v, next %v after %d", pad, page["truncated"], next, seen[len(seen)-1])
			}
			if !more {
				break
			}
			args = map[string]any{"space_id": w.shared, "app_id": app, "before": next}
		}
		if len(seen) != total {
			t.Fatalf("pad %d: %d releases seen, want %d", pad, len(seen), total)
		}
		for i, n := range seen {
			if n != total-i {
				t.Fatalf("pad %d: release %d is number %d, want %d", pad, i, n, total-i)
			}
		}
	}
	// A cursor past the oldest is an empty list, and a negative one is refused.
	if rel := w.ok(w.aliceConv, "list_app_releases", map[string]any{"space_id": w.shared, "app_id": app, "before": 1})["releases"].([]any); len(rel) != 0 {
		t.Fatalf("before 1 = %v", rel)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "list_app_releases", map[string]any{"space_id": w.shared, "app_id": app, "before": -1})
	w.status(http.StatusForbidden, w.bobConv, "list_app_releases", map[string]any{"space_id": w.shared, "app_id": app, "before": 5})
}
