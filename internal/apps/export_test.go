package apps

import (
	"errors"
	"slices"
	"testing"
)

func (w *world) grantIn(a App, userID int64, roles ...string) {
	w.e.t.Helper()
	for _, r := range roles {
		if _, err := w.e.db.Exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, ?)`, a.ID, userID, r); err != nil {
			w.e.t.Fatal(err)
		}
	}
}

func descriptions(items []map[string]any) []string {
	var out []string
	for _, it := range items {
		if d, ok := it["description"].(string); ok {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

func TestListRowsReadsAccessAndRecordsInOneSnapshot(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.add(w.alice, w.shared, w.sharedApp, "second", "5")
	w.grantIn(w.sharedApp, w.alice.id, "data_manager")
	w.grantIn(w.sharedApp, w.owner.id, "builder")

	w.e.dbHooks.BeforeQuery("FROM app_rows r JOIN apps a", func() {
		tx, err := w.e.db.Begin()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`UPDATE app_roles SET role = 'user' WHERE app_id = ? AND user_id = ?`, w.sharedApp.ID, w.alice.id); err != nil {
			t.Error(err)
		}
		if _, err := tx.Exec(`UPDATE app_rows SET deleted_at = CURRENT_TIMESTAMP WHERE app_id = ? AND data LIKE '%team-lunch%'`, w.sharedApp.ID); err != nil {
			t.Error(err)
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	})
	items, err := w.e.s.ListRows(ctx, Actor{UserID: w.alice.id}, w.shared.ID, w.sharedApp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := descriptions(items); !slices.Equal(got, []string{"second", "team-lunch"}) {
		t.Fatalf("records = %v, want both, as of the one state the access was read in", got)
	}
	items, err = w.e.s.ListRows(ctx, Actor{UserID: w.alice.id}, w.shared.ID, w.sharedApp.ID)
	if err != nil || !slices.Equal(descriptions(items), []string{"second"}) {
		t.Fatalf("records after = %v, %v, want their own that is left", descriptions(items), err)
	}
}

func TestExportAppNeedsARoleHeldInTheApp(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.grantIn(w.sharedApp, w.bob.id, "reader")
	owner := Actor{UserID: w.owner.id}

	if _, err := w.e.s.ExportApp(ctx, owner, w.shared.ID, w.sharedApp.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an admin's workspace export of a stranded app they hold no role in = %v, want ErrNotFound", err)
	}
	if _, err := w.e.s.ListRows(ctx, owner, w.shared.ID, w.sharedApp.ID); err != nil {
		t.Fatalf("the admin's way back into the app = %v: the test needs it open to them", err)
	}
	w.grantIn(w.sharedApp, w.owner.id, "reader")
	got, err := w.e.s.ExportApp(ctx, owner, w.shared.ID, w.sharedApp.ID, true)
	if err != nil || len(got.Rows) != 1 || got.Live == nil || got.Live.Definition == nil {
		t.Fatalf("export with a role = %+v, %v", got, err)
	}
	if got.App.Draft != nil || got.App.EffectTargets != nil {
		t.Errorf("the export holds the draft: %+v", got.App)
	}
}

func TestExportAppPairsTheDefinitionWithItsRecords(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	published := false
	w.e.dbHooks.BeforeQuery("FROM app_rows r JOIN apps a", func() {
		cur := w.view(w.alice, w.shared, w.sharedApp)
		cur = w.mustPatchDraft(w.alice, w.shared, cur, without(*cur.Draft, "description"))
		published = w.tryPublish(w.alice, w.shared, cur, SchemaChange{ChangeFieldRemoved, "description"}).Code == 200
	})
	got, err := w.e.s.ExportApp(ctx, Actor{UserID: w.alice.id}, w.shared.ID, w.sharedApp.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("the test's publish did not happen")
	}
	hasField := slices.ContainsFunc(got.Live.Definition.Collection.Fields, func(f Field) bool { return f.Name == "description" })
	hasValues := len(descriptions(got.Rows)) > 0
	if hasField != hasValues {
		t.Fatalf("release %d's definition has the field = %v but the records show it = %v: not one release", got.Live.Number, hasField, hasValues)
	}
}

func TestExportAppAsksTheWorkspaceRoleInItsSnapshot(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.grantIn(w.sharedApp, w.owner.id, "reader")
	owner := Actor{UserID: w.owner.id}
	demote := func() {
		if _, err := w.e.db.Exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, w.ws, w.owner.id); err != nil {
			t.Error(err)
		}
	}
	w.e.dbHooks.BeforeBegin(demote)
	got, err := w.e.s.ExportApp(ctx, owner, w.shared.ID, w.sharedApp.ID, true)
	if !errors.Is(err, ErrNotAdmin) || len(got.Rows) != 0 || got.Live != nil {
		t.Fatalf("a workspace export of an app by an admin demoted before its snapshot = %+v, %v, want ErrNotAdmin and nothing", got, err)
	}
	if _, err := w.e.s.ExportApp(ctx, owner, w.shared.ID, w.sharedApp.ID, false); err != nil {
		t.Fatalf("an export that isn't the workspace's = %v", err)
	}
	if _, err := w.e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, w.ws, w.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE users SET disabled_at = CURRENT_TIMESTAMP WHERE id = ?`, w.owner.id); err != nil {
		t.Fatal(err)
	}
	if got, err := w.e.s.ExportApp(ctx, owner, w.shared.ID, w.sharedApp.ID, true); err == nil || len(got.Rows) != 0 {
		t.Fatalf("a disabled admin's export of an app = %+v, %v, want nothing", got, err)
	}
}
