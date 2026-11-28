package conversations

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

func (e *env) replaceConversation(id, ws int64, owner, reader *client) {
	e.t.Helper()
	if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, 9999, '')`, id, ws, owner.id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO conversation_members (conversation_id, user_id, role, added_by) VALUES (?, ?, 'viewer', ?)`, id, reader.id, owner.id); err != nil {
		e.t.Fatal(err)
	}
}

func TestAViewerStreamEndsWhenAnotherConversationTakesItsID(t *testing.T) {
	const old = `{"type":"ops","ops":[["a",["docs","pi.live","generation","message","content",0,"text"],"the removed conversation's output"]]}`
	e, srv, admin, ws, conv := newStreamEnv(t)
	other := e.invite(admin, ws, "other@example.com")
	reader := e.invite(admin, ws, "reader@example.com")
	e.shareWith(admin, ws, conv.ID, reader)
	e.svc.heartbeat = time.Hour
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	s := reader.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	e.replaceConversation(conv.ID, ws, other, reader)
	w.send(t, old)
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)
}

func TestAReadInProgressWritesNothingOfAConversationThatLostItsID(t *testing.T) {
	for _, op := range []string{"entries", "messages", "model"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t, false)
			admin, ws := e.setup()
			other := e.invite(admin, ws, "other@example.com")
			reader := e.invite(admin, ws, "reader@example.com")
			conv := admin.create(ws, nil)
			e.rt.mu.Lock()
			e.rt.inputs[conv.agentIDForTest(t, e)] = []agentrt.Input{{Content: "the removed conversation's question"}}
			e.rt.mu.Unlock()
			e.shareWith(admin, ws, conv.ID, reader)
			var once sync.Once
			e.rt.onRead = func(got string) {
				if got == op {
					once.Do(func() { e.replaceConversation(conv.ID, ws, other, reader) })
				}
			}
			rec := reader.do(http.MethodGet, path(ws, fmt.Sprintf("/%d/%s", conv.ID, op)), nil)
			if rec.Code != http.StatusNotFound || decode[apiError](t, rec).Code != "conversation_not_found" {
				t.Fatalf("a read of a conversation that lost its ID = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnAdmittedRosterReadRefusesAViewerWhoWasUnshared(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	other := e.invite(owner, ws, "other@example.com")
	conv := owner.create(ws, nil)
	e.shareWith(owner, ws, conv.ID, reader)
	e.shareWith(owner, ws, conv.ID, other)
	admitted, err := e.svc.reach(t.Context(), ws, reader.id, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner.expect(http.StatusNoContent, http.MethodDelete, path(ws, fmt.Sprintf("/%d/members/%d", conv.ID, reader.id)), nil)
	if got, err := e.svc.members(t.Context(), admitted); err == nil {
		t.Fatalf("the roster of a viewer who was unshared = %+v, want a refusal", got)
	}
}
