package desk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type storedDesk struct {
	layout  string
	version int64
}

func homeLayout(workspace int64, route string) []byte {
	return []byte(fmt.Sprintf(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[{"id":"t1","path":"/w/%d/%s"}]}}`, workspace, route))
}

func normalized(t *testing.T, workspace int64, raw []byte) string {
	t.Helper()
	checked, err := Check(workspace, raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(checked)
}

func addWorkspace(t *testing.T, sqlDB *sql.DB, name string) int64 {
	t.Helper()
	result, err := sqlDB.Exec(`INSERT INTO workspaces (name) VALUES (?)`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addUser(t *testing.T, sqlDB *sql.DB, email string) int64 {
	t.Helper()
	result, err := sqlDB.Exec(`INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'hash')`, email, email)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func join(t *testing.T, sqlDB *sql.DB, workspace, user int64) {
	t.Helper()
	if _, err := sqlDB.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, workspace, user); err != nil {
		t.Fatal(err)
	}
}

func storedOf(t *testing.T, sqlDB *sql.DB, id int64) storedDesk {
	t.Helper()
	var d storedDesk
	if err := sqlDB.QueryRow(`SELECT layout, version FROM desks WHERE id = ?`, id).Scan(&d.layout, &d.version); err != nil {
		t.Fatalf("desk %d: %v", id, err)
	}
	return d
}

func deskRows(t *testing.T, sqlDB *sql.DB, where string, args ...any) int {
	t.Helper()
	var count int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM desks WHERE `+where, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAStaleVersionAnswersDeskChangedAndAFreshOneSaves(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	first, second, third := homeLayout(1, "home"), homeLayout(1, "boards"), homeLayout(1, "apps")
	saved, err := service.SaveLayout(ctx, 1, 1, 0, 0, first)
	if err != nil || saved.Version != 1 {
		t.Fatalf("first save = %+v, err %v", saved, err)
	}
	refused := func(label string, id, version int64) {
		t.Helper()
		before := storedOf(t, sqlDB, saved.ID)
		current, err := service.SaveLayout(ctx, 1, 1, id, version, third)
		if !errors.Is(err, ErrChanged) {
			t.Fatalf("%s: err = %v, want desk changed", label, err)
		}
		if current.ID != saved.ID || current.Version != before.version || string(current.Layout) != before.layout {
			t.Fatalf("%s: current = %+v, stored %+v", label, current, before)
		}
		if after := storedOf(t, sqlDB, saved.ID); after != before {
			t.Fatalf("%s: a refused save changed the stored desk from %+v to %+v", label, before, after)
		}
	}
	refused("by id, older version", saved.ID, 0)
	refused("main, older version", 0, 0)
	refused("by id, version from the future", saved.ID, 9)
	refused("main, version from the future", 0, 9)
	refused("by id, negative version", saved.ID, -1)
	refused("main, negative version", 0, -1)

	version := saved.Version
	for i, step := range []struct {
		id     int64
		layout []byte
	}{{saved.ID, second}, {0, first}, {0, third}, {saved.ID, second}, {0, first}, {0, second}} {
		next, err := service.SaveLayout(ctx, 1, 1, step.id, version, step.layout)
		if err != nil || next.ID != saved.ID || next.Version != version+1 || string(next.Layout) != normalized(t, 1, step.layout) {
			t.Fatalf("fresh save %d (desk %d, version %d) = %+v, err %v", i, step.id, version, next, err)
		}
		if got := storedOf(t, sqlDB, saved.ID); got.version != next.Version {
			t.Fatalf("fresh save %d stored %+v", i, got)
		}
		version = next.Version
	}
	refused("main, the version just replaced", 0, version-1)
	refused("by id, the version just replaced", saved.ID, version-1)
}

func TestAStaleFirstSaveWritesNothing(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	for _, version := range []int64{1, 5, -1} {
		current, err := service.SaveLayout(ctx, 1, 1, 0, version, homeLayout(1, "home"))
		if !errors.Is(err, ErrChanged) || current.ID != 0 || current.Version != 0 {
			t.Fatalf("version %d: current = %+v, err %v", version, current, err)
		}
		if rows := deskRows(t, sqlDB, `1 = 1`); rows != 0 {
			t.Fatalf("version %d: a refused first save left %d rows", version, rows)
		}
	}
	if _, err := service.SaveLayout(ctx, 1, 1, 0, 0, []byte(`{"v":1,"focus":"x","root":null}`)); err == nil {
		t.Fatal("an invalid layout was saved")
	}
	if rows := deskRows(t, sqlDB, `1 = 1`); rows != 0 {
		t.Fatalf("an invalid first save left %d rows", rows)
	}
}

func TestTwoSavesOfTheSameVersionOnlyOneWins(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	saved, err := service.SaveLayout(ctx, 1, 1, 0, 0, homeLayout(1, "home"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, saved.ID} {
		start := storedOf(t, sqlDB, saved.ID).version
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, route := range []string{"boards", "apps"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := service.SaveLayout(ctx, 1, 1, id, start, homeLayout(1, route))
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		wins, changed := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrChanged):
				changed++
			default:
				t.Fatalf("desk %d: save error = %v", id, err)
			}
		}
		if wins != 1 || changed != 1 {
			t.Fatalf("desk %d: successes %d, conflicts %d", id, wins, changed)
		}
		if got := storedOf(t, sqlDB, saved.ID).version; got != start+1 {
			t.Fatalf("desk %d: version %d after two saves of version %d", id, got, start)
		}
	}
}

func TestEachMemberHasTheirOwnMainDeskInEachWorkspace(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	alice, bob := int64(1), addUser(t, sqlDB, "bob@example.test")
	join(t, sqlDB, 1, bob)
	second := addWorkspace(t, sqlDB, "Second")
	join(t, sqlDB, second, alice)
	join(t, sqlDB, second, bob)

	type key struct{ user, workspace int64 }
	layouts := map[key][]byte{
		{alice, 1}:      homeLayout(1, "home"),
		{bob, 1}:        homeLayout(1, "boards"),
		{alice, second}: homeLayout(second, "apps"),
	}
	ids := map[key]int64{}
	taken := map[int64]bool{}
	for k, raw := range layouts {
		saved, err := service.SaveLayout(ctx, k.workspace, k.user, 0, 0, raw)
		if err != nil || saved.ID == 0 || taken[saved.ID] {
			t.Fatalf("save %+v = %+v, err %v", k, saved, err)
		}
		ids[k], taken[saved.ID] = saved.ID, true
	}
	for k, raw := range layouts {
		got, err := service.Get(ctx, k.workspace, k.user, 0)
		if err != nil || got.ID != ids[k] || got.Version != 1 || string(got.Layout) != normalized(t, k.workspace, raw) {
			t.Fatalf("main desk of %+v = %+v, err %v", k, got, err)
		}
		list, err := service.List(ctx, k.workspace, k.user)
		if err != nil || len(list.Desks) != 1 || list.Desks[0].ID != ids[k] {
			t.Fatalf("list of %+v = %+v, err %v", k, list, err)
		}
	}
	virtual, err := service.Get(ctx, second, bob, 0)
	if err != nil || virtual.ID != 0 || virtual.Version != 0 || string(virtual.Layout) != string(emptyLayout) {
		t.Fatalf("a member with no desk in a workspace sees %+v, err %v", virtual, err)
	}
	list, err := service.List(ctx, second, bob)
	if err != nil || len(list.Desks) != 1 || list.Desks[0].ID != 0 {
		t.Fatalf("a member with no desk in a workspace lists %+v, err %v", list, err)
	}

	untouched := func(except key) {
		t.Helper()
		for k, raw := range layouts {
			if k == except {
				continue
			}
			want := storedDesk{layout: normalized(t, k.workspace, raw), version: 1}
			if got := storedOf(t, sqlDB, ids[k]); got != want {
				t.Fatalf("saving the desk of %+v changed the desk of %+v: %+v", except, k, got)
			}
		}
	}
	if _, err := service.SaveLayout(ctx, 1, bob, 0, 1, homeLayout(1, "settings")); err != nil {
		t.Fatal(err)
	}
	untouched(key{bob, 1})
	if got := storedOf(t, sqlDB, ids[key{bob, 1}]); got.version != 2 {
		t.Fatalf("bob's own desk = %+v", got)
	}
	if _, err := service.SaveLayout(ctx, 1, alice, 0, 1, homeLayout(1, "notifications")); err != nil {
		t.Fatal(err)
	}
	if got := storedOf(t, sqlDB, ids[key{alice, second}]); got.version != 1 || got.layout != normalized(t, second, layouts[key{alice, second}]) {
		t.Fatalf("saving alice's desk of one workspace changed her desk of another: %+v", got)
	}
	if got := storedOf(t, sqlDB, ids[key{bob, 1}]); got.version != 2 || got.layout != normalized(t, 1, homeLayout(1, "settings")) {
		t.Fatalf("saving alice's desk changed bob's: %+v", got)
	}
}

func TestAnotherPersonsOrWorkspacesDeskIsNeverReadOrWritten(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	alice, bob := int64(1), addUser(t, sqlDB, "bob@example.test")
	join(t, sqlDB, 1, bob)
	second := addWorkspace(t, sqlDB, "Second")
	join(t, sqlDB, second, alice)
	saved, err := service.SaveLayout(ctx, 1, alice, 0, 0, homeLayout(1, "home"))
	if err != nil {
		t.Fatal(err)
	}
	want := storedOf(t, sqlDB, saved.ID)
	attempts := []struct {
		label     string
		workspace int64
		user      int64
	}{{"another member", 1, bob}, {"another workspace", second, alice}}
	for _, a := range attempts {
		if _, err := service.Get(ctx, a.workspace, a.user, saved.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s read: err = %v", a.label, err)
		}
		if _, err := service.SaveLayout(ctx, a.workspace, a.user, saved.ID, 1, homeLayout(a.workspace, "boards")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s write: err = %v", a.label, err)
		}
		checked, err := Check(a.workspace, homeLayout(a.workspace, "boards"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.updateLayout(ctx, a.workspace, a.user, saved.ID, 1, checked); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s update: err = %v", a.label, err)
		}
		if got := storedOf(t, sqlDB, saved.ID); got != want {
			t.Fatalf("%s changed the desk from %+v to %+v", a.label, want, got)
		}
	}
}

func TestLeavingAWorkspaceRemovesOnlyThatPersonsDesksThere(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	alice, bob := int64(1), addUser(t, sqlDB, "bob@example.test")
	join(t, sqlDB, 1, bob)
	second := addWorkspace(t, sqlDB, "Second")
	join(t, sqlDB, second, alice)
	join(t, sqlDB, second, bob)
	for _, k := range []struct{ user, workspace int64 }{{alice, 1}, {alice, second}, {bob, 1}, {bob, second}} {
		if _, err := service.SaveLayout(ctx, k.workspace, k.user, 0, 0, homeLayout(k.workspace, "home")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, second, alice); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		user, workspace int64
		want            int
	}{{alice, second, 0}, {alice, 1, 1}, {bob, 1, 1}, {bob, second, 1}} {
		if got := deskRows(t, sqlDB, `user_id = ? AND workspace_id = ?`, c.user, c.workspace); got != c.want {
			t.Fatalf("after alice left the second workspace, desks of user %d in workspace %d = %d, want %d", c.user, c.workspace, got, c.want)
		}
	}
	if _, err := sqlDB.Exec(`DELETE FROM spaces WHERE owner_id = ?`, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM users WHERE id = ?`, bob); err != nil {
		t.Fatal(err)
	}
	if got := deskRows(t, sqlDB, `user_id = ?`, bob); got != 0 {
		t.Fatalf("a deleted account left %d desks", got)
	}
	if got := deskRows(t, sqlDB, `user_id = ?`, alice); got != 1 {
		t.Fatalf("deleting another account changed alice's desks: %d", got)
	}
	if _, err := sqlDB.Exec(`DELETE FROM spaces WHERE workspace_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM workspaces WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := deskRows(t, sqlDB, `workspace_id = 1`); got != 0 {
		t.Fatalf("a deleted workspace left %d desks", got)
	}
}
