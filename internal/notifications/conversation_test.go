package notifications_test

import (
	"context"
	"fmt"
	"testing"

	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/notifications"
)

func TestASharedConversationNotificationNamesItOnlyWhileItCanBeRead(t *testing.T) {
	w := newWorld(t, true)
	w.svc.SetConversations(conversations.New(w.db, nil))
	var conv int64
	if err := w.db.QueryRow(`INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, 41, 'Quarterly plan') RETURNING id`,
		w.ws, w.owner.id).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`INSERT INTO conversation_members (conversation_id, user_id, role, added_by) VALUES (?, ?, 'viewer', ?)`,
		conv, w.bob.id, w.owner.id); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []*user{w.bob, w.mallory} {
		if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: who.id, WorkspaceID: w.ws,
			Kind: notifications.KindConversationShared, Ref: notifications.Ref{Conversation: conv}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got := w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindConversationShared ||
		got[0].Params["title"] != "Quarterly plan" || got[0].Params["owner"] != "Owner" ||
		got[0].Target.Section != "conversations" || got[0].Target.Item != fmt.Sprint(conv) {
		t.Fatalf("inbox = %+v", got)
	}

	got = w.inbox(w.mallory, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindChanged || len(got[0].Params) != 0 || got[0].Target.Item != "" {
		t.Fatalf("a notification of a conversation that isn't shared with its recipient = %+v", got)
	}

	if _, err := w.db.Exec(`DELETE FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, conv, w.bob.id); err != nil {
		t.Fatal(err)
	}
	got = w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindChanged || len(got[0].Params) != 0 {
		t.Fatalf("inbox after the share ended = %+v", got)
	}
}

func TestAConversationNotificationShowsNothingWithoutAReader(t *testing.T) {
	w := newWorld(t, true)
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws,
		Kind: notifications.KindConversationShared, Ref: notifications.Ref{Conversation: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindChanged {
		t.Fatalf("inbox = %+v", got)
	}
}
