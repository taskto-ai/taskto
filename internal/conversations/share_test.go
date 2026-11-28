package conversations

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit/audittest"
)

const privateTitle = "a title only the owner should see"

type apiError struct {
	Code string `json:"code"`
}

func (c *client) expectCode(status int, code, method, path string, body any) {
	c.e.t.Helper()
	rec := c.expect(status, method, path, body)
	if got := decode[apiError](c.e.t, rec).Code; got != code {
		c.e.t.Fatalf("%s %s: code = %q, want %q", method, path, got, code)
	}
}

func (e *env) auditRows(action string) []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT coalesce(actor_id, 0), coalesce(workspace_id, 0), target_type, coalesce(target_id, 0), details
		FROM audit_events WHERE action = ? ORDER BY id`, action)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var actor, workspace, target int64
		var targetType, details string
		if err := rows.Scan(&actor, &workspace, &targetType, &target, &details); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("actor=%d workspace=%d %s=%d %s", actor, workspace, targetType, target, details))
	}
	return out
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) addUser(ws int64, name string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(`INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x') RETURNING id`,
		name+"@example.com", name).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func members(t *testing.T, c *client, ws, id int64) []Member {
	t.Helper()
	return decode[Members](t, c.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/members", id)), nil)).Members
}

func ids(list []Conversation) []int64 {
	out := []int64{}
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}

func listOf(t *testing.T, c *client, ws int64, query string) []Conversation {
	t.Helper()
	return decode[struct{ Conversations []Conversation }](t, c.expect(http.StatusOK, http.MethodGet, path(ws, query), nil)).Conversations
}

func TestOwnerSharesWithAMemberToRead(t *testing.T) {
	e, srv, owner, ws, _ := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	outsider := e.invite(owner, ws, "outsider@example.com")
	conv := owner.create(ws, map[string]any{"title": privateTitle})
	one := path(ws, fmt.Sprintf("/%d", conv.ID))
	owner.expect(http.StatusAccepted, http.MethodPost, one+"/messages", map[string]any{"content": "the owner's question"})

	type route struct {
		method, suffix string
		body           any
	}
	routes := []route{
		{http.MethodGet, "", nil},
		{http.MethodGet, "/entries", nil},
		{http.MethodGet, "/messages", nil},
		{http.MethodGet, "/model", nil},
		{http.MethodGet, "/members", nil},
		{http.MethodGet, "/cli-permissions", nil},
		{http.MethodPatch, "", map[string]any{"title": "mine now"}},
		{http.MethodPost, "/messages", map[string]any{"content": "hi"}},
		{http.MethodPut, "/model", map[string]any{"provider": "script", "model_id": "script-1"}},
		{http.MethodPost, "/stop", nil},
		{http.MethodPost, "/notes", map[string]any{"content": "a note the agent never sees"}},
		{http.MethodPost, "/cli-permissions/abc", map[string]any{"allow": true}},
		{http.MethodPut, fmt.Sprintf("/members/%d", outsider.id), map[string]any{"role": "viewer"}},
		{http.MethodDelete, fmt.Sprintf("/members/%d", owner.id), nil},
	}
	for _, r := range routes {
		outsider.expectCode(http.StatusNotFound, "conversation_not_found", r.method, one+r.suffix, r.body)
		reader.expectCode(http.StatusNotFound, "conversation_not_found", r.method, one+r.suffix, r.body)
	}
	if got := listOf(t, reader, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("shared list before sharing = %v", ids(got))
	}

	share := path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id))
	got := decode[Member](t, owner.expect(http.StatusOK, http.MethodPut, share, map[string]any{"role": "viewer"}))
	if got.User.ID != reader.id || got.User.Name != "reader@example.com" || got.Role != "viewer" || got.AddedAt == "" {
		t.Fatalf("shared = %+v", got)
	}
	if rows := e.auditRows("conversation_shared"); len(rows) != 1 ||
		rows[0] != fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"role":"viewer","user_id":%d}`, owner.id, ws, conv.ID, reader.id) {
		t.Fatalf("audit rows = %q", rows)
	}
	if n := e.count(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = 'conversation_shared' AND workspace_id = ?`, reader.id, ws); n != 1 {
		t.Fatalf("%d notifications for the reader, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM notifications WHERE user_id = ?`, outsider.id); n != 0 {
		t.Fatalf("%d notifications for someone it wasn't shared with", n)
	}

	seen := decode[Conversation](t, reader.expect(http.StatusOK, http.MethodGet, one, nil))
	if seen.ID != conv.ID || seen.Role != "viewer" || seen.Owner != (Person{ID: owner.id, Name: "Owner"}) || seen.Title != privateTitle {
		t.Fatalf("the reader sees %+v", seen)
	}
	if mine := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, one, nil)); mine.Role != "owner" || mine.Owner.ID != owner.id {
		t.Fatalf("the owner sees %+v", mine)
	}
	for _, suffix := range []string{"/entries", "/messages", "/model", "/members"} {
		reader.expect(http.StatusOK, http.MethodGet, one+suffix, nil)
	}
	if got := ids(listOf(t, reader, ws, "?shared=true")); fmt.Sprint(got) != fmt.Sprint([]int64{conv.ID}) {
		t.Fatalf("shared list = %v", got)
	}
	if got := listOf(t, reader, ws, ""); len(got) != 0 {
		t.Fatalf("the reader's own list = %v, want only what they own", ids(got))
	}
	if got := listOf(t, owner, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("the owner's shared list = %v", ids(got))
	}
	if got := listOf(t, outsider, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("an outsider's shared list = %v", ids(got))
	}
	reader.expect(http.StatusBadRequest, http.MethodGet, path(ws, "?shared=maybe"), nil)
	if list := members(t, reader, ws, conv.ID); len(list) != 1 || list[0].User.ID != reader.id {
		t.Fatalf("members = %+v", list)
	}

	beforeAudit := e.count(`SELECT count(*) FROM audit_events`)
	for _, r := range routes[6:] {
		reader.expectCode(http.StatusForbidden, "conversation_read_only", r.method, one+r.suffix, r.body)
	}
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodGet, one+"/cli-permissions", nil)
	outsider.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, one+"/members", nil)
	if after := e.count(`SELECT count(*) FROM audit_events`); after != beforeAudit {
		t.Fatalf("refused requests recorded %d audit events", after-beforeAudit)
	}
	e.rt.mu.Lock()
	sent, aborted, model := len(e.rt.inputs[conv.agentIDForTest(t, e)]), e.rt.aborts[conv.agentIDForTest(t, e)], e.rt.models[conv.agentIDForTest(t, e)]
	noted := len(e.rt.notes[conv.agentIDForTest(t, e)])
	e.rt.mu.Unlock()
	if sent != 1 || aborted != 0 || model != defaultModel || noted != 0 {
		t.Fatalf("the reader changed the runtime: %d inputs, %d aborts, %d notes, model %+v", sent, aborted, noted, model)
	}
	if again := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, one, nil)); again.Title != privateTitle {
		t.Fatalf("title = %q", again.Title)
	}

	s := reader.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	w.send(t, `{"type":"delta","text":"live"}`)
	if got := s.next(); got.data != `{"type":"delta","text":"live"}` {
		t.Fatalf("the reader's stream = %+v", got)
	}
	s.cancel()

	owner.expect(http.StatusOK, http.MethodPut, share, map[string]any{"role": "viewer"})
	if rows := e.auditRows("conversation_shared"); len(rows) != 1 {
		t.Fatalf("sharing again recorded %q", rows)
	}
	if rows := e.auditRows("conversation_share_changed"); len(rows) != 0 {
		t.Fatalf("sharing again changed %q", rows)
	}
	if n := e.count(`SELECT count(*) FROM notifications WHERE user_id = ?`, reader.id); n != 1 {
		t.Fatalf("sharing again made %d notifications", n)
	}
	audittest.NoSecrets(t, e.db, map[string]string{"the conversation's title": privateTitle, "the owner's message": "the owner's question"})
}

func TestSharingIsTheOwnersAndChecksTheTarget(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	other := e.invite(owner, ws, "other@example.com")
	conv := owner.create(ws, nil)
	share := func(user int64) string { return path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, user)) }

	owner.expect(http.StatusBadRequest, http.MethodPut, share(owner.id), map[string]any{"role": "viewer"})
	owner.expect(http.StatusBadRequest, http.MethodPut, share(reader.id), map[string]any{"role": "participant"})
	owner.expect(http.StatusBadRequest, http.MethodPut, share(reader.id), map[string]any{"role": "admin"})
	owner.expect(http.StatusBadRequest, http.MethodPut, share(reader.id), map[string]any{})
	owner.expect(http.StatusBadRequest, http.MethodPut, share(reader.id), map[string]any{"role": "viewer", "extra": 1})

	elsewhere := e.workspace()
	stranger := e.addUser(elsewhere, "stranger")
	owner.expectCode(http.StatusNotFound, "member_not_found", http.MethodPut, share(stranger), map[string]any{"role": "viewer"})
	owner.expectCode(http.StatusNotFound, "member_not_found", http.MethodPut, share(9999), map[string]any{"role": "viewer"})
	owner.expectCode(http.StatusNotFound, "member_not_found", http.MethodPut, path(ws, fmt.Sprintf("/%d/members/x", conv.ID)), map[string]any{"role": "viewer"})
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 0 {
		t.Fatalf("%d shares after only refused requests", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action LIKE 'conversation_%'`); n != 0 {
		t.Fatalf("%d audit events after only refused requests", n)
	}

	owner.expect(http.StatusOK, http.MethodPut, share(reader.id), map[string]any{"role": "viewer"})
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodPut, share(other.id), map[string]any{"role": "viewer"})
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodDelete, share(owner.id), nil)
	owner.expect(http.StatusOK, http.MethodPut, share(other.id), map[string]any{"role": "viewer"})
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodDelete, share(other.id), nil)
	if got := members(t, owner, ws, conv.ID); len(got) != 2 {
		t.Fatalf("members = %+v", got)
	}

	owner.expectCode(http.StatusNotFound, "conversation_member_not_found", http.MethodDelete, share(stranger), nil)
	owner.expect(http.StatusNoContent, http.MethodDelete, share(other.id), nil)
	if rows := e.auditRows("conversation_unshared"); len(rows) != 1 ||
		rows[0] != fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"user_id":%d}`, owner.id, ws, conv.ID, other.id) {
		t.Fatalf("audit rows = %q", rows)
	}
	other.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil)

	reader.expect(http.StatusNoContent, http.MethodDelete, share(reader.id), nil)
	if rows := e.auditRows("conversation_unshared"); len(rows) != 2 ||
		rows[1] != fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"user_id":%d}`, reader.id, ws, conv.ID, reader.id) {
		t.Fatalf("audit rows = %q", rows)
	}
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil)
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodDelete, share(reader.id), nil)
	if got := members(t, owner, ws, conv.ID); len(got) != 0 {
		t.Fatalf("members = %+v", got)
	}
}

func TestAConversationOfAnotherWorkspaceStaysUnreachable(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})

	elsewhere := e.workspace(owner, reader)
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, path(elsewhere, fmt.Sprintf("/%d", conv.ID)), nil)
	if got := listOf(t, reader, elsewhere, "?shared=true"); len(got) != 0 {
		t.Fatalf("the other workspace lists %v", ids(got))
	}
}

func TestAShareEndsWithMembership(t *testing.T) {
	e := newEnv(t, false)
	admin, ws := e.setup()
	owner := e.invite(admin, ws, "owner2@example.com")
	reader := e.invite(admin, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", conv.ID))
	owner.expect(http.StatusOK, http.MethodPut, one+fmt.Sprintf("/members/%d", reader.id), map[string]any{"role": "viewer"})
	reader.expect(http.StatusOK, http.MethodGet, one, nil)

	admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, owner.id), nil)
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, one, nil)
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, one+"/events", nil)
	if got := listOf(t, reader, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("shared list with a disabled owner = %v", ids(got))
	}
	admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/enable", ws, owner.id), nil)
	reader.expect(http.StatusOK, http.MethodGet, one, nil)

	admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, reader.id), nil)
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 1 {
		t.Fatalf("disabling a member removed their share: %d left", n)
	}
	admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/enable", ws, reader.id), nil)

	owner.expect(http.StatusOK, http.MethodPost, "/api/login", map[string]string{"email": "owner2@example.com", "password": password})
	second := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", second.ID, reader.id)), map[string]any{"role": "viewer"})
	admin.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, reader.id), nil)
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 0 {
		t.Fatalf("a member who left the workspace keeps %d shares", n)
	}
	want := []string{
		fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"user_id":%d}`, admin.id, ws, conv.ID, reader.id),
		fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"user_id":%d}`, admin.id, ws, second.ID, reader.id),
	}
	if rows := e.auditRows("conversation_unshared"); fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("audit rows = %q, want one for each share the departure ended: %q", rows, want)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws, reader.id); err != nil {
		t.Fatal(err)
	}
	reader.expect(http.StatusOK, http.MethodPost, "/api/login", map[string]string{"email": "reader@example.com", "password": password})
	reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, one, nil)
	if got := listOf(t, reader, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("shared list after rejoining = %v", ids(got))
	}
}

func TestAConversationIsSharedWithAtMostFiftyPeople(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	share := func(user int64) string { return path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, user)) }
	var first int64
	for i := range maxMembers {
		id := e.addUser(ws, fmt.Sprintf("person%d", i))
		if i == 0 {
			first = id
		}
		owner.expect(http.StatusOK, http.MethodPut, share(id), map[string]any{"role": "viewer"})
	}
	extra := e.addUser(ws, "extra")
	owner.expectCode(http.StatusConflict, "conversation_members_limit", http.MethodPut, share(extra), map[string]any{"role": "viewer"})
	owner.expect(http.StatusOK, http.MethodPut, share(first), map[string]any{"role": "viewer"})
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != maxMembers {
		t.Fatalf("%d members, want %d", n, maxMembers)
	}
	if rows := e.auditRows("conversation_shared"); len(rows) != maxMembers {
		t.Fatalf("%d audit rows, want %d", len(rows), maxMembers)
	}
	owner.expect(http.StatusNoContent, http.MethodDelete, share(first), nil)
	owner.expect(http.StatusOK, http.MethodPut, share(extra), map[string]any{"role": "viewer"})
}

func TestChangingARoleIsAudited(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", conv.ID))
	if _, err := e.db.Exec(`INSERT INTO conversation_members (conversation_id, user_id, role, added_by) VALUES (?, ?, 'participant', ?)`,
		conv.ID, reader.id, owner.id); err != nil {
		t.Fatal(err)
	}
	reader.expect(http.StatusOK, http.MethodGet, one, nil)
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodPost, one+"/messages", map[string]any{"content": "hi"})

	got := decode[Member](t, owner.expect(http.StatusOK, http.MethodPut, one+fmt.Sprintf("/members/%d", reader.id), map[string]any{"role": "viewer"}))
	if got.Role != "viewer" {
		t.Fatalf("changed = %+v", got)
	}
	if rows := e.auditRows("conversation_share_changed"); len(rows) != 1 ||
		rows[0] != fmt.Sprintf(`actor=%d workspace=%d conversation=%d {"role":"viewer","user_id":%d}`, owner.id, ws, conv.ID, reader.id) {
		t.Fatalf("audit rows = %q", rows)
	}
	if rows := e.auditRows("conversation_shared"); len(rows) != 0 {
		t.Fatalf("a change recorded a share: %q", rows)
	}
}

func TestTheOwnerIsNeverAMemberOfTheirConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	if _, err := e.db.Exec(`INSERT INTO conversation_members (conversation_id, user_id, role) VALUES (?, ?, 'viewer')`, conv.ID, owner.id); err == nil {
		t.Fatal("the database took the owner as a member of their own conversation")
	}
}

func TestDeletingTheOwnerRemovesItsShares(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})
	if _, err := DeleteOwned(t.Context(), e.db, owner.id); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 0 {
		t.Fatalf("%d shares of a deleted conversation", n)
	}
}

func TestTheExportHoldsOnlyTheOwnersConversations(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	own := reader.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})
	got, err := e.svc.ExportList(t.Context(), ws, reader.id)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(got)) != fmt.Sprint([]int64{own.ID}) {
		t.Fatalf("the reader's export lists %v, want only %d", ids(got), own.ID)
	}
}

func TestASharedStreamEndsWhenTheShareDoes(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})
	e.svc.heartbeat = time.Hour
	ticks := make(chan time.Time)
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	s := reader.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Time{}:
		case <-time.After(10 * time.Second):
			t.Fatal("the stream took no heartbeat")
		}
	}
	tick()
	if got := s.next(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want a heartbeat while it is shared", got)
	}
	owner.expect(http.StatusNoContent, http.MethodDelete, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), nil)
	tick()
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)
}

func TestASharedStreamCountsAgainstTheViewer(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})
	e.svc.maxStreams = 1
	reader.openEvents(srv, ws, conv.ID)
	owner.openEvents(srv, ws, conv.ID)
	req, err := http.NewRequest(http.MethodGet, srv.URL+path(ws, fmt.Sprintf("/%d/events", conv.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(reader.cookie)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream of the viewer = %d, want 429", res.StatusCode)
	}
}

func TestAStaleOwnerCannotShareOrUnshareAnotherConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	other := e.invite(owner, ws, "other@example.com")
	conv := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", conv.ID))
	owner.expect(http.StatusOK, http.MethodPut, one+fmt.Sprintf("/members/%d", reader.id), map[string]any{"role": "viewer"})

	read, err := e.svc.reach(t.Context(), ws, owner.id, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, 9999, '')`,
		conv.ID, ws, other.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.share(t.Context(), read, owner.id, reader.id, RoleViewer); !errors.Is(err, errNotFound) {
		t.Fatalf("share on a conversation that changed hands = %v, want %v", err, errNotFound)
	}
	if err := e.svc.unshare(t.Context(), read, owner.id, reader.id); !errors.Is(err, errNotFound) {
		t.Fatalf("unshare on a conversation that changed hands = %v, want %v", err, errNotFound)
	}
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 0 {
		t.Fatalf("%d shares of the new conversation", n)
	}
}

func TestASharedConversationIsUnreachableWhileItsOwnerIsNotAMember(t *testing.T) {
	e := newEnv(t, false)
	admin, ws := e.setup()
	owner := e.invite(admin, ws, "owner2@example.com")
	reader := e.invite(admin, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", conv.ID))
	owner.expect(http.StatusOK, http.MethodPut, one+fmt.Sprintf("/members/%d", reader.id), map[string]any{"role": "viewer"})
	reader.expect(http.StatusOK, http.MethodGet, one, nil)

	admin.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, owner.id), nil)
	if n := e.count(`SELECT count(*) FROM conversation_members WHERE user_id = ?`, reader.id); n != 1 {
		t.Fatalf("the owner leaving removed the reader's share: %d left", n)
	}
	for _, suffix := range []string{"", "/entries", "/messages", "/model", "/members", "/events"} {
		reader.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodGet, one+suffix, nil)
	}
	if got := listOf(t, reader, ws, "?shared=true"); len(got) != 0 {
		t.Fatalf("shared list while the owner is not a member = %v", ids(got))
	}
}

func TestAViewerWhoIsDisabledOrNotAMemberReadsNothing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), map[string]any{"role": "viewer"})
	reads := func() error {
		_, err := e.svc.reach(t.Context(), ws, reader.id, conv.ID)
		return err
	}
	listed := func() int {
		got, err := e.svc.list(t.Context(), ws, reader.id, false, true)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if err := reads(); err != nil || listed() != 1 {
		t.Fatalf("the viewer can't read their share: %v, %d listed", err, listed())
	}

	if _, err := e.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, reader.id); err != nil {
		t.Fatal(err)
	}
	if err := reads(); !errors.Is(err, errNotFound) || listed() != 0 {
		t.Fatalf("a disabled viewer: %v, %d listed", err, listed())
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, reader.id); err != nil {
		t.Fatal(err)
	}
	if err := reads(); err != nil {
		t.Fatalf("enabling the viewer again: %v", err)
	}

	if _, err := e.db.Exec(`DROP TRIGGER conversation_members_leave_workspace`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws, reader.id); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM conversation_members WHERE user_id = ?`, reader.id); n != 1 {
		t.Fatalf("the share row went with the membership: %d left, so the read check is not what is tested", n)
	}
	if err := reads(); !errors.Is(err, errNotFound) || listed() != 0 {
		t.Fatalf("a viewer who left the workspace, with the row still there: %v, %d listed", err, listed())
	}
}

func TestAStaleReadCannotShareAConversationThatChangedItsAgentConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	conv := owner.create(ws, nil)
	read, err := e.svc.reach(t.Context(), ws, owner.id, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, ?, '')`,
		conv.ID, ws, owner.id, read.agentID+1000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.share(t.Context(), read, owner.id, reader.id, RoleViewer); !errors.Is(err, errNotFound) {
		t.Fatalf("share with a read of the replaced conversation = %v, want %v", err, errNotFound)
	}
	if n := e.count(`SELECT count(*) FROM conversation_members`); n != 0 {
		t.Fatalf("%d shares of the new conversation", n)
	}
}
