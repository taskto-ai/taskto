package apps

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// The search index holds a record's string, choice and barcode values, as the live release
// declares them: never a field's name, nothing undeclared, nothing of another type, and nothing at
// all of a record that is deleted, gone, or of an app that was never published.
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

	// An undeclared field, and a value of another type than its field's, are left out.
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.hidden', 'quokka', '$.description', 'visible', '$.amount', 'text') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if text, _, _ = SearchText(ctx, w.e.db, id); text != "visible" {
		t.Fatalf("text = %q", text)
	}
	// A string of the wrong type for its field is left out too.
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.description', 12) WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if text, indexed, _ = SearchText(ctx, w.e.db, id); text != "" || !indexed {
		t.Fatalf("text = %q, indexed = %v", text, indexed)
	}

	// Deleted, gone, or a draft's: nothing.
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

// A record's text is bounded, in bytes: the fields that fit, in the collection's order.
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

// ReadsRecord is the one test of whether someone reads a record, as the lists of records go by.
func TestReadsRecordIsTheScopeOfTheRoles(t *testing.T) {
	mine, theirs, gone := sql.NullInt64{Int64: 7, Valid: true}, sql.NullInt64{Int64: 8, Valid: true}, sql.NullInt64{}
	cases := []struct {
		name string
		acc  access.AppAccess
		want [3]bool // a record of 7, of 8, of nobody's account any more
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

// Choices are indexed as people read them (the label, and the stored value too if it is another
// text) and barcodes as they are; a location, a signature and attachments are never indexed.
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
	// A release can't be changed: a new one, with these fields, goes live.
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
	// A labelled choice: its label, then the value it is stored as. A barcode as it is.
	if got := set(`{"description":"lunch","category":"food","sku":"4006381333931"}`); got != "lunch\nMeals and snacks\nfood\n4006381333931" {
		t.Fatalf("text = %q", got)
	}
	// An option with no label is shown, and stored, as it is: once.
	if got := set(`{"category":"travel"}`); got != "travel" {
		t.Fatalf("an unlabelled choice = %q", got)
	}
	// A location, a signature and photos hold ids and coordinates: nothing to search for.
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
