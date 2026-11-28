package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

func TestAWorkspaceSkillIsWrittenOnlyByAnAdminWhoIsOneInTheTransaction(t *testing.T) {
	ctx := context.Background()
	wide := func(w *world) Owner { return w.owner(ScopeWorkspace, 0, 0) }
	title := "Changed"

	for name, revoke := range map[string]func(w *world){
		"demoted": func(w *world) {
			w.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann)
		},
		"removed": func(w *world) { w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann) },
		"disabled": func(w *world) {
			w.exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.ann)
		},
	} {
		w := newWorld(t)
		made := w.create(wide(w), w.ann, "house-style")
		before := len(w.events())
		revoke(w)

		if _, err := w.store.Create(ctx, wide(w), w.ann, input("late")); err == nil {
			t.Errorf("%s: a create by an admin who no longer is one", name)
		}
		if _, err := w.store.Update(ctx, wide(w), w.ann, made.ID, Patch{Title: &title}); err == nil {
			t.Errorf("%s: an update by an admin who no longer is one", name)
		}
		if err := w.store.Delete(ctx, wide(w), w.ann, made.ID); err == nil {
			t.Errorf("%s: a delete by an admin who no longer is one", name)
		}
		if n := w.scalar(`SELECT count(*) FROM skills WHERE workspace_id = ? AND title = ? OR name = 'late'`, w.ws, title); n != 0 {
			t.Errorf("%s: %d skills changed or made", name, n)
		}
		if n := w.scalar(`SELECT count(*) FROM skills WHERE id = ?`, made.ID); n != 1 {
			t.Errorf("%s: the skill was deleted", name)
		}
		if got := len(w.events()); got != before {
			t.Errorf("%s: %d audit events after refused writes, want %d", name, got, before)
		}
	}

	w := newWorld(t)
	w.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann)
	if _, err := w.store.Create(ctx, wide(w), w.ann, input("late")); code(err) != apierr.CodeAdminOnly {
		t.Fatalf("a demoted admin creating: %v", err)
	}
	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann)
	if _, err := w.store.Create(ctx, wide(w), w.ann, input("later")); code(err) != apierr.CodeWorkspaceNotFound {
		t.Fatalf("a removed admin creating: %v", err)
	}
}

func TestAPersonalSkillIsWrittenOnlyByAMemberOfItsWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	mine := w.owner(ScopeUser, w.carol, 0)
	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.carol)

	if _, err := w.store.Create(ctx, mine, w.carol, input("late")); code(err) != apierr.CodeWorkspaceNotFound {
		t.Fatalf("a create by a person who left: %v", err)
	}
	w.member(w.ws, w.carol, "member")
	if got, err := w.store.ForUser(ctx, w.ws, w.carol); err != nil || len(got) != 0 {
		t.Fatalf("coming back brings a skill made after leaving: %v, %v", names(got), err)
	}
	if n := w.scalar(`SELECT count(*) FROM skills WHERE user_id = ?`, w.carol); n != 0 {
		t.Fatalf("%d personal skills of a person who left", n)
	}

	someoneElse := w.owner(ScopeUser, w.bob, 0)
	if _, err := w.store.Create(ctx, someoneElse, w.carol, input("not-mine")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a person's skill made for another person: %v", err)
	}
}

func TestTheSchemaRefusesAPersonalSkillOfANonMember(t *testing.T) {
	w := newWorld(t)
	_, err := w.db.Exec(`INSERT INTO skills (workspace_id, scope, user_id, name, title, description, body)
		VALUES (?, 'user', ?, 'stray', 'T', 'D', 'B')`, w.ws, w.dave)
	if err == nil {
		t.Fatal("a personal skill of a person who is not in the workspace was stored")
	}
	if n := w.scalar(`SELECT count(*) FROM skills`); n != 0 {
		t.Fatalf("%d skills stored", n)
	}
}

func TestListFollowsWhoMayReadAndManageAtTheTimeOfTheRead(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	off := false
	wide := w.owner(ScopeWorkspace, 0, 0)
	w.create(wide, w.ann, "house-style")
	draft := w.create(wide, w.ann, "draft-only")
	if _, err := w.store.Update(ctx, wide, w.ann, draft.ID, Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	team := w.owner(ScopeSpace, 0, w.team)
	w.create(team, w.bob, "team-way")
	mine := w.owner(ScopeUser, w.bob, 0)
	w.create(mine, w.bob, "bobs-own")

	list := func(o Owner, actor int64) ([]string, error) {
		got, err := w.store.List(ctx, o, actor)
		return names(got), err
	}
	if got, err := list(wide, w.ann); err != nil || !slices.Equal(got, []string{"workspace:draft-only", "workspace:house-style"}) {
		t.Fatalf("an admin's list: %v, %v", got, err)
	}
	if got, err := list(wide, w.bob); err != nil || !slices.Equal(got, []string{"workspace:house-style"}) {
		t.Fatalf("a member's list: %v, %v", got, err)
	}
	w.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann)
	if got, err := list(wide, w.ann); err != nil || !slices.Equal(got, []string{"workspace:house-style"}) {
		t.Fatalf("a demoted admin's list: %v, %v", got, err)
	}
	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.ann)
	if got, err := list(wide, w.ann); code(err) != apierr.CodeWorkspaceNotFound || len(got) != 0 {
		t.Fatalf("a removed member's list: %v, %v", got, err)
	}

	if _, err := list(mine, w.carol); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's own skills: %v", err)
	}
	w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team, w.carol)
	if _, err := list(team, w.carol); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a space's skills for a person who can't read it: %v", err)
	}
	w.exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.bob)
	if got, err := list(mine, w.bob); err == nil || len(got) != 0 {
		t.Fatalf("a disabled account's own skills: %v, %v", got, err)
	}
}

func TestForUserAnswersFromOneMomentWhenAccessIsRevokedMidway(t *testing.T) {
	setup := func() *world {
		w := newWorld(t)
		w.create(w.owner(ScopeWorkspace, 0, 0), w.ann, "house-style")
		w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-way")
		w.create(w.owner(ScopeUser, w.bob, 0), w.bob, "bobs-own")
		return w
	}
	everything := []string{"workspace:house-style", "space:team-way", "user:bobs-own"}
	for name, revoke := range map[string]func(w *world){
		"removed from the workspace": func(w *world) {
			w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.bob)
		},
		"disabled": func(w *world) {
			w.exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.bob)
		},
	} {
		w := setup()
		w.hooks.BeforeQuery("FROM spaces s", func() { revoke(w) })
		got, err := w.store.ForUser(context.Background(), w.ws, w.bob)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := names(got); len(n) != 0 && !slices.Equal(n, everything) {
			t.Errorf("%s: %v is neither what the person could use before nor nothing", name, n)
		}
	}
}

func TestForUserSupportsManyReadableSpaces(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := range 1001 {
		if err := tx.QueryRow(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', ?) RETURNING id`, w.ws, fmt.Sprintf("Many %04d", i)).Scan(&last); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, last, w.carol); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.create(w.owner(ScopeWorkspace, 0, 0), w.ann, "house-style")
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, last, w.ann)
	w.create(w.owner(ScopeSpace, 0, last), w.ann, "last-way")

	got, err := w.store.ForUser(ctx, w.ws, w.carol)
	if err != nil || !slices.Equal(names(got), []string{"workspace:house-style", "space:last-way"}) {
		t.Fatalf("a person who reads 1001 spaces: %v, %v", names(got), err)
	}
}

func replaceSpaceKeepingItsID(t *testing.T, w *world, space int64) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := w.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		fmt.Sprintf(`DELETE FROM space_members WHERE space_id = %d`, space),
		fmt.Sprintf(`DELETE FROM spaces WHERE id = %d`, space),
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	again := w.space(w.ws, "Again")
	if again != space {
		t.Fatalf("the new space has id %d, want the removed one's %d", again, space)
	}
	w.join(again, w.ann, "editor")
	return again
}

func TestASpaceThatTookAnotherSpacesIDCannotChangeItsSkills(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	old := w.create(w.owner(ScopeSpace, 0, w.secret), w.ann, "secret-way")
	replaceSpaceKeepingItsID(t, w, w.secret)
	now := w.owner(ScopeSpace, 0, w.secret)
	title := "Taken"

	if _, err := w.store.Update(ctx, now, w.ann, old.ID, Patch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an update of the old space's skill by the new space: %v", err)
	}
	if err := w.store.Delete(ctx, now, w.ann, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a delete of the old space's skill by the new space: %v", err)
	}
	if n := w.scalar(`SELECT count(*) FROM skills WHERE id = ? AND title = ?`, old.ID, old.Title); n != 1 {
		t.Fatal("the old space's skill was changed or removed through the new space")
	}
}

func TestAOneLineFieldHasNoLineBreakOfAnyKind(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	o := w.owner(ScopeUser, w.bob, 0)
	breaks := map[string]string{
		"a line separator":      "a" + string(rune(0x2028)) + "b",
		"a paragraph separator": "a" + string(rune(0x2029)) + "b",
		"a next line":           "a" + string(rune(0x85)) + "b",
		"a vertical tab":        "a" + string(rune(0x0b)) + "b",
		"a form feed":           "a" + string(rune(0x0c)) + "b",
		"a carriage return":     "a" + string(rune(0x0d)) + "b",
	}
	made := w.create(o, w.bob, "closing")
	n := 0
	for name, text := range breaks {
		n++
		in := input(fmt.Sprintf("quote-%d", n))
		in.Title = text
		if _, err := w.store.Create(ctx, o, w.bob, in); code(err) != apierr.CodeSkillTitleLength {
			t.Errorf("a title with %s: %v", name, err)
		}
		in = input(fmt.Sprintf("other-%d", n))
		in.Description = text
		if _, err := w.store.Create(ctx, o, w.bob, in); code(err) != apierr.CodeSkillDescriptionLength {
			t.Errorf("a description with %s: %v", name, err)
		}
		text := text
		if _, err := w.store.Update(ctx, o, w.bob, made.ID, Patch{Title: &text}); code(err) != apierr.CodeSkillTitleLength {
			t.Errorf("a patched title with %s: %v", name, err)
		}
		if _, err := w.store.Update(ctx, o, w.bob, made.ID, Patch{Description: &text}); code(err) != apierr.CodeSkillDescriptionLength {
			t.Errorf("a patched description with %s: %v", name, err)
		}
	}
	tabbed := input("tabbed")
	tabbed.Title = "a\tb"
	if _, err := w.store.Create(ctx, o, w.bob, tabbed); err != nil {
		t.Fatalf("a title with a tab: %v", err)
	}
	if n := w.scalar(`SELECT count(*) FROM skills WHERE title LIKE '%' || char(8232) || '%' OR description LIKE '%' || char(8233) || '%'`); n != 0 {
		t.Fatalf("%d skills with a line break stored", n)
	}
}
