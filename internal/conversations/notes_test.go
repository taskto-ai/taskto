package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

func notesPath(ws, id int64) string { return path(ws, fmt.Sprintf("/%d/notes", id)) }

func TestOwnerAddsANote(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	if _, err := e.db.Exec(`UPDATE conversations SET updated_at = '2000-01-01T00:00:00.000Z' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}

	got := decode[NoteReply](t, owner.expect(http.StatusAccepted, http.MethodPost, notesPath(ws, c.ID),
		map[string]any{"content": "checked with finance", "request_id": "n-1"}))
	if got.SubmissionID == 0 {
		t.Fatalf("reply = %+v", got)
	}

	e.rt.mu.Lock()
	notes, inputs := e.rt.notes[agent], e.rt.inputs[agent]
	e.rt.mu.Unlock()
	want := []agentrt.NoteInput{{Text: "checked with finance", Author: owner.id, RequestID: "n-1"}}
	if fmt.Sprint(notes) != fmt.Sprint(want) {
		t.Fatalf("the agent runtime got notes %+v, want %+v", notes, want)
	}
	if len(inputs) != 0 {
		t.Fatalf("a note reached the model as input: %+v", inputs)
	}
	time.Sleep(50 * time.Millisecond)
	if n := e.rt.waited.Load(); n != 0 {
		t.Fatalf("a note queued %d settle waits, which are what queue search indexing", n)
	}

	after := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", c.ID)), nil))
	if after.UpdatedAt == "2000-01-01T00:00:00.000Z" {
		t.Fatal("a note didn't mark the conversation updated")
	}
	msgs := decode[struct{ Messages []agentrt.Message }](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/messages", c.ID)), nil))
	for _, m := range msgs.Messages {
		if strings.Contains(m.Text, "finance") {
			t.Fatalf("messages carry the note: %+v", msgs.Messages)
		}
	}
}

func TestNoteIsRefusedWhenInvalid(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	url := notesPath(ws, c.ID)

	for name, body := range map[string]map[string]any{
		"empty":      {"content": ""},
		"blank":      {"content": " \n\t"},
		"too long":   {"content": strings.Repeat("a", maxNoteChars+1)},
		"multi-byte": {"content": strings.Repeat("é", maxNoteChars+1)},
	} {
		rec := owner.expect(http.StatusBadRequest, http.MethodPost, url, body)
		if code := decode[map[string]any](t, rec)["code"]; code != string(apierr.CodeConversationNoteInvalid) {
			t.Errorf("%s: code = %v", name, code)
		}
	}
	for name, body := range map[string]map[string]any{
		"request id too long": {"content": "x", "request_id": strings.Repeat("a", 129)},
		"request id control":  {"content": "x", "request_id": "tab\there"},
		"unknown field":       {"content": "x", "author": 99},
	} {
		if rec := owner.do(http.MethodPost, url, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	owner.expect(http.StatusAccepted, http.MethodPost, url, map[string]any{"content": strings.Repeat("é", maxNoteChars)})

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if n := len(e.rt.notes[c.agentIDForTest(t, e)]); n != 1 {
		t.Fatalf("the agent runtime got %d notes, want only the valid one", n)
	}
}

func TestNoteOnAnArchivedConversationIsRefused(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true})
	rec := owner.expect(http.StatusConflict, http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "late"})
	if code := decode[map[string]any](t, rec)["code"]; code != string(apierr.CodeConversationArchived) {
		t.Fatalf("code = %v", code)
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.notes) != 0 {
		t.Fatalf("the agent runtime got %v", e.rt.notes)
	}
}

func TestNoteIsRefusedToANonOwnerBeforeItsBodyIsRead(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	reader := e.invite(owner, ws, "reader@example.com")
	stranger := e.invite(owner, ws, "stranger@example.com")
	c := owner.create(ws, nil)
	e.shareWith(owner, ws, c.ID, reader)

	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "  "})
	reader.expectCode(http.StatusForbidden, "conversation_read_only", http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": strings.Repeat("x", maxNoteChars+1)})
	stranger.expectCode(http.StatusNotFound, "conversation_not_found", http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "  "})
	owner.expectCode(http.StatusBadRequest, string(apierr.CodeConversationNoteInvalid), http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "  "})
}

func TestNoteNeedsTheRuntime(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: http.StatusNotFound, Status: "404 Not Found", Message: "unknown conversation"}
	e.rt.mu.Unlock()
	owner.expect(http.StatusGone, http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "hello"})

	off := newEnv(t, true)
	offOwner, offWS := off.setup()
	offOwner.expect(http.StatusNotFound, http.MethodPost, notesPath(offWS, 1), map[string]any{"content": "hello"})
}

func TestNoteRechecksTheArchiveAfterWaitingForTheRuntime(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.svc.agent = func(ctx context.Context) (Runtime, error) {
		if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, c.ID); err != nil {
			t.Error(err)
		}
		return e.rt, nil
	}
	rec := owner.expect(http.StatusConflict, http.MethodPost, notesPath(ws, c.ID), map[string]any{"content": "late"})
	if code := decode[map[string]any](t, rec)["code"]; code != string(apierr.CodeConversationArchived) {
		t.Fatalf("code = %v", code)
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.notes) != 0 {
		t.Fatalf("the agent runtime got %v", e.rt.notes)
	}
}

type noteEntries struct{ Runtime }

func (noteEntries) Entries(context.Context, int64, int64, int) (json.RawMessage, error) {
	return json.RawMessage(`{"entries":[` +
		`{"id":1,"kind":"pi.user","model":[{"role":"user","content":"hello"}]},` +
		`{"id":2,"kind":"taskto.note","data":{"v":1,"author":7,"text":"checked with finance"}}]}`), nil
}

func TestTheExportCarriesANoteLikeAnyOtherEntry(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	owner.create(ws, nil)
	e.svc.agent = func(context.Context) (Runtime, error) { return noteEntries{e.rt}, nil }

	list, err := e.svc.ExportList(context.Background(), ws, owner.id)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	var kinds []string
	var note string
	err = e.svc.ExportEntries(context.Background(), list[0], func(raw json.RawMessage) error {
		var entry struct {
			Kind string `json:"kind"`
			Data struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		kinds = append(kinds, entry.Kind)
		if entry.Kind == "taskto.note" {
			note = entry.Data.Text
		}
		return nil
	})
	if err != nil || fmt.Sprint(kinds) != "[pi.user taskto.note]" || note != "checked with finance" {
		t.Fatalf("exported %v with note %q, %v; want the message and the note, in order", kinds, note, err)
	}
}
