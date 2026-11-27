package skills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/db"
)

type world struct {
	t      *testing.T
	db     *sql.DB
	store  *Store
	ws     int64
	other  int64
	ann    int64
	bob    int64
	carol  int64
	dave   int64
	team   int64
	secret int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, db: d, store: New(d, access.New(d, nil))}
	w.ann = w.user("ann@example.com")
	w.bob = w.user("bob@example.com")
	w.carol = w.user("carol@example.com")
	w.dave = w.user("dave@example.com")
	w.ws = w.workspace("One")
	w.other = w.workspace("Two")
	w.member(w.ws, w.ann, "admin")
	w.member(w.ws, w.bob, "member")
	w.member(w.ws, w.carol, "member")
	w.member(w.other, w.dave, "admin")
	w.team = w.space(w.ws, "Team")
	w.secret = w.space(w.ws, "Secret")
	w.join(w.team, w.bob, "editor")
	w.join(w.team, w.carol, "viewer")
	w.join(w.secret, w.ann, "editor")
	return w
}

func (w *world) scalar(q string, args ...any) int64 {
	w.t.Helper()
	var v int64
	if err := w.db.QueryRow(q, args...).Scan(&v); err != nil {
		w.t.Fatal(err)
	}
	return v
}

func (w *world) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.db.Exec(q, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) user(email string) int64 {
	return w.scalar(`INSERT INTO users (email, name, password_hash) VALUES (?, 'N', 'x') RETURNING id`, email)
}

func (w *world) workspace(name string) int64 {
	return w.scalar(`INSERT INTO workspaces (name) VALUES (?) RETURNING id`, name)
}

func (w *world) member(ws, user int64, role string) {
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)`, ws, user, role)
}

func (w *world) space(ws int64, name string) int64 {
	return w.scalar(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', ?) RETURNING id`, ws, name)
}

func (w *world) join(space, user int64, role string) {
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, ?)`, space, user, role)
}

func (w *world) owner(scope Scope, user, space int64) Owner {
	o := Owner{Scope: scope, WorkspaceID: w.ws, UserID: user}
	if scope == ScopeSpace {
		o.SpaceID, o.SpaceGeneration = space, w.scalar(`SELECT generation FROM spaces WHERE id = ?`, space)
	}
	return o
}

func input(name string) Input {
	return Input{Name: name, Title: "Title of " + name, Description: "when to use " + name, Body: "# " + name + "\n\nDo it well."}
}

func (w *world) create(o Owner, actor int64, name string) Skill {
	w.t.Helper()
	s, err := w.store.Create(context.Background(), o, actor, input(name))
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	return s
}

func (w *world) get(o Owner, id int64) (Skill, error) {
	w.t.Helper()
	list, err := w.store.List(context.Background(), o, true)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return s, nil
		}
	}
	return Skill{}, ErrNotFound
}

func names(list []Skill) []string {
	out := []string{}
	for _, s := range list {
		out = append(out, string(s.Scope)+":"+s.Name)
	}
	return out
}

func code(err error) apierr.Code {
	var api *apierr.Error
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

func (w *world) events() []audit.Row {
	w.t.Helper()
	rows, err := audit.List(context.Background(), w.db, audit.Filter{Workspace: w.ws, Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []audit.Row
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "skill_") {
			out = append(out, r)
		}
	}
	return out
}

func TestEachScopeHasItsOwnCreateListUpdateAndDelete(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for name, o := range map[string]Owner{
		"user":      w.owner(ScopeUser, w.bob, 0),
		"space":     w.owner(ScopeSpace, 0, w.team),
		"workspace": w.owner(ScopeWorkspace, 0, 0),
	} {
		actor := w.bob
		if o.Scope == ScopeWorkspace {
			actor = w.ann
		}
		created := w.create(o, actor, "quote-writer")
		if created.Scope != o.Scope || !created.Enabled || created.Origin != "" || created.Body == "" {
			t.Fatalf("%s: created %+v", name, created)
		}
		if (o.Scope == ScopeSpace) != (created.SpaceID != nil) {
			t.Fatalf("%s: space id %v", name, created.SpaceID)
		}
		list, err := w.store.List(ctx, o, true)
		if err != nil || len(list) != 1 || list[0].ID != created.ID {
			t.Fatalf("%s: list = %+v, %v", name, list, err)
		}
		title, off := "A new title", false
		updated, err := w.store.Update(ctx, o, actor, created.ID, Patch{Title: &title, Enabled: &off})
		if err != nil || updated.Title != title || updated.Enabled || updated.Name != "quote-writer" {
			t.Fatalf("%s: update = %+v, %v", name, updated, err)
		}
		if shown, _ := w.store.List(ctx, o, false); len(shown) != 0 {
			t.Fatalf("%s: a disabled skill is listed to those who only use it: %+v", name, shown)
		}
		renamed := "quote-maker"
		if got, err := w.store.Update(ctx, o, actor, created.ID, Patch{Name: &renamed}); err != nil || got.Name != renamed {
			t.Fatalf("%s: rename = %+v, %v", name, got, err)
		}
		if err := w.store.Delete(ctx, o, actor, created.ID); err != nil {
			t.Fatalf("%s: delete: %v", name, err)
		}
		if err := w.store.Delete(ctx, o, actor, created.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: second delete = %v", name, err)
		}
		if _, err := w.get(o, created.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: get after delete = %v", name, err)
		}
	}
}

func TestNamesAndSizesAreChecked(t *testing.T) {
	w := newWorld(t)
	o := w.owner(ScopeUser, w.bob, 0)
	ctx := context.Background()
	bad := map[string]struct {
		mod  func(*Input)
		code apierr.Code
	}{
		"a capital":            {func(i *Input) { i.Name = "Quote" }, apierr.CodeSkillNameInvalid},
		"a leading digit":      {func(i *Input) { i.Name = "1quote" }, apierr.CodeSkillNameInvalid},
		"one letter":           {func(i *Input) { i.Name = "q" }, apierr.CodeSkillNameInvalid},
		"41 letters":           {func(i *Input) { i.Name = "a" + strings.Repeat("b", 40) }, apierr.CodeSkillNameInvalid},
		"an underscore":        {func(i *Input) { i.Name = "my_skill" }, apierr.CodeSkillNameInvalid},
		"a path":               {func(i *Input) { i.Name = "a/../b" }, apierr.CodeSkillNameInvalid},
		"no name":              {func(i *Input) { i.Name = "" }, apierr.CodeSkillNameInvalid},
		"no title":             {func(i *Input) { i.Title = "  " }, apierr.CodeSkillTitleLength},
		"a title of 101":       {func(i *Input) { i.Title = strings.Repeat("t", 101) }, apierr.CodeSkillTitleLength},
		"a title of two lines": {func(i *Input) { i.Title = "a\nb" }, apierr.CodeSkillTitleLength},
		"no description":       {func(i *Input) { i.Description = "" }, apierr.CodeSkillDescriptionLength},
		"a description of 301": {func(i *Input) { i.Description = strings.Repeat("d", 301) }, apierr.CodeSkillDescriptionLength},
		"no body":              {func(i *Input) { i.Body = " \n " }, apierr.CodeSkillBodyLength},
		"a body over 32 KiB":   {func(i *Input) { i.Body = strings.Repeat("b", MaxBody+1) }, apierr.CodeSkillBodyLength},
		"a NUL in the body":    {func(i *Input) { i.Body = "a\x00b" }, apierr.CodeSkillBodyLength},
		"invalid UTF-8":        {func(i *Input) { i.Body = "a\xffb" }, apierr.CodeSkillBodyLength},
	}
	for name, c := range bad {
		in := input("valid-name")
		c.mod(&in)
		if _, err := w.store.Create(ctx, o, w.bob, in); code(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := w.scalar(`SELECT count(*) FROM skills`); n != 0 {
		t.Fatalf("%d skills made by refused requests", n)
	}

	exact := Input{Name: "a" + strings.Repeat("b", 39), Title: strings.Repeat("é", MaxTitle), Description: strings.Repeat("d", MaxDescription), Body: strings.Repeat("b", MaxBody)}
	if _, err := w.store.Create(ctx, o, w.bob, exact); err != nil {
		t.Fatalf("the largest valid skill: %v", err)
	}
	if _, err := w.store.Update(ctx, o, w.bob, 1, Patch{}); code(err) != apierr.CodeBadRequest {
		t.Fatalf("an empty patch: %v", err)
	}
	long := strings.Repeat("x", MaxBody+1)
	if _, err := w.store.Update(ctx, o, w.bob, 1, Patch{Body: &long}); code(err) != apierr.CodeSkillBodyLength {
		t.Fatalf("a long body in a patch: %v", err)
	}
	badName := "Bad Name"
	if _, err := w.store.Update(ctx, o, w.bob, 1, Patch{Name: &badName}); code(err) != apierr.CodeSkillNameInvalid {
		t.Fatalf("a bad name in a patch: %v", err)
	}
}

func TestANameIsUniquePerOwnerOnly(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	bobs := w.owner(ScopeUser, w.bob, 0)
	w.create(bobs, w.bob, "closing")
	if _, err := w.store.Create(ctx, bobs, w.bob, input("closing")); code(err) != apierr.CodeSkillNameTaken {
		t.Fatalf("the same name twice: %v", err)
	}
	w.create(w.owner(ScopeUser, w.carol, 0), w.carol, "closing")
	w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "closing")
	w.create(w.owner(ScopeWorkspace, 0, 0), w.ann, "closing")
	if _, err := w.store.Create(ctx, w.owner(ScopeSpace, 0, w.team), w.bob, input("closing")); code(err) != apierr.CodeSkillNameTaken {
		t.Fatalf("the space's name twice: %v", err)
	}
	if _, err := w.store.Create(ctx, w.owner(ScopeWorkspace, 0, 0), w.ann, input("closing")); code(err) != apierr.CodeSkillNameTaken {
		t.Fatalf("the workspace's name twice: %v", err)
	}
	other := w.create(bobs, w.bob, "other")
	taken := "closing"
	if _, err := w.store.Update(ctx, bobs, w.bob, other.ID, Patch{Name: &taken}); code(err) != apierr.CodeSkillNameTaken {
		t.Fatalf("a rename onto another's name: %v", err)
	}
}

func TestAnOwnerHoldsAtMostSomeSkills(t *testing.T) {
	w := newWorld(t)
	o := w.owner(ScopeUser, w.bob, 0)
	for i := range MaxPerOwner {
		w.create(o, w.bob, fmt.Sprintf("skill-%03d", i))
	}
	if _, err := w.store.Create(context.Background(), o, w.bob, input("one-more")); code(err) != apierr.CodeTooManySkills {
		t.Fatalf("the skill over the cap: %v", err)
	}
	w.create(w.owner(ScopeUser, w.carol, 0), w.carol, "mine")
}

func TestForUserGivesExactlyWhatAPersonMayUse(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.create(w.owner(ScopeWorkspace, 0, 0), w.ann, "house-style")
	off := false
	hidden := w.create(w.owner(ScopeWorkspace, 0, 0), w.ann, "draft-only")
	if _, err := w.store.Update(ctx, w.owner(ScopeWorkspace, 0, 0), w.ann, hidden.ID, Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-way")
	w.create(w.owner(ScopeSpace, 0, w.secret), w.ann, "secret-way")
	w.create(w.owner(ScopeUser, w.bob, 0), w.bob, "bobs-own")
	w.create(w.owner(ScopeUser, w.carol, 0), w.carol, "carols-own")
	w.create(w.owner(ScopeUser, w.ann, 0), w.ann, "anns-own")
	offSpace := w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-off")
	if _, err := w.store.Update(ctx, w.owner(ScopeSpace, 0, w.team), w.bob, offSpace.ID, Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	w.create(Owner{Scope: ScopeWorkspace, WorkspaceID: w.other}, w.dave, "two-style")

	for name, c := range map[string]struct {
		user int64
		want []string
	}{
		"a member of the team space": {w.bob, []string{"workspace:house-style", "space:team-way", "user:bobs-own"}},
		"a reader of the team space": {w.carol, []string{"workspace:house-style", "space:team-way", "user:carols-own"}},
		"a member of the secret one": {w.ann, []string{"workspace:house-style", "space:secret-way", "user:anns-own"}},
	} {
		got, err := w.store.ForUser(ctx, w.ws, c.user)
		if err != nil || !slices.Equal(names(got), c.want) {
			t.Errorf("%s: %v, %v; want %v", name, names(got), err, c.want)
		}
	}

	if got, err := w.store.ForUser(ctx, w.ws, w.dave); err != nil || len(got) != 0 {
		t.Fatalf("a person of another workspace: %v, %v", names(got), err)
	}
	if got, err := w.store.ForUser(ctx, w.other, w.bob); err != nil || len(got) != 0 {
		t.Fatalf("a person asking for a workspace they are not in: %v, %v", names(got), err)
	}
	if got, err := w.store.ForUser(ctx, w.other, w.dave); err != nil || !slices.Equal(names(got), []string{"workspace:two-style"}) {
		t.Fatalf("workspace two: %v, %v", names(got), err)
	}
}

func TestForUserFollowsAccessAsItChanges(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-way")
	w.create(w.owner(ScopeUser, w.carol, 0), w.carol, "carols-own")
	list := func(user int64) []string {
		t.Helper()
		got, err := w.store.ForUser(ctx, w.ws, user)
		if err != nil {
			t.Fatal(err)
		}
		return names(got)
	}

	w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team, w.carol)
	if got := list(w.carol); !slices.Equal(got, []string{"user:carols-own"}) {
		t.Fatalf("after leaving the space: %v", got)
	}
	w.exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.bob)
	if got := list(w.bob); len(got) != 0 {
		t.Fatalf("a disabled account: %v", got)
	}
	w.exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, w.bob)
	if got := list(w.bob); !slices.Equal(got, []string{"space:team-way"}) {
		t.Fatalf("an enabled account again: %v", got)
	}

	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.carol)
	if n := w.scalar(`SELECT count(*) FROM skills WHERE user_id = ?`, w.carol); n != 0 {
		t.Fatalf("%d personal skills stay after leaving the workspace", n)
	}
	w.member(w.ws, w.carol, "member")
	if got := list(w.carol); len(got) != 0 {
		t.Fatalf("coming back brings the old skills back: %v", got)
	}
}

func TestASpaceThatTookAnotherSpacesIDDoesNotInheritItsSkills(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	old := w.owner(ScopeSpace, 0, w.secret)
	w.create(old, w.ann, "secret-way")

	conn, err := w.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		fmt.Sprintf(`DELETE FROM space_members WHERE space_id = %d`, w.secret),
		fmt.Sprintf(`DELETE FROM spaces WHERE id = %d`, w.secret),
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()

	again := w.space(w.ws, "Again")
	if again != w.secret {
		t.Fatalf("the new space has id %d, want the removed one's %d", again, w.secret)
	}
	w.join(again, w.ann, "editor")
	if gen := w.scalar(`SELECT generation FROM spaces WHERE id = ?`, again); gen == old.SpaceGeneration {
		t.Fatalf("the generation %d came twice", gen)
	}

	if got, err := w.store.ForUser(ctx, w.ws, w.ann); err != nil || len(got) != 0 {
		t.Fatalf("a skill of the space that had the ID: %v, %v", names(got), err)
	}
	now := w.owner(ScopeSpace, 0, again)
	if list, err := w.store.List(ctx, now, true); err != nil || len(list) != 0 {
		t.Fatalf("listing the new space's: %v, %v", names(list), err)
	}
	if _, err := w.store.Create(ctx, old, w.ann, input("late")); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a write for the generation the space no longer has: %v", err)
	}
}

func TestRemovingASpaceOrAWorkspaceRemovesItsSkills(t *testing.T) {
	w := newWorld(t)
	w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-way")
	w.create(w.owner(ScopeSpace, 0, w.secret), w.ann, "secret-way")
	w.exec(`DELETE FROM spaces WHERE id = ?`, w.team)
	if n := w.scalar(`SELECT count(*) FROM skills WHERE space_id = ?`, w.team); n != 0 {
		t.Fatalf("%d skills of a removed space", n)
	}
	if n := w.scalar(`SELECT count(*) FROM skills`); n != 1 {
		t.Fatalf("%d skills left, want the other space's", n)
	}
	eve := w.user("eve@example.com")
	w.exec(`INSERT INTO skills (workspace_id, scope, user_id, name, title, description, body) VALUES (?, 'user', ?, 'eves-own', 'T', 'D', 'B')`, w.ws, eve)
	w.exec(`DELETE FROM users WHERE id = ?`, eve)
	if n := w.scalar(`SELECT count(*) FROM skills WHERE scope = 'user'`); n != 0 {
		t.Fatalf("%d personal skills of a removed account", n)
	}
}

func TestOnlyAWriterOfASharedSpaceChangesItsSkills(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	o := w.owner(ScopeSpace, 0, w.team)
	made := w.create(o, w.bob, "team-way")
	title := "Hijacked"

	if _, err := w.store.Create(ctx, o, w.carol, input("by-reader")); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a reader creating: %v", err)
	}
	if _, err := w.store.Update(ctx, o, w.carol, made.ID, Patch{Title: &title}); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a reader updating: %v", err)
	}
	if err := w.store.Delete(ctx, o, w.carol, made.ID); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a reader deleting: %v", err)
	}
	if _, err := w.store.Create(ctx, o, w.dave, input("by-outsider")); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("an outsider creating: %v", err)
	}
	elsewhere := Owner{Scope: ScopeSpace, WorkspaceID: w.other, SpaceID: w.team, SpaceGeneration: o.SpaceGeneration}
	if _, err := w.store.Create(ctx, elsewhere, w.bob, input("by-name-of-another-workspace")); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a write that names another workspace for the space: %v", err)
	}
	w.exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.team)
	if _, err := w.store.Update(ctx, o, w.bob, made.ID, Patch{Title: &title}); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("an editor of an archived space updating: %v", err)
	}
	if got, err := w.get(o, made.ID); err != nil || got.Title == title {
		t.Fatalf("the skill after the refused writes: %+v, %v", got, err)
	}
	if len(w.events()) != 1 {
		t.Fatalf("refused writes left audit rows: %+v", w.events())
	}
}

func TestAPrivateSpaceHasNoSpaceSkills(t *testing.T) {
	w := newWorld(t)
	private := w.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, w.ws, w.bob)
	o := Owner{Scope: ScopeSpace, WorkspaceID: w.ws, SpaceID: private, SpaceGeneration: w.scalar(`SELECT generation FROM spaces WHERE id = ?`, private)}
	if _, err := w.store.Create(context.Background(), o, w.bob, input("in-private")); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a space skill in a private space: %v", err)
	}
}

func TestWorkspaceAndSpaceChangesAreAuditedAndPersonalOnesAreNot(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	mine := w.owner(ScopeUser, w.bob, 0)
	personal := w.create(mine, w.bob, "bobs-own")
	title := "Changed"
	if _, err := w.store.Update(ctx, mine, w.bob, personal.ID, Patch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Delete(ctx, mine, w.bob, personal.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.events(); len(got) != 0 {
		t.Fatalf("personal skills left audit rows: %+v", got)
	}

	team := w.owner(ScopeSpace, 0, w.team)
	wide := w.owner(ScopeWorkspace, 0, 0)
	s := w.create(team, w.bob, "team-way")
	body := "New instructions"
	if _, err := w.store.Update(ctx, team, w.bob, s.ID, Patch{Body: &body, Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Delete(ctx, team, w.bob, s.ID); err != nil {
		t.Fatal(err)
	}
	h := w.create(wide, w.ann, "house-style")
	if err := w.store.Delete(ctx, wide, w.ann, h.ID); err != nil {
		t.Fatal(err)
	}

	got := w.events()
	if len(got) != 5 {
		t.Fatalf("%d events: %+v", len(got), got)
	}
	slices.Reverse(got)
	want := []struct {
		action string
		actor  int64
		target int64
		scope  string
		name   string
		space  bool
	}{
		{audit.SkillCreated, w.bob, s.ID, "space", "team-way", true},
		{audit.SkillUpdated, w.bob, s.ID, "space", "team-way", true},
		{audit.SkillRemoved, w.bob, s.ID, "space", "team-way", true},
		{audit.SkillCreated, w.ann, h.ID, "workspace", "house-style", false},
		{audit.SkillRemoved, w.ann, h.ID, "workspace", "house-style", false},
	}
	for i, e := range want {
		r := got[i]
		if r.Action != e.action || r.TargetType != audit.TargetSkill || r.TargetID == nil || *r.TargetID != e.target || r.ActorID == nil || *r.ActorID != e.actor ||
			r.Details["scope"] != e.scope || r.Details["name"] != e.name {
			t.Errorf("event %d = %+v", i, r)
		}
		if _, has := r.Details["space_id"]; has != e.space {
			t.Errorf("event %d: space_id in %v", i, r.Details)
		}
		if r.TargetCreatedAt == nil {
			t.Errorf("event %d: no creation time of its target", i)
		}
		for k, v := range r.Details {
			if text, ok := v.(string); ok && (strings.Contains(text, "instructions") || strings.Contains(text, "Changed")) {
				t.Errorf("event %d: content in detail %s = %q", i, k, text)
			}
		}
	}
	if fields, _ := got[1].Details["fields"].([]any); len(fields) != 2 || fields[0] != "body" || fields[1] != "title" {
		t.Errorf("update fields = %v", got[1].Details["fields"])
	}

	if _, err := w.store.Create(ctx, wide, w.ann, Input{Name: "Bad"}); err == nil {
		t.Fatal("a refused create")
	}
	if _, err := w.store.Create(ctx, wide, w.ann, input("house-style")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Create(ctx, wide, w.ann, input("house-style")); code(err) != apierr.CodeSkillNameTaken {
		t.Fatalf("the same name again: %v", err)
	}
	if n := len(w.events()); n != 6 {
		t.Fatalf("%d events after refused requests, want 6", n)
	}
}

func TestTheSchemaKeepsEachSkillWithItsOwnerWhateverWritesIt(t *testing.T) {
	w := newWorld(t)
	private := w.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, w.ws, w.bob)
	generation := func(space int64) int64 { return w.scalar(`SELECT generation FROM spaces WHERE id = ?`, space) }
	otherSpace := w.space(w.other, "Elsewhere")

	insert := func(workspace int64, scope string, user, space, gen any, name string) error {
		_, err := w.db.Exec(`INSERT INTO skills (workspace_id, scope, user_id, space_id, space_generation, name, title, description, body)
			VALUES (?, ?, ?, ?, ?, ?, 'T', 'D', 'B')`, workspace, scope, user, space, gen, name)
		return err
	}
	for name, err := range map[string]error{
		"a space skill in a private space":                 insert(w.ws, "space", nil, private, generation(private), "one-a"),
		"a space skill in another workspace's space":       insert(w.ws, "space", nil, otherSpace, generation(otherSpace), "one-b"),
		"a space skill with the generation of another one": insert(w.ws, "space", nil, w.team, generation(w.team)+1, "one-c"),
		"a space skill with no generation":                 insert(w.ws, "space", nil, w.team, nil, "one-d"),
		"a personal skill with a space":                    insert(w.ws, "user", w.bob, w.team, nil, "one-e"),
		"a personal skill with no person":                  insert(w.ws, "user", nil, nil, nil, "one-f"),
		"a workspace skill with a person":                  insert(w.ws, "workspace", w.bob, nil, nil, "one-g"),
		"a name of one letter":                             insert(w.ws, "workspace", nil, nil, nil, "q"),
		"a scope that isn't one":                           insert(w.ws, "global", nil, nil, nil, "one-h"),
	} {
		if err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	if n := w.scalar(`SELECT count(*) FROM skills`); n != 0 {
		t.Fatalf("%d skills stored by refused writes", n)
	}

	made := w.create(w.owner(ScopeSpace, 0, w.team), w.bob, "team-way")
	for name, q := range map[string]string{
		"another workspace":  fmt.Sprintf(`UPDATE skills SET workspace_id = %d WHERE id = %d`, w.other, made.ID),
		"another scope":      fmt.Sprintf(`UPDATE skills SET scope = 'workspace', space_id = NULL, space_generation = NULL WHERE id = %d`, made.ID),
		"another space":      fmt.Sprintf(`UPDATE skills SET space_id = %d, space_generation = %d WHERE id = %d`, w.secret, generation(w.secret), made.ID),
		"another generation": fmt.Sprintf(`UPDATE skills SET space_generation = space_generation + 1 WHERE id = %d`, made.ID),
	} {
		if _, err := w.db.Exec(q); err == nil {
			t.Errorf("a skill moved to %s", name)
		}
	}
	got, err := w.get(w.owner(ScopeSpace, 0, w.team), made.ID)
	if err != nil || got.Name != "team-way" {
		t.Fatalf("the skill after the refused updates: %+v, %v", got, err)
	}
}

func TestStatusOfNotFoundIs404(t *testing.T) {
	if ErrNotFound.Status != http.StatusNotFound {
		t.Fatalf("status = %d", ErrNotFound.Status)
	}
}
