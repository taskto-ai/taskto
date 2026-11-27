package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

func TestAgentTurnRecordsAssistantUsageFromDurableEntries(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	e.rt.rawEntries = map[int64][]json.RawMessage{agent: {
		json.RawMessage(`{"id":1,"kind":"pi.user","model":[{"role":"user","content":"hi"}]}`),
		json.RawMessage(`{"id":2,"kind":"pi.assistant","model":[{"role":"assistant","provider":"provider-a","usage":{"input":3,"output":2}}]}`),
		json.RawMessage(`{"id":3,"kind":"pi.assistant","model":[{"role":"assistant","provider":"provider-a","usage":{"input":4,"output":3}}]}`),
	}}
	if err := e.svc.recordAgentUsage(context.Background(), e.rt, Principal{Conversation: c.ID, AgentConversation: agent, Workspace: ws, User: owner.id}, "script", 1); err != nil {
		t.Fatal(err)
	}
	var input, output, turns int64
	if err := e.db.QueryRow(`SELECT input_tokens, output_tokens, turns FROM agent_usage_daily WHERE user_id = ? AND workspace_id = ? AND source = 'agent' AND engine = 'provider-a'`, owner.id, ws).Scan(&input, &output, &turns); err != nil {
		t.Fatal(err)
	}
	if input != 7 || output != 5 || turns != 1 {
		t.Fatalf("recorded usage = %d input, %d output, %d turns", input, output, turns)
	}
}

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
	off := newEnv(t, true)
	if _, err := off.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) || !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("with the agent runtime off: %v", err)
	}

	e := newEnv(t, false)
	e.rt.err = errors.New("broken")
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) || !errors.Is(err, ErrRuntimeDown) || errors.Is(err, ErrGone) {
		t.Fatalf("with an agent runtime that doesn't answer: %v", err)
	}
	e.rt.err = &agentrt.StatusError{Code: http.StatusInternalServerError, Status: "500", Message: "boom"}
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) || errors.Is(err, ErrRuntimeDown) || errors.Is(err, ErrGone) {
		t.Fatalf("with a refused conversation: %v", err)
	}
	e.rt.err = &agentrt.StatusError{Code: http.StatusNotFound, Status: "404 Not Found", Message: "unknown conversation"}
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrGone) {
		t.Fatalf("with an unknown conversation: %v", err)
	}
	e.svc.agent = func(context.Context) (Runtime, error) { return nil, errors.New("starting") }
	e.svc.readyWait = 10 * time.Millisecond
	if _, err := e.svc.Transcript(context.Background(), 1); !errors.Is(err, ErrNoTranscript) || !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("with the agent runtime starting: %v", err)
	}
}

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
	if _, err := e.db.Exec(`UPDATE conversations SET updated_at = '2020-01-01T00:00:00.000Z' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	var queued int
	if _, err := e.db.Exec(`DELETE FROM search_conversation_queue`); err != nil {
		t.Fatal(err)
	}
	inQueue := func() bool {
		var n int
		if err := e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue WHERE conversation_id = ?`, c.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	e.rt.settle = make(chan struct{})
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "hello"})
	waitFor(t, "the turn to be waited for", func() bool { return e.rt.waited.Load() == 1 })
	if updated() == "2020-01-01T00:00:00.000Z" {
		t.Fatal("the send didn't mark the conversation")
	}
	if err := e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, c.ID).Scan(&queued); err != nil || queued != 3 || !inQueue() {
		t.Fatalf("queued after the send: revision %d (queued %v), %v: want 1 for the creation, 2 for the test's reset and 3 for the send", queued, inQueue(), err)
	}

	close(e.rt.settle)
	waitFor(t, "the turn's end to mark the conversation", func() bool {
		return e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, c.ID).Scan(&queued) == nil && queued == 4
	})

	e.rt.settle = make(chan struct{})
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "again"})
	waitFor(t, "the second turn to be waited for", func() bool { return e.rt.waited.Load() == 2 })
	e.svc.EndStreams()
	time.Sleep(50 * time.Millisecond)
	if err := e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, c.ID).Scan(&queued); err != nil || queued != 5 {
		t.Fatalf("after a turn cut short by shutdown: revision %d, %v (the send's mark only)", queued, err)
	}
}

func revisionOf(t *testing.T, e *env, id int64) int {
	t.Helper()
	var rev int
	if err := e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, id).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestAMarkIsOnlyOfTheConversationItWasFor(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	var agentID int64
	if err := e.db.QueryRow(`SELECT agent_id FROM conversations WHERE id = ?`, c.ID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	before := revisionOf(t, e, c.ID)
	if err := e.svc.touch(context.Background(), c.ID, agentID+1000); err != nil {
		t.Fatal(err)
	}
	if got := revisionOf(t, e, c.ID); got != before {
		t.Fatalf("a mark for another conversation moved the revision from %d to %d", before, got)
	}
	if err := e.svc.touch(context.Background(), c.ID, agentID); err != nil {
		t.Fatal(err)
	}
	if got := revisionOf(t, e, c.ID); got != before+1 {
		t.Fatalf("the conversation's own mark moved the revision from %d to %d", before, got)
	}
}

func TestAFailedWaitMarksNothing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.waitErr = &agentrt.StatusError{Code: http.StatusNotFound, Status: "404 Not Found", Message: "unknown submission"}
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), map[string]any{"content": "hello"})
	waitFor(t, "the turn to be waited for", func() bool { return e.rt.waited.Load() == 1 })
	sent := revisionOf(t, e, c.ID)
	time.Sleep(100 * time.Millisecond)
	if got := revisionOf(t, e, c.ID); got != sent {
		t.Fatalf("a failed wait marked the conversation: revision %d, want the send's %d", got, sent)
	}
}
