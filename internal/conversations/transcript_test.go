package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

// Transcript is what people and the agent said: their messages, oldest first, and neither the
// system's own text nor what tools returned.
func TestTranscriptIsWhatWasSaid(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "where is the quokka?"})
	e.rt.mu.Lock()
	e.rt.replies = map[int64][]agentrt.Message{agent: {
		{Role: "toolResult", Text: "a card's text the user can read, from a tool"},
		{Role: "assistant", Text: "on the island"},
		{Role: "system", Text: "the system prompt"},
		{Role: "assistant", Text: "  "},
	}}
	e.rt.mu.Unlock()

	got, err := e.svc.Transcript(context.Background(), agent)
	if err != nil || got != "where is the quokka?\non the island\n" {
		t.Fatalf("transcript = %q, %v", got, err)
	}
}

func TestTranscriptSaysWhyItIsNotThere(t *testing.T) {
	// No agent runtime at all.
	off := newEnv(t, true)
	if _, err := off.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("with the agent runtime off: %v", err)
	}

	e := newEnv(t, false)
	// The runtime fails: it can't say now.
	e.rt.err = errors.New("broken")
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) || errors.Is(err, ErrGone) {
		t.Fatalf("with a broken agent runtime: %v", err)
	}
	// The runtime doesn't have the conversation: it is gone.
	e.rt.err = &agentrt.StatusError{Code: http.StatusNotFound, Status: "404 Not Found", Message: "unknown conversation"}
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrGone) {
		t.Fatalf("with an unknown conversation: %v", err)
	}
	// Not ready: waited for, then ErrNoTranscript.
	e.svc.agent = func(context.Context) (Runtime, error) { return nil, errors.New("starting") }
	e.svc.readyWait = 10 * time.Millisecond
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("with the agent runtime starting: %v", err)
	}
}

// A message marks its conversation updated when it is sent, and again when the agent runtime has
// dealt with it: what the agent says in reply is in the transcript only then, and the marks are
// what the search index follows.
func TestSendMarksTheConversationAgainWhenTheTurnEnds(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	updated := func() string {
		t.Helper()
		var at string
		if err := e.db.QueryRow(`SELECT updated_at FROM conversations WHERE id = ?`, c.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	// An old mark, so a later one is told from it.
	if _, err := e.db.Exec(`UPDATE conversations SET updated_at = '2020-01-01T00:00:00.000Z' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	var queued int
	if _, err := e.db.Exec(`DELETE FROM search_conversation_queue`); err != nil {
		t.Fatal(err)
	}

	e.rt.settle = make(chan struct{})
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "hello"})
	waitFor(t, "the turn to be waited for", func() bool { return e.rt.waited.Load() == 1 })
	// Sent: marked, and so queued for the index, at once.
	if updated() == "2020-01-01T00:00:00.000Z" {
		t.Fatal("the send didn't mark the conversation")
	}
	if err := e.db.QueryRow(`SELECT revision FROM search_conversation_queue WHERE conversation_id = ?`, c.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued after the send: revision %d, %v", queued, err)
	}

	// The turn ends: marked again, which is a new revision of the queue's entry.
	close(e.rt.settle)
	waitFor(t, "the turn's end to mark the conversation", func() bool {
		return e.db.QueryRow(`SELECT revision FROM search_conversation_queue WHERE conversation_id = ?`, c.ID).Scan(&queued) == nil && queued == 2
	})

	// A turn that never ends marks nothing more, and the server's shutdown stops waiting for it.
	e.rt.settle = make(chan struct{})
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "again"})
	waitFor(t, "the second turn to be waited for", func() bool { return e.rt.waited.Load() == 2 })
	e.svc.EndStreams()
	time.Sleep(50 * time.Millisecond)
	if err := e.db.QueryRow(`SELECT revision FROM search_conversation_queue WHERE conversation_id = ?`, c.ID).Scan(&queued); err != nil || queued != 3 {
		t.Fatalf("after a turn cut short by shutdown: revision %d, %v (the send's mark only)", queued, err)
	}
}
