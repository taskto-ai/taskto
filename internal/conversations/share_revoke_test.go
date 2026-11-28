package conversations

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

func (e *env) shareWith(owner *client, ws, conv int64, reader *client) {
	e.t.Helper()
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/members/%d", conv, reader.id)), map[string]any{"role": "viewer"})
}

func TestAViewerStreamReceivesNothingAfterAccessEnds(t *testing.T) {
	const live = `{"type":"ops","ops":[["a",["docs","pi.live","generation","message","content",0,"text"],"new conversation output"]]}`
	removals := map[string]func(admin, owner, reader *client, ws, conv int64){
		"the viewer is removed from the workspace": func(admin, _, reader *client, ws, _ int64) {
			admin.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, reader.id), nil)
		},
		"the owner is removed from the workspace": func(admin, owner, _ *client, ws, _ int64) {
			admin.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, owner.id), nil)
		},
		"the viewer is disabled": func(admin, _, reader *client, ws, _ int64) {
			admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, reader.id), nil)
		},
		"the owner is disabled": func(admin, owner, _ *client, ws, _ int64) {
			admin.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, owner.id), nil)
		},
		"the share is removed": func(_, owner, reader *client, ws, conv int64) {
			owner.expect(http.StatusNoContent, http.MethodDelete, path(ws, fmt.Sprintf("/%d/members/%d", conv, reader.id)), nil)
		},
	}
	for name, remove := range removals {
		t.Run(name, func(t *testing.T) {
			e, srv, admin, ws, _ := newStreamEnv(t)
			owner := e.invite(admin, ws, "owner2@example.com")
			reader := e.invite(admin, ws, "reader@example.com")
			conv := owner.create(ws, nil)
			e.shareWith(owner, ws, conv.ID, reader)
			e.svc.heartbeat = time.Hour
			e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
			s := reader.openEvents(srv, ws, conv.ID)
			w := e.rt.watch(t, 0)
			remove(admin, owner, reader, ws, conv.ID)
			w.send(t, live)
			s.expectEnd(interruptedLine)
			waitFor(t, "the watch to close", w.isClosed)
		})
	}
}

func TestAReadInProgressWritesNothingAfterAccessEnds(t *testing.T) {
	for _, op := range []string{"entries", "messages", "model"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t, false)
			admin, ws := e.setup()
			owner := e.invite(admin, ws, "owner2@example.com")
			reader := e.invite(admin, ws, "reader@example.com")
			conv := owner.create(ws, nil)
			e.rt.mu.Lock()
			e.rt.inputs[conv.agentIDForTest(t, e)] = []agentrt.Input{{Content: "the owner's question"}}
			e.rt.mu.Unlock()
			e.shareWith(owner, ws, conv.ID, reader)
			var once sync.Once
			e.rt.onRead = func(got string) {
				if got == op {
					once.Do(func() {
						admin.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, reader.id), nil)
					})
				}
			}
			rec := reader.do(http.MethodGet, path(ws, fmt.Sprintf("/%d/%s", conv.ID, op)), nil)
			if rec.Code != http.StatusNotFound || rec.Body.Len() == 0 || decode[apiError](t, rec).Code != "conversation_not_found" {
				t.Fatalf("a read that outlived the viewer's access = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAStaleActorCannotWriteAShare(t *testing.T) {
	disable := func(e *env, user int64) {
		if _, err := e.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, user); err != nil {
			e.t.Fatal(err)
		}
	}
	leave := func(e *env, ws, user int64) {
		if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws, user); err != nil {
			e.t.Fatal(err)
		}
	}
	for name, end := range map[string]func(e *env, ws, owner, reader int64){
		"owner disabled":  func(e *env, _, owner, _ int64) { disable(e, owner) },
		"owner left":      func(e *env, ws, owner, _ int64) { leave(e, ws, owner) },
		"viewer disabled": func(e *env, _, _, reader int64) { disable(e, reader) },
		"viewer left":     func(e *env, ws, _, reader int64) { leave(e, ws, reader) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, false)
			admin, ws := e.setup()
			owner := e.invite(admin, ws, "owner2@example.com")
			reader := e.invite(admin, ws, "reader@example.com")
			other := e.invite(admin, ws, "other@example.com")
			conv := owner.create(ws, nil)
			e.shareWith(owner, ws, conv.ID, reader)
			asOwner, err := e.svc.reach(t.Context(), ws, owner.id, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			asReader, err := e.svc.reach(t.Context(), ws, reader.id, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			end(e, ws, owner.id, reader.id)
			ownerGone := name == "owner disabled" || name == "owner left"
			viewerGone := name == "viewer disabled" || name == "viewer left"
			audits, notices := e.count(`SELECT count(*) FROM audit_events`), e.count(`SELECT count(*) FROM notifications`)

			if ownerGone {
				if _, err := e.svc.share(t.Context(), asOwner, owner.id, other.id, RoleViewer); !errors.Is(err, errNotFound) {
					t.Fatalf("share by an owner who lost access = %v, want %v", err, errNotFound)
				}
				if err := e.svc.unshare(t.Context(), asOwner, owner.id, reader.id); !errors.Is(err, errNotFound) {
					t.Fatalf("unshare by an owner who lost access = %v, want %v", err, errNotFound)
				}
				if err := e.svc.unshare(t.Context(), asReader, reader.id, reader.id); !errors.Is(err, errNotFound) {
					t.Fatalf("a viewer removing themself from a conversation whose owner left = %v, want %v", err, errNotFound)
				}
			}
			if viewerGone {
				err := e.svc.unshare(t.Context(), asReader, reader.id, reader.id)
				if !errors.Is(err, errNotFound) && (name != "viewer left" || !errors.Is(err, errNotShared)) {
					t.Fatalf("a viewer who lost access removing themself = %v", err)
				}
			}
			if after := e.count(`SELECT count(*) FROM audit_events`); after != audits {
				t.Fatalf("refused writes recorded %d audit events", after-audits)
			}
			if after := e.count(`SELECT count(*) FROM notifications`); after != notices {
				t.Fatalf("refused writes made %d notifications", after-notices)
			}
			if n := e.count(`SELECT count(*) FROM conversation_members WHERE user_id = ?`, other.id); n != 0 {
				t.Fatalf("a refused share left %d rows", n)
			}
		})
	}
}

func TestARosterReadBelongsToTheConversationThatWasAuthorized(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		otherWorkspace, otherOwner bool
		otherAgentConversation     bool
	}{
		{name: "only the workspace differs", otherWorkspace: true},
		{name: "only the owner differs", otherOwner: true},
		{name: "only the agent conversation differs", otherAgentConversation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, false)
			owner, ws := e.setup()
			reader := e.invite(owner, ws, "reader@example.com")
			other := e.invite(owner, ws, "other@example.com")
			conv := owner.create(ws, nil)
			e.shareWith(owner, ws, conv.ID, reader)
			read, err := e.svc.reach(t.Context(), ws, reader.id, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := e.svc.members(t.Context(), read); err != nil || len(got.Members) != 1 {
				t.Fatalf("the roster of the conversation itself = %+v, %v", got, err)
			}
			agent, replacementWorkspace, replacementOwner := conv.agentIDForTest(t, e), ws, owner.id
			if tc.otherWorkspace {
				replacementWorkspace = e.workspace(owner, reader, other)
			}
			if tc.otherOwner {
				replacementOwner = other.id
			}
			if tc.otherAgentConversation {
				agent = 9999
			}
			if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, conv.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, ?, '')`,
				conv.ID, replacementWorkspace, replacementOwner, agent); err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.Exec(`INSERT INTO conversation_members (conversation_id, user_id, role, added_by) VALUES (?, ?, 'viewer', ?)`,
				conv.ID, reader.id, replacementOwner); err != nil {
				t.Fatal(err)
			}
			if got, err := e.svc.members(t.Context(), read); !errors.Is(err, errNotFound) || len(got.Members) != 0 {
				t.Fatalf("the roster of a conversation that took the ID = %+v, %v; want nothing", got, err)
			}
		})
	}
}
