package audit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
)

// event inserts an event at a time, as the log would hold it.
func event(t *testing.T, d *sql.DB, ws, actor int64, at, action string) int64 {
	t.Helper()
	return exec(t, d, `INSERT INTO audit_events (at, workspace_id, actor_id, action) VALUES (?, ?, ?, ?)`,
		at, nullable(ws), nullable(actor), action)
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func TestListWorkspaceNeedsAWorkspace(t *testing.T) {
	d := open(t)
	ws := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Home')`)
	event(t, d, ws, 0, "2026-01-01T00:00:00.000Z", audit.SpaceCreated)
	event(t, d, 0, 0, "2026-01-01T00:00:01.000Z", audit.LoginFailed)

	for _, bad := range []int64{0, -1} {
		page, err := audit.ListWorkspace(context.Background(), d, bad, audit.Query{})
		if !errors.Is(err, audit.ErrNoWorkspace) || len(page.Events) != 0 {
			t.Fatalf("workspace %d: %+v, %v", bad, page, err)
		}
	}
}

func TestListWorkspaceHoldsOnlyItsWorkspacesEvents(t *testing.T) {
	d := open(t)
	one := exec(t, d, `INSERT INTO workspaces (name) VALUES ('One')`)
	two := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Two')`)
	mine := event(t, d, one, 0, "2026-01-01T00:00:00.000Z", audit.SpaceCreated)
	event(t, d, two, 0, "2026-01-01T00:00:01.000Z", audit.SpaceCreated)
	event(t, d, 0, 0, "2026-01-01T00:00:02.000Z", audit.LoginSucceeded)

	page, err := audit.ListWorkspace(context.Background(), d, one, audit.Query{})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != mine {
		t.Fatalf("page = %+v, %v", page, err)
	}
	// The unrestricted List the tests use does see all three: the view must not be it.
	if all, _ := audit.List(context.Background(), d, audit.Filter{}); len(all) != 3 {
		t.Fatalf("List = %d events", len(all))
	}
}

func TestListWorkspaceFiltersAndPages(t *testing.T) {
	d := open(t)
	ws := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Home')`)
	a := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'A', '')`)
	b := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('b@example.com', 'B', '')`)
	// Five events at one instant and two later: the cursor must not skip or repeat the ties.
	var ids []int64
	for range 5 {
		ids = append(ids, event(t, d, ws, a, "2026-01-01T00:00:00.000Z", audit.SpaceCreated))
	}
	ids = append(ids, event(t, d, ws, b, "2026-01-02T00:00:00.000Z", audit.MemberRemoved))
	ids = append(ids, event(t, d, ws, b, "2026-01-03T00:00:00.000Z", audit.SpaceCreated))
	exec(t, d, `UPDATE audit_events SET target_type = 'user' WHERE action = 'member_removed'`)

	ctx := context.Background()
	var got []int64
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := audit.ListWorkspace(ctx, d, ws, audit.Query{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			got = append(got, e.ID)
		}
		if page.Next == "" {
			break
		}
		if pages > 5 {
			t.Fatal("the cursor doesn't end")
		}
		cursor = page.Next
	}
	want := []int64{ids[6], ids[5], ids[4], ids[3], ids[2], ids[1], ids[0]}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged %v, want %v", got, want)
	}

	count := func(q audit.Query) int {
		t.Helper()
		page, err := audit.ListWorkspace(ctx, d, ws, q)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Events)
	}
	day := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for name, c := range map[string]struct {
		q    audit.Query
		want int
	}{
		"action":      {audit.Query{Action: audit.SpaceCreated}, 6},
		"actor":       {audit.Query{Actor: b}, 2},
		"target type": {audit.Query{TargetType: "user"}, 1},
		"all filters": {audit.Query{Action: audit.SpaceCreated, Actor: b, TargetType: ""}, 1},
		"from":        {audit.Query{From: day("2026-01-02T00:00:00Z")}, 2},
		"to":          {audit.Query{To: day("2026-01-02T00:00:00Z")}, 5},
		"from and to": {audit.Query{From: day("2026-01-02T00:00:00Z"), To: day("2026-01-03T00:00:00Z")}, 1},
		"limit":       {audit.Query{Limit: 2}, 2},
	} {
		if n := count(c.q); n != c.want {
			t.Errorf("%s: %d events, want %d", name, n, c.want)
		}
	}
	if _, err := audit.ListWorkspace(ctx, d, ws, audit.Query{Cursor: "not a cursor"}); !errors.Is(err, audit.ErrBadCursor) {
		t.Fatalf("a bad cursor: %v", err)
	}
	if page, _ := audit.ListWorkspace(ctx, d, ws, audit.Query{Limit: 100000}); len(page.Events) != 7 {
		t.Fatalf("a large limit: %d events", len(page.Events))
	}
}

func TestListWorkspaceNamesOnlyWhatBelongsToTheWorkspace(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	one := exec(t, d, `INSERT INTO workspaces (name) VALUES ('One')`)
	two := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Two')`)
	member := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('m@example.com', 'Member', '')`)
	left := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('l@example.com', 'Left', '')`)
	exec(t, d, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, one, member)
	exec(t, d, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, two, left)
	memberCreated := text(t, d, `SELECT created_at FROM users WHERE id = ?`, member)
	leftCreated := text(t, d, `SELECT created_at FROM users WHERE id = ?`, left)
	space := exec(t, d, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Team')`, one)
	otherSpace := exec(t, d, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Their secret plan')`, two)
	spaceCreated := text(t, d, `SELECT created_at FROM spaces WHERE id = ?`, space)
	otherCreated := text(t, d, `SELECT created_at FROM spaces WHERE id = ?`, otherSpace)

	record := func(actor int64, typ string, target int64, created string) {
		t.Helper()
		exec(t, d, `INSERT INTO audit_events (workspace_id, actor_id, action, target_type, target_id, target_created_at)
			VALUES (?, ?, 'space_updated', ?, ?, ?)`, one, nullable(actor), typ, target, nullable2(created))
	}
	record(member, "space", space, spaceCreated)               // named
	record(left, "space", otherSpace, otherCreated)            // a user and a space of workspace two
	record(0, "space", space, "2020-01-01T00:00:00.000Z")      // the id of another object, made earlier
	record(0, "space", space, "")                              // no creation time
	record(member, "user", member, memberCreated)              // a member
	record(member, "user", left, leftCreated)                  // a user of another workspace
	record(member, "user", member, "2020-01-01T00:00:00.000Z") // another user's id, once
	record(member, "workspace", one, "")
	record(member, "workspace", two, "")

	page, err := audit.ListWorkspace(ctx, d, one, audit.Query{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	type shown struct {
		actor, user, name string
	}
	var got []shown
	for i := len(page.Events) - 1; i >= 0; i-- { // oldest first, as recorded
		e := page.Events[i]
		s := shown{}
		if e.Actor != nil {
			s.actor = e.Actor.Name
		}
		if e.TargetUser != nil {
			s.user = e.TargetUser.Name
		}
		if e.TargetName != nil {
			s.name = *e.TargetName
		}
		got = append(got, s)
	}
	want := []shown{
		{"Member", "", "Team"},
		{"", "", ""},
		{"", "", ""},
		{"", "", ""},
		{"Member", "Member", ""},
		{"Member", "", ""},
		{"Member", "", ""},
		{"Member", "", "One"},
		{"Member", "", ""},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("names = %+v\nwant    %+v", got, want)
	}
}

func text(t *testing.T, d *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := d.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func nullable2(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestListWorkspaceBoundsWithFinerDigitsKeepMillisecondEvents(t *testing.T) {
	d := open(t)
	ws := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Home')`)
	event(t, d, ws, 0, "2026-01-01T00:00:00.123Z", audit.SpaceCreated)
	bound, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00.1235Z")
	if err != nil {
		t.Fatal(err)
	}
	count := func(q audit.Query) int {
		t.Helper()
		page, err := audit.ListWorkspace(context.Background(), d, ws, q)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Events)
	}
	// An event at .123 is before .1235: out of "from" it, in "to" it.
	if n := count(audit.Query{From: bound}); n != 0 {
		t.Errorf("from .1235 includes the event at .123 (%d)", n)
	}
	if n := count(audit.Query{To: bound}); n != 1 {
		t.Errorf("to .1235 leaves out the event at .123 (%d)", n)
	}
	exact := bound.Add(-500 * time.Microsecond)
	if count(audit.Query{From: exact}) != 1 || count(audit.Query{To: exact}) != 0 {
		t.Error("a bound at the event's own millisecond is from-inclusive, to-exclusive")
	}
}
