package apps

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func TestSearchTextIsTheDeclaredTextValues(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	row := func(a App, description string) int64 {
		t.Helper()
		var id int64
		if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ? AND json_extract(data, '$.description') = ?`, a.ID, description).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := row(w.aliceApp, "alice-secret")
	text, indexed, err := SearchText(ctx, w.e.db, id)
	if err != nil || !indexed || text != "alice-secret" {
		t.Fatalf("text = %q, %v, %v: want the description only, not the amount or any name", text, indexed, err)
	}

	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.hidden', 'quokka', '$.description', 'visible', '$.amount', 'text') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if text, _, _ = SearchText(ctx, w.e.db, id); text != "visible" {
		t.Fatalf("text = %q", text)
	}
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.description', 12) WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if text, indexed, _ = SearchText(ctx, w.e.db, id); text != "" || !indexed {
		t.Fatalf("text = %q, indexed = %v", text, indexed)
	}

	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.description', 'visible'), deleted_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if text, indexed, err = SearchText(ctx, w.e.db, id); err != nil || indexed || text != "" {
		t.Fatalf("a deleted record = %q, %v, %v", text, indexed, err)
	}
	if _, indexed, err = SearchText(ctx, w.e.db, id+1000); err != nil || indexed {
		t.Fatalf("a record that isn't there: %v, %v", indexed, err)
	}
	draft := w.draft(w.alice, w.alicePrivate, "Draft")
	if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description":"in a draft"}', ?)`, draft.ID, w.alice.id); err != nil {
		t.Fatal(err)
	}
	draftRow := row(draft, "in a draft")
	if text, indexed, err = SearchText(ctx, w.e.db, draftRow); err != nil || indexed || text != "" {
		t.Fatalf("a draft's record = %q, %v, %v", text, indexed, err)
	}
}

func TestSearchTextIsBounded(t *testing.T) {
	w := newWorld(t)
	id := int64(0)
	if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ?`, w.aliceApp.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.description', ?) WHERE id = ?`, strings.Repeat("a", MaxSearchText+10), id); err != nil {
		t.Fatal(err)
	}
	text, indexed, err := SearchText(context.Background(), w.e.db, id)
	if err != nil || !indexed || len(text) > MaxSearchText {
		t.Fatalf("a long value: %d bytes, %v, %v", len(text), indexed, err)
	}
}

func TestReadsRecordIsTheScopeOfTheRoles(t *testing.T) {
	mine, theirs, gone := sql.NullInt64{Int64: 7, Valid: true}, sql.NullInt64{Int64: 8, Valid: true}, sql.NullInt64{}
	cases := []struct {
		name string
		acc  access.AppAccess
		want [3]bool
	}{
		{"every record", access.AppAccess{View: true, ReadRecords: access.ScopeAll}, [3]bool{true, true, true}},
		{"only their own", access.AppAccess{View: true, ReadRecords: access.ScopeOwn}, [3]bool{true, false, false}},
		{"none", access.AppAccess{View: true, ReadRecords: access.ScopeNone}, [3]bool{false, false, false}},
		{"no view", access.AppAccess{ReadRecords: access.ScopeAll}, [3]bool{false, false, false}},
	}
	for _, c := range cases {
		got := [3]bool{ReadsRecord(c.acc, 7, mine), ReadsRecord(c.acc, 7, theirs), ReadsRecord(c.acc, 7, gone)}
		if got != c.want {
			t.Errorf("%s: reads %v, want %v", c.name, got, c.want)
		}
		wantScope := c.acc.ReadRecords
		if !c.acc.View {
			wantScope = access.ScopeNone
		}
		if RecordScope(c.acc) != wantScope {
			t.Errorf("%s: scope %v, want %v", c.name, RecordScope(c.acc), wantScope)
		}
	}
}

func TestSearchTextOfChoicesBarcodesAndCaptures(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fields := `[
		{"name":"description","type":"string"},
		{"name":"category","type":"choice","options":["food","travel"],"option_labels":{"food":"Meals and snacks"}},
		{"name":"sku","type":"barcode"},
		{"name":"where","type":"location"},
		{"name":"sign","type":"signature"},
		{"name":"photos","type":"attachment"}]`
	if _, err := w.e.db.Exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT rel.app_id, 99, json_set(rel.definition, '$.collection', json_object('fields', json(?))), rel.definition_version, rel.requires
		FROM app_releases rel JOIN apps a ON a.id = rel.app_id AND rel.number = a.live_release WHERE a.id = ?`, fields, w.aliceApp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 99 WHERE id = ?`, w.aliceApp.ID); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ?`, w.aliceApp.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	set := func(data string) string {
		t.Helper()
		if _, err := w.e.db.Exec(`UPDATE app_rows SET data = ? WHERE id = ?`, data, id); err != nil {
			t.Fatal(err)
		}
		text, indexed, err := SearchText(ctx, w.e.db, id)
		if err != nil || !indexed {
			t.Fatalf("SearchText = %q, %v, %v", text, indexed, err)
		}
		return text
	}
	if got := set(`{"description":"lunch","category":"food","sku":"4006381333931"}`); got != "lunch\nMeals and snacks\nfood\n4006381333931" {
		t.Fatalf("text = %q", got)
	}
	if got := set(`{"category":"travel"}`); got != "travel" {
		t.Fatalf("an unlabelled choice = %q", got)
	}
	got := set(`{"description":"visit","where":{"lat":-6.1,"lng":39.3,"captured_at":"2026-01-01T00:00:00.000Z"},"sign":"signature-id","photos":["photo-id-1","photo-id-2"]}`)
	if got != "visit" {
		t.Fatalf("a record with a location, a signature and photos = %q, want its description only", got)
	}
	for _, leak := range []string{"signature-id", "photo-id", "39.3", "-6.1", "where", "sign", "photos"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the text %q holds %q", got, leak)
		}
	}
}
