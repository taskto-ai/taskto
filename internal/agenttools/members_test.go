package agenttools

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// members returns "id:name:role" of each person list_members answered with.
func members(t *testing.T, out map[string]any) []string {
	t.Helper()
	var got []string
	for _, m := range out["members"].([]any) {
		m := m.(map[string]any)
		got = append(got, fmt.Sprintf("%d:%s:%s", id(m["id"]), m["name"], m["role"]))
		if m["email"] == "" {
			t.Fatalf("a member without an email: %v", m)
		}
	}
	return got
}

func TestListMembers(t *testing.T) {
	w := newWorld(t)
	want := func(got []string, expect ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(expect) {
			t.Fatalf("members = %v, want %v", got, expect)
		}
	}

	// A workspace's members: only that workspace's, whichever the user is in.
	want(members(t, w.ok(w.aliceConv, "list_members", map[string]any{})),
		fmt.Sprintf("%d:Alice:admin", w.alice), fmt.Sprintf("%d:Bob:member", w.bob))
	want(members(t, w.ok(w.bobConv, "list_members", map[string]any{})),
		fmt.Sprintf("%d:Alice:admin", w.alice), fmt.Sprintf("%d:Bob:member", w.bob))
	want(members(t, w.ok(w.aliceConv2, "list_members", map[string]any{})),
		fmt.Sprintf("%d:Alice:admin", w.alice), fmt.Sprintf("%d:Carol:member", w.carol))

	// A shared space's members, with their roles in it; a private space's owner alone.
	want(members(t, w.ok(w.aliceConv, "list_members", map[string]any{"space_id": w.shared})),
		fmt.Sprintf("%d:Alice:editor", w.alice), fmt.Sprintf("%d:Bob:viewer", w.bob))
	want(members(t, w.ok(w.bobConv, "list_members", map[string]any{"space_id": w.shared})),
		fmt.Sprintf("%d:Alice:editor", w.alice), fmt.Sprintf("%d:Bob:viewer", w.bob))
	want(members(t, w.ok(w.aliceConv, "list_members", map[string]any{"space_id": w.alicePrivate})),
		fmt.Sprintf("%d:Alice:owner", w.alice))
}

// list_members lists only the members of what the conversation's owner can read: not another
// user's private space, not a shared space they aren't in (a workspace admin included), not a
// space of another workspace, and a space that doesn't exist answers the same.
func TestListMembersCannotReachWhatTheOwnerCannotRead(t *testing.T) {
	w := newWorld(t)
	// A shared space of workspace one with bob only: alice, the workspace's admin, isn't in it.
	bobsOnly := w.insert(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Bob only') RETURNING id`, w.one)
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, bobsOnly, w.bob)
	carolConv := w.conversation(w.two, w.carol)

	missing := w.alicePrivate2 + 1000
	cases := []struct {
		name  string
		conv  int64
		space int64
	}{
		{"another user's private space", w.aliceConv, w.bobPrivate},
		{"another user's private space, the other way", w.bobConv, w.alicePrivate},
		{"a shared space the user isn't in, as the workspace's admin", w.aliceConv, bobsOnly},
		{"a space of another workspace", w.aliceConv, w.alicePrivate2},
		{"a space of another workspace, as its other user", carolConv, w.shared},
		{"a shared space of one, from a conversation in two", w.aliceConv2, w.shared},
		{"a space that doesn't exist", w.aliceConv, missing},
	}
	for _, tc := range cases {
		out := w.status(http.StatusNotFound, tc.conv, "list_members", map[string]any{"space_id": tc.space})
		if out["error"] != "not found" {
			t.Errorf("%s: %v", tc.name, out)
		}
	}

	// Carol, in workspace two only, never sees workspace one's people.
	for _, who := range members(t, w.ok(carolConv, "list_members", map[string]any{})) {
		if who == fmt.Sprintf("%d:Bob:member", w.bob) {
			t.Fatalf("carol sees bob: %v", who)
		}
	}
	// A member who has left the workspace gets nothing: the conversation is refused.
	w.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.one, w.bob)
	w.status(http.StatusForbidden, w.bobConv, "list_members", map[string]any{})
}

func TestListMembersRefusesBadArguments(t *testing.T) {
	w := newWorld(t)
	for _, args := range []map[string]any{{"space_id": "x"}, {"user_id": 1}, {"space_id": w.shared, "extra": true},
		{"cursor": "x"}, {"cursor": 1.5}, {"space_id": 1.5}} {
		w.status(http.StatusBadRequest, w.aliceConv, "list_members", args)
	}
}

// A space_id or cursor that is there but null, 0 or negative is refused: only leaving space_id
// out asks for the whole workspace, so a null from a model that fills every field never
// widens a call.
func TestListMembersRefusesNullAndNonPositiveIDs(t *testing.T) {
	w := newWorld(t)
	for _, field := range []string{"space_id", "cursor"} {
		for _, v := range []any{nil, 0, -1} {
			w.status(http.StatusBadRequest, w.aliceConv, "list_members", map[string]any{field: v})
			w.status(http.StatusBadRequest, w.aliceConv, "list_members", map[string]any{"space_id": w.shared, field: v})
		}
	}
	// Left out, they are the whole workspace's first page.
	if got := members(t, w.ok(w.aliceConv, "list_members", map[string]any{})); len(got) != 2 {
		t.Fatalf("members = %v", got)
	}
}

// bigWorkspace adds n people to workspace one, and returns the IDs of all its members,
// ascending.
func (w *world) bigWorkspace(n int) []int64 {
	w.t.Helper()
	w.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO users (email, name, password_hash) SELECT 'member' || i || '@example.com', 'x', 'x' FROM n`, n)
	w.exec(`INSERT INTO memberships (workspace_id, user_id, role)
		SELECT ?, id, 'member' FROM users WHERE email LIKE 'member%@example.com'`, w.one)
	rows, err := w.db.Query(`SELECT user_id FROM memberships WHERE workspace_id = ? ORDER BY user_id`, w.one)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			w.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	return ids
}

// pages lists every page of list_members for conv from the first, and returns the IDs, in the
// order they came, and how many pages there were.
func (w *world) pages(conv int64, args map[string]any) ([]int64, int) {
	w.t.Helper()
	var got []int64
	for page := 1; ; page++ {
		out := w.ok(conv, "list_members", args)
		for _, m := range out["members"].([]any) {
			got = append(got, id(m.(map[string]any)["id"]))
		}
		next, more := out["next_cursor"]
		if !more {
			return got, page
		}
		if page > 1000 {
			w.t.Fatal("pages without end")
		}
		args["cursor"] = id(next)
	}
}

// A workspace with more people than a reply holds is listed whole, a page at a time by user
// ID, whatever the size of a person: a reply that is cut short or has more after it carries
// next_cursor, and that is part of what is measured, so no size leaves a reply over the limit
// (as 875 people that fill it, and one more, once did).
func TestListMembersPagesThroughALargeWorkspace(t *testing.T) {
	w := newWorld(t)
	want := w.bigWorkspace(1300)
	sawCut, sawMore := false, false
	// The names' sizes sweep across the one at which a page's reply is exactly the limit.
	for nameLen := 1; nameLen <= 160; nameLen += 3 {
		w.exec(`UPDATE users SET name = substr(replace(hex(zeroblob(?)), '0', 'n'), 1, ?) WHERE email LIKE 'member%@example.com'`, nameLen, nameLen)
		got, pages := w.pages(w.aliceConv, map[string]any{})
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("name length %d: %d members in %d pages, want %d, each once, by id", nameLen, len(got), pages, len(want))
		}
		first := w.ok(w.aliceConv, "list_members", map[string]any{})
		if n := len(first["members"].([]any)); n < membersPage {
			sawCut = true // cut by the size, not by the page's number
		} else {
			sawMore = true
		}
	}
	if !sawCut || !sawMore {
		t.Fatalf("the sweep didn't cover both a reply cut by its size (%v) and one cut by its count (%v)", sawCut, sawMore)
	}

	// The last page has no next_cursor, and a cursor past everyone is an empty page.
	last := want[len(want)-1]
	out := w.ok(w.aliceConv, "list_members", map[string]any{"cursor": last})
	if _, more := out["next_cursor"]; more || len(out["members"].([]any)) != 0 {
		t.Fatalf("after the last = %v", out)
	}
}

// A shared space's people are paged by the same cursor.
func TestListMembersPagesASharedSpace(t *testing.T) {
	w := newWorld(t)
	ids := w.bigWorkspace(700)
	w.exec(`INSERT INTO space_members (space_id, user_id, role) SELECT ?, id, 'viewer' FROM users
		WHERE email LIKE 'member%@example.com'`, w.shared)
	got, pages := w.pages(w.aliceConv, map[string]any{"space_id": w.shared})
	if pages < 2 {
		t.Fatalf("%d people in %d pages", len(got), pages)
	}
	// alice and bob were in it from the start, with the 700 added.
	if len(got) != 702 || !slices.IsSorted(got) || len(ids) != 702 {
		t.Fatalf("%d people (workspace: %d), sorted: %v", len(got), len(ids), slices.IsSorted(got))
	}
}
