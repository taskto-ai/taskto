package apps

import (
	"errors"
	"testing"
)

func TestRowsOfAnAppThatMovedAreNotReadFromTheSpaceItLeft(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	acc, err := w.e.s.appAccess(ctx, Actor{UserID: w.alice.id}, w.alicePrivate.ID, w.aliceApp.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := readable(acc, w.alice.id)

	rows, err := loadSpaceRows(ctx, w.e.s.db, w.alicePrivate.ID, w.aliceApp.ID, v)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows in the private space = %v, %v, want its one row", rows, err)
	}

	if _, err := w.e.db.Exec(`UPDATE apps SET space_id = ? WHERE id = ?`, w.shared.ID, w.aliceApp.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := loadSpaceRows(ctx, w.e.s.db, w.alicePrivate.ID, w.aliceApp.ID, v); err != nil || len(rows) != 0 {
		t.Fatalf("rows of an app that left the private space = %v, %v, want none", rows, err)
	}
	if _, err := w.e.s.ListRows(ctx, Actor{UserID: w.alice.id}, w.alicePrivate.ID, w.aliceApp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListRows of the app through the space it left = %v, want ErrNotFound", err)
	}

	const id = "0123456789abcdef0123456789abcdef"
	if _, err := w.e.db.Exec(`INSERT INTO app_attachments (id, app_id, space_id, field, status, size, received, content_type)
		VALUES (?, ?, ?, 'photos', 'ready', 3, 3, 'image/png')`, id, w.aliceApp.ID, w.shared.ID); err != nil {
		t.Fatal(err)
	}
	def := Definition{}
	def.Collection.Fields = append(def.Collection.Fields, Field{Name: "photos", Type: TypeAttachment})
	describe := func(spaceID int64) Described {
		items := []map[string]any{{"photos": []any{id}}}
		if err := describeAttachments(ctx, w.e.s.db, spaceID, w.aliceApp.ID, def, items); err != nil {
			t.Fatal(err)
		}
		return items[0]["photos"].([]Described)[0]
	}
	if got := describe(w.shared.ID); got.ContentType != "image/png" {
		t.Fatalf("attachment in the app's own space = %+v, want its metadata", got)
	}
	if got := describe(w.alicePrivate.ID); got.ContentType != "" || got.Size != 0 {
		t.Fatalf("attachment of an app that left the space was described: %+v", got)
	}
}
