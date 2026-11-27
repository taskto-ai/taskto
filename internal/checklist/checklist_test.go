package checklist

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/db"
)

type world struct {
	t   *testing.T
	db  *sql.DB
	ws  int64
	ann int64
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
	w := &world{t: t, db: d}
	w.ann = w.user("ann@example.com")
	w.ws = w.workspace("One", w.ann)
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

func (w *world) workspace(name string, admin int64) int64 {
	id := w.scalar(`INSERT INTO workspaces (name) VALUES (?) RETURNING id`, name)
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, id, admin)
	return id
}

func (w *world) sharedSpace(ws int64) int64 {
	return w.scalar(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Team') RETURNING id`, ws)
}

func (w *world) app(space int64) int64 {
	return w.scalar(`INSERT INTO apps (space_id, name, definition) VALUES (?, 'A', '{}') RETURNING id`, space)
}

func (w *world) provider(ws int64) {
	w.exec(`INSERT INTO provider_accounts (workspace_id, name, display_name, kind, base_url, models)
		VALUES (?, 'p', 'P', 'openai-compatible', 'https://models.example.com/v1', '[]')`, ws)
}

func (w *world) view(svc *Service, ws, user int64) map[Item]State {
	w.t.Helper()
	v, err := svc.View(context.Background(), ws, user)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(v.Items) != len(Items) {
		w.t.Fatalf("items = %v", v.Items)
	}
	out := map[Item]State{}
	for i, e := range v.Items {
		if e.ID != Items[i] {
			w.t.Fatalf("items out of order: %v", v.Items)
		}
		out[e.ID] = e.State
	}
	return out
}

func TestANewWorkspaceHasEveryStepToDo(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	v, err := svc.View(context.Background(), w.ws, w.ann)
	if err != nil {
		t.Fatal(err)
	}
	if v.Dismissed || v.Complete {
		t.Fatalf("a new workspace: %+v", v)
	}
	for _, e := range v.Items {
		if e.State != Todo {
			t.Fatalf("%s = %s", e.ID, e.State)
		}
		if e.Optional != (e.ID == Email || e.ID == Backups) {
			t.Fatalf("%s optional = %v", e.ID, e.Optional)
		}
	}
}

func TestStepsAreDoneByWhatExists(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	ctx := context.Background()

	w.exec(`INSERT INTO invites (workspace_id, token_hash, email, role, expires_at) VALUES (?, 'h', 'b@example.com', 'member', '2999-01-01T00:00:00.000Z')`, w.ws)
	if got := w.view(svc, w.ws, w.ann)[Invite]; got != Done {
		t.Fatalf("a pending invite: %s", got)
	}
	w.exec(`DELETE FROM invites`)
	if got := w.view(svc, w.ws, w.ann)[Invite]; got != Todo {
		t.Fatalf("no invite: %s", got)
	}
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, w.user("b@example.com"))
	if got := w.view(svc, w.ws, w.ann)[Invite]; got != Done {
		t.Fatalf("a second member: %s", got)
	}

	w.provider(w.ws)
	if got := w.view(svc, w.ws, w.ann)[Provider]; got != Done {
		t.Fatalf("provider: %s", got)
	}
	w.exec(`DELETE FROM provider_accounts`)
	if got := w.view(svc, w.ws, w.ann)[Provider]; got != Todo {
		t.Fatalf("a removed provider is to do again: %s", got)
	}

	space := w.sharedSpace(w.ws)
	if got := w.view(svc, w.ws, w.ann)[Space]; got != Done {
		t.Fatalf("space: %s", got)
	}
	w.exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00Z' WHERE id = ?`, space)
	if got := w.view(svc, w.ws, w.ann)[Space]; got != Todo {
		t.Fatalf("an archived space doesn't count: %s", got)
	}
	w.exec(`UPDATE spaces SET archived_at = NULL WHERE id = ?`, space)

	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, space, w.ann)
	app := w.app(space)
	if got := w.view(svc, w.ws, w.ann)[App]; got != Done {
		t.Fatalf("app: %s", got)
	}
	w.exec(`UPDATE apps SET archived_at = '2026-01-01T00:00:00Z' WHERE id = ?`, app)
	if got := w.view(svc, w.ws, w.ann)[App]; got != Todo {
		t.Fatalf("an archived app doesn't count: %s", got)
	}

	v, err := svc.View(ctx, w.ws, w.ann)
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete {
		t.Fatalf("the app step is open: %+v", v)
	}
}

func TestOnlyAWorkspacesOwnDataCounts(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	other := w.workspace("Other", w.user("o@example.com"))
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, other, w.user("p@example.com"))
	w.provider(other)
	w.app(w.sharedSpace(other))
	for item, got := range w.view(svc, w.ws, w.ann) {
		if got != Todo {
			t.Fatalf("%s is %s from another workspace's data", item, got)
		}
	}
}

func TestAnotherPersonsPrivateAppDoesNotCount(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	bob := w.user("bob@example.com")
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, bob)
	private := func(user int64) int64 {
		return w.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, w.ws, user)
	}
	w.app(private(bob))
	if got := w.view(svc, w.ws, w.ann)[App]; got != Todo {
		t.Fatalf("a member's private app is theirs alone: %s", got)
	}
	w.app(private(w.ann))
	if got := w.view(svc, w.ws, w.ann)[App]; got != Done {
		t.Fatalf("the admin's own app: %s", got)
	}
}

func TestASharedSpaceTheAdminIsNotInDoesNotCount(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	space := w.sharedSpace(w.ws)
	w.app(space)
	if got := w.view(svc, w.ws, w.ann)[App]; got != Todo {
		t.Fatalf("an app in a shared space the admin can't read: %s", got)
	}
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, space, w.ann)
	if got := w.view(svc, w.ws, w.ann)[App]; got != Done {
		t.Fatalf("an app in a shared space the admin is in: %s", got)
	}
}

func TestOnlyAnOpenOrAcceptedInviteCounts(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	w.exec(`INSERT INTO invites (workspace_id, token_hash, email, role, expires_at) VALUES (?, 'h', 'b@example.com', 'member', '2000-01-01T00:00:00.000Z')`, w.ws)
	if got := w.view(svc, w.ws, w.ann)[Invite]; got != Todo {
		t.Fatalf("an expired unused invite: %s", got)
	}
	w.exec(`UPDATE invites SET accepted_at = '1999-12-31T00:00:00.000Z'`)
	if got := w.view(svc, w.ws, w.ann)[Invite]; got != Done {
		t.Fatalf("an accepted invite: %s", got)
	}
}

func TestServerStepsFollowTheServer(t *testing.T) {
	w := newWorld(t)
	email, backups := false, false
	svc := New(w.db, Options{
		Email:   func() bool { return email },
		Backups: func() (bool, error) { return backups, nil },
	})
	if got := w.view(svc, w.ws, w.ann); got[Email] != Todo || got[Backups] != Todo {
		t.Fatalf("%v", got)
	}
	email, backups = true, true
	if got := w.view(svc, w.ws, w.ann); got[Email] != Done || got[Backups] != Done {
		t.Fatalf("%v", got)
	}
	failing := New(w.db, Options{Backups: func() (bool, error) { return true, errors.New("unreadable") }})
	if got := w.view(failing, w.ws, w.ann)[Backups]; got != Todo {
		t.Fatalf("unreadable status: %s", got)
	}
}

func TestSkippingAndCompleting(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	ctx := context.Background()
	for _, item := range []Item{Invite, Provider, Space} {
		if err := svc.Skip(ctx, w.ws, w.ann, item, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Skip(ctx, w.ws, w.ann, Invite, true); err != nil {
		t.Fatal(err)
	}
	v, _ := svc.View(ctx, w.ws, w.ann)
	if v.Complete || v.Items[0].State != Skipped {
		t.Fatalf("one required step is open: %+v", v)
	}
	if err := svc.Skip(ctx, w.ws, w.ann, App, true); err != nil {
		t.Fatal(err)
	}
	v, _ = svc.View(ctx, w.ws, w.ann)
	if !v.Complete {
		t.Fatalf("every required step is handled, the optional ones don't hold it open: %+v", v)
	}
	w.provider(w.ws)
	if got := w.view(svc, w.ws, w.ann)[Provider]; got != Done {
		t.Fatalf("done beats skipped: %s", got)
	}
	if err := svc.Skip(ctx, w.ws, w.ann, Invite, false); err != nil {
		t.Fatal(err)
	}
	if v, _ = svc.View(ctx, w.ws, w.ann); v.Complete || v.Items[0].State != Todo {
		t.Fatalf("a skip taken back: %+v", v)
	}
}

func TestDismissing(t *testing.T) {
	w := newWorld(t)
	svc := New(w.db, Options{})
	ctx := context.Background()
	other := w.workspace("Other", w.ann)
	if err := svc.Dismiss(ctx, w.ws, w.ann, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Dismiss(ctx, w.ws, w.ann, true); err != nil {
		t.Fatalf("dismissing twice: %v", err)
	}
	if v, _ := svc.View(ctx, w.ws, w.ann); !v.Dismissed {
		t.Fatal("not dismissed")
	}
	if v, _ := svc.View(ctx, other, w.ann); v.Dismissed {
		t.Fatal("dismissing one workspace's checklist dismissed another's")
	}
	if err := svc.Dismiss(ctx, w.ws, w.ann, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.View(ctx, w.ws, w.ann); v.Dismissed {
		t.Fatal("still dismissed")
	}
	if err := svc.Dismiss(ctx, w.ws, w.ann, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Skip(ctx, w.ws, w.ann, Invite, true); err != nil {
		t.Fatal(err)
	}
	w.exec(`DELETE FROM workspaces WHERE id = ?`, w.ws)
	if n := w.scalar(`SELECT (SELECT COUNT(*) FROM setup_checklist_dismissed) + (SELECT COUNT(*) FROM setup_checklist_skipped)`); n != 0 {
		t.Fatalf("%d marks left of a deleted workspace", n)
	}
}

func TestParse(t *testing.T) {
	for _, i := range Items {
		if got, err := Parse(string(i)); err != nil || got != i {
			t.Fatalf("%s: %v %v", i, got, err)
		}
	}
	for _, bad := range []string{"", "Invite", "../invite", "backup"} {
		if _, err := Parse(bad); !errors.Is(err, ErrUnknownItem) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
