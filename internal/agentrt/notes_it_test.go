package agentrt

import (
	"encoding/json"
	"strings"
	"testing"
)

type entryPage struct {
	Entries []struct {
		ID    int64           `json:"id"`
		Kind  string          `json:"kind"`
		Data  json.RawMessage `json:"data"`
		Model json.RawMessage `json:"model"`
	} `json:"entries"`
}

func TestNoteIsKeptAndNeverReachesTheModel(t *testing.T) {
	s := startSidecar(t, spikeToolsEnv+"=1")
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := func(content string) {
		t.Helper()
		sub, err := s.client.Submit(ctx, conv, Input{Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("wait = %+v, %v", settled, err)
		}
	}
	run("hello")

	const secret = "finance says the numbers are fine"
	sub, err := s.client.Note(ctx, conv, NoteInput{Text: secret, Author: 7, RequestID: "note-1"})
	if err != nil || sub == 0 {
		t.Fatalf("note = %d, %v", sub, err)
	}
	again, err := s.client.Note(ctx, conv, NoteInput{Text: secret, Author: 7, RequestID: "note-1"})
	if err != nil || again != sub {
		t.Fatalf("the same request id gave submission %d, %v; want %d", again, err, sub)
	}

	raw, err := s.client.Entries(ctx, conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var page entryPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	var notes int
	for i, e := range page.Entries {
		if e.Kind != "taskto.note" {
			continue
		}
		notes++
		if len(e.Model) != 0 && string(e.Model) != "null" {
			t.Fatalf("the note entry has a model message: %s", e.Model)
		}
		var data struct {
			V      int    `json:"v"`
			Author int64  `json:"author"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(e.Data, &data); err != nil || data.V != 1 || data.Author != 7 || data.Text != secret {
			t.Fatalf("data = %s, %v", e.Data, err)
		}
		if i == 0 || page.Entries[i-1].ID >= e.ID {
			t.Fatalf("the note is not placed after the earlier entries: %+v", page.Entries)
		}
	}
	if notes != 1 {
		t.Fatalf("%d note entries, want 1 (a repeated request id writes once): %+v", notes, page.Entries)
	}

	msgs, err := s.client.Messages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "finance") {
			t.Fatalf("messages carry the note: %+v", msgs)
		}
	}
	run("context")
	got := lastAssistant(t, s.client, conv)
	if strings.Contains(got, "finance") || !strings.Contains(got, "user=hello") {
		t.Fatalf("the model's context = %q, want the conversation without the note", got)
	}
}

func TestNoteWhileATurnRuns(t *testing.T) {
	s := startSidecar(t, spikeToolsEnv+"=1")
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.client.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	running, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:1500"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 400ms")

	if _, err := s.client.Note(ctx, conv, NoteInput{Text: "while it runs", Author: 3}); err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "while it runs")

	if settled, err := s.client.Wait(ctx, running); err != nil || settled.Status != "done" {
		t.Fatalf("the run: wait = %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "finished pause_safe: paused 1500ms" {
		t.Fatalf("answer = %q, the note disturbed the run", got)
	}
	msgs, err := s.client.Messages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "while it runs") {
			t.Fatalf("messages carry the note: %+v", msgs)
		}
	}
}

func TestNoteIsRefusedWhenInvalid(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]NoteInput{
		"empty":       {Text: " ", Author: 1},
		"too long":    {Text: strings.Repeat("a", 8001), Author: 1},
		"no author":   {Text: "x"},
		"bad request": {Text: "x", Author: 1, RequestID: "tab\there"},
	} {
		if _, err := s.client.Note(ctx, conv, n); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: err = %v, want a 400", name, err)
		}
	}
	if _, err := s.client.Note(ctx, 999_999, NoteInput{Text: "x", Author: 1}); statusCode(err) != 404 {
		t.Errorf("an unknown conversation: %v, want a 404", err)
	}
}
