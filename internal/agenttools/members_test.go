package agenttools

import (
	"fmt"
	"net/http"
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
	for _, args := range []map[string]any{{"space_id": "x"}, {"user_id": 1}, {"space_id": w.shared, "extra": true}} {
		w.status(http.StatusBadRequest, w.aliceConv, "list_members", args)
	}
}
