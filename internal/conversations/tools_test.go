package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestSendCarriesTheCapability(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	msgs := path(ws, fmt.Sprintf("/%d/messages", c.ID))

	owner.expect(http.StatusAccepted, http.MethodPost, msgs, map[string]any{"content": "before"})
	e.svc.SetCapabilities(func(id, agentID int64) string { return fmt.Sprintf("cap-%d-%d", id, agentID) })
	owner.expect(http.StatusAccepted, http.MethodPost, msgs, map[string]any{"content": "after"})

	e.rt.mu.Lock()
	inputs := e.rt.inputs[c.agentIDForTest(t, e)]
	e.rt.mu.Unlock()
	if len(inputs) != 2 || inputs[0].Capability != "" {
		t.Fatalf("inputs = %+v", inputs)
	}
	if want := fmt.Sprintf("cap-%d-%d", c.ID, c.agentIDForTest(t, e)); inputs[1].Capability != want {
		t.Fatalf("capability = %q, want %q", inputs[1].Capability, want)
	}
}

func TestPrincipalOf(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	c := member.create(ws, nil)
	ctx := context.Background()

	p, err := PrincipalOf(ctx, e.db, c.ID)
	if err != nil || p != (Principal{Conversation: c.ID, AgentConversation: c.agentIDForTest(t, e), Workspace: ws, User: member.id}) {
		t.Fatalf("PrincipalOf = %+v, %v", p, err)
	}
	inactive := func(why string) {
		t.Helper()
		if _, err := PrincipalOf(ctx, e.db, c.ID); !errors.Is(err, ErrInactive) {
			t.Fatalf("%s: PrincipalOf = %v, want ErrInactive", why, err)
		}
	}
	if _, err := PrincipalOf(ctx, e.db, c.ID+100); !errors.Is(err, ErrInactive) {
		t.Fatalf("unknown conversation: %v", err)
	}

	member.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true})
	inactive("archived")
	member.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": false})
	if _, err := PrincipalOf(ctx, e.db, c.ID); err != nil {
		t.Fatalf("unarchived: %v", err)
	}

	owner.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, member.id), nil)
	inactive("the owner left the workspace")
}

func TestOwns(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")
	ws2 := e.workspace(owner)
	c := owner.create(ws, nil)
	c2 := owner.create(ws2, nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name            string
		workspace, user int64
		id              int64
		want            bool
	}{
		{"the owner", ws, owner.id, c.ID, true},
		{"the owner, in the other workspace", ws2, owner.id, c2.ID, true},
		{"someone else", ws, other.id, c.ID, false},
		{"the owner's conversation of another workspace", ws, owner.id, c2.ID, false},
		{"one that doesn't exist", ws, owner.id, c2.ID + 1, false},
	} {
		got, err := Owns(ctx, e.db, tc.workspace, tc.user, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: Owns = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
