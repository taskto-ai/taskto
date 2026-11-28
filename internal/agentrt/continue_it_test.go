package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func twoScriptModels(t *testing.T) []string {
	t.Helper()
	return scriptModels(t, map[string]any{"id": "script", "kind": "script"}, map[string]any{"id": "backup", "kind": "script", "name": "Backup"})
}

func threeScriptModels(t *testing.T) []string {
	t.Helper()
	return scriptModels(t,
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "backup", "kind": "script", "name": "Backup"},
		map[string]any{"id": "mentioned", "kind": "script", "name": "Mentioned"})
}

func scriptModels(t *testing.T, list ...any) []string {
	t.Helper()
	config := map[string]any{"providers": list}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"TASKTO_AGENT_PROVIDERS=" + path}
}

func revisionOf(t *testing.T, c *Client, conv int64) int64 {
	t.Helper()
	state, err := c.Model(ctxFor(t), conv)
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

func setModelNow(ctx context.Context, c *Client, conv int64, model ModelRef) error {
	state, err := c.Model(ctx, conv)
	if err != nil {
		return err
	}
	_, err = c.SetModel(ctx, conv, state.Revision, model)
	return err
}

func conflictReason(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Code == 409 {
		return se.Reason
	}
	return ""
}

var (
	scriptModel    = ModelRef{Provider: "script", ModelID: "script-1"}
	backupModel    = ModelRef{Provider: "backup", ModelID: "script-1"}
	mentionedModel = ModelRef{Provider: "mentioned", ModelID: "script-1"}
)

func lastEntryID(t *testing.T, c *Client, conv int64) int64 {
	t.Helper()
	raw, err := c.Entries(ctxFor(t), conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Entries []struct {
			ID    int64 `json:"id"`
			Model []struct {
				Role     string `json:"role"`
				Provider string `json:"provider"`
			} `json:"model"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Entries) == 0 {
		t.Fatalf("entries %s: %v", raw, err)
	}
	return page.Entries[len(page.Entries)-1].ID
}

func lastAssistantProvider(t *testing.T, c *Client, conv int64) string {
	t.Helper()
	raw, err := c.Entries(ctxFor(t), conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Entries []struct {
			Model []struct {
				Role     string `json:"role"`
				Provider string `json:"provider"`
			} `json:"model"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	for i := len(page.Entries) - 1; i >= 0; i-- {
		for j := len(page.Entries[i].Model) - 1; j >= 0; j-- {
			if m := page.Entries[i].Model[j]; m.Role == "assistant" {
				return m.Provider
			}
		}
	}
	t.Fatalf("no assistant message in %s", raw)
	return ""
}

func failedConversation(t *testing.T, s *sidecar) int64 {
	t.Helper()
	return failedConversationOf(t, s, "fail:script")
}

func failedConversationOf(t *testing.T, s *sidecar, prompt string) int64 {
	t.Helper()
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.client.Submit(ctx, conv, Input{Content: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, failed); err != nil || settled.Status != "unanswered" || settled.Reason != "model_error" {
		t.Fatalf("the failing request settled %+v, %v; want unanswered with model_error", settled, err)
	}
	return conv
}

func TestContinueAfterAFailedReplyAnswersOnTheModelItWasGiven(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	sub, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "echo: fail:script" {
		t.Fatalf("the answer after the continue = %q, want the request answered again on the new model", got)
	}
	if got := lastAssistantProvider(t, s.client, conv); got != "backup" {
		t.Fatalf("the answer came from %q, want backup", got)
	}
}

func TestAContinueIsRefusedWhenAnotherTabChoseAModelBeforeItsCommit(t *testing.T) {
	s := startSidecar(t, append(twoScriptModels(t), "TASKTO_AGENT_TEST_CONTINUE_PAUSE_MS=1500")...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	after := lastEntryID(t, s.client, conv)
	before := entriesJSON(t, s.client, conv)
	done := make(chan error, 1)
	read := revisionOf(t, s.client, conv)
	go func() {
		_, _, err := s.client.Continue(ctx, conv, read, backupModel, after)
		done <- err
	}()
	waitForOutput(t, s, "test: the continue is waiting before its commit")
	if err := setModelNow(ctx, s.client, conv, scriptModel); err != nil {
		t.Fatal(err)
	}
	if err := <-done; conflictReason(err) != "model_changed" {
		t.Fatalf("a continue that read the model before another tab wrote it: err = %v, want a 409 with reason model_changed", err)
	}
	if got := entriesJSON(t, s.client, conv); got != before {
		t.Fatalf("a refused continue changed the conversation:\n%s\n%s", before, got)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel {
		t.Fatalf("the model is %+v, %v; want the other tab's script", got, err)
	}
}

func TestContinueOfAFailureThatMovedOnIsRefusedAndStartsNothing(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	stale := lastEntryID(t, s.client, conv)
	again, err := s.client.Submit(ctx, conv, Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, again); err != nil || settled.Status != "done" {
		t.Fatalf("the next request settled %+v, %v; want done", settled, err)
	}
	before := lastEntryID(t, s.client, conv)
	if before == stale {
		t.Fatal("the next request wrote no entry")
	}
	if _, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), scriptModel, stale); conflictReason(err) != "moved" {
		t.Fatalf("a continue of a failure the conversation moved past: err = %v, want a 409 with reason moved", err)
	}
	if after := lastEntryID(t, s.client, conv); after != before {
		t.Fatalf("a refused continue changed the conversation: last entry %d, was %d", after, before)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel {
		t.Fatalf("a refused continue changed the model to %+v, %v; want script", got, err)
	}
}

func addNote(t *testing.T, s *sidecar, conv int64) int64 {
	t.Helper()
	if _, err := s.client.Note(ctxFor(t), conv, NoteInput{Text: "a private note", Author: 7}); err != nil {
		t.Fatal(err)
	}
	return lastEntryID(t, s.client, conv)
}

func TestAPrivateNoteDoesNotMoveTheFailureAContinueAnswers(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	failure := lastEntryID(t, s.client, conv)
	note := addNote(t, s, conv)
	if note == failure {
		t.Fatal("the note wrote no entry")
	}
	if _, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, note); conflictReason(err) != "moved" {
		t.Fatalf("a continue after the note's entry: err = %v, want a 409 with reason moved: the note is not the failed reply", err)
	}
	sub, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, failure)
	if err != nil {
		t.Fatalf("a continue of the failed reply with a note after it: %v", err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
}

func TestAPrivateNoteDoesNotReopenAFailureAContinueConsumed(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := s.client.Submit(ctx, conv, Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, answered); err != nil || settled.Status != "done" {
		t.Fatalf("the request settled %+v, %v; want done", settled, err)
	}
	reply := lastEntryID(t, s.client, conv)
	first, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, reply)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, first); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the continue settled %+v, %v; want unanswered", settled, err)
	}
	note := addNote(t, s, conv)
	for _, entry := range []int64{note, reply} {
		if _, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, entry); conflictReason(err) != "moved" {
			t.Fatalf("a continue of entry %d after a continue consumed the reply %d and a note came: err = %v, want a 409 with reason moved", entry, reply, err)
		}
	}
}

func TestContinueOnTheProductionEntryEchoesAFailurePrompt(t *testing.T) {
	s := startSidecarAt(t, productionEntry)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.client.Submit(ctx, conv, Input{Content: "fail:script"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("a failure prompt on the production entry settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "echo: fail:script" {
		t.Fatalf("the production entry answered %q, want an echo", got)
	}
}

func TestContinueOfABusyConversationIsRefusedAndStartsNothing(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
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
	running, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:30000"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if _, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, lastEntryID(t, s.client, conv)); conflictReason(err) != "busy" {
		t.Fatalf("continue while a run is going: err = %v, want a 409 with reason busy", err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel {
		t.Fatalf("a refused continue changed the running conversation's model to %+v, %v; want script", got, err)
	}
	if err := s.client.Abort(ctx, conv); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if settled, err := s.client.Wait(waitCtx, running); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the running submission = %+v, %v; want unanswered", settled, err)
	}
}

func TestContinueOfARetiredOrUnknownConversationIsNotFound(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.client.RetireConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.client.Continue(ctx, conv, 0, scriptModel, 1); !notFound(err) {
		t.Fatalf("continue of a retired conversation: err = %v, want a 404", err)
	}
	if _, _, err := s.client.Continue(ctx, conv+1000, 0, scriptModel, 1); !notFound(err) {
		t.Fatalf("continue of an unknown conversation: err = %v, want a 404", err)
	}
}

func TestContinueOvertakenByARetireStartsNoRun(t *testing.T) {
	s := startSidecar(t, "TASKTO_AGENT_TEST_CONTINUE_PAUSE_MS=600")
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	go func() {
		_, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), scriptModel, 1)
		resumed <- err
	}()
	waitForOutput(t, s, "test: the continue is waiting before its commit")
	if err := s.client.RetireConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; !notFound(err) {
		t.Fatalf("a continue the retire overtook: err = %v, want a 404", err)
	}
}

func TestAContinuedRunKeepsItsModelWhileItIsRunning(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversationOf(t, s, "fail:script;tool:pause_safe:1500")
	w, err := s.client.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if err := setModelNow(ctx, s.client, conv, scriptModel); conflictReason(err) != "busy" {
		t.Fatalf("a model write while the continued run is going: err = %v, want a 409 with reason busy", err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistantProvider(t, s.client, conv); got != "backup" {
		t.Fatalf("the generation after the tool ran on %q, want the continue's backup", got)
	}
	if err := setModelNow(ctx, s.client, conv, scriptModel); err != nil {
		t.Fatalf("a model write once the continued run ended: %v", err)
	}
}

func TestAnEntryThatAContinueConsumedIsNotContinuedAgainWhenTheRunAddedNoEntry(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := s.client.Submit(ctx, conv, Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, answered); err != nil || settled.Status != "done" {
		t.Fatalf("the request settled %+v, %v; want done", settled, err)
	}
	after := lastEntryID(t, s.client, conv)
	first, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, after)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, first); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the continue of an answered conversation settled %+v, %v; want unanswered", settled, err)
	}
	if now := lastEntryID(t, s.client, conv); now != after {
		t.Fatalf("the continue added entry %d: this test needs a run that adds none", now)
	}
	if _, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), backupModel, after); conflictReason(err) != "moved" {
		t.Fatalf("a second continue of the entry the first one consumed: err = %v, want a 409 with reason moved", err)
	}
}

type turnRecorder struct {
	mu      sync.Mutex
	pinned  []bool
	started []int64
}

func (r *turnRecorder) serve(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "t.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cli/turn", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Pinned        bool  `json:"pinned"`
			ModelRevision int64 `json:"modelRevision"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.pinned = append(r.pinned, body.Pinned)
		r.started = append(r.started, body.ModelRevision)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"seq":1,"type":"done","status":"ok","text":"ok"}` + "\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return path
}

func (r *turnRecorder) revisions() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.started...)
}

func (r *turnRecorder) seen() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.pinned...)
}

func TestACLITurnOfAContinuedRunSaysItIsPinnedAndALaterOneDoesNot(t *testing.T) {
	rec := &turnRecorder{}
	socket := rec.serve(t)
	config := map[string]any{"providers": []any{
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "cli-claude", "kind": "cli", "name": "Claude Code", "cli": "claude", "models": []any{map[string]any{"id": "default", "name": "Default"}}},
	}}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startSidecar(t, "TASKTO_AGENT_PROVIDERS="+path, "TASKTO_AGENT_TOOLS_SOCKET="+socket)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.client.Submit(ctx, conv, Input{Content: "fail:script", Capability: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, failed); err != nil || settled.Reason != "model_error" {
		t.Fatalf("the failing request settled %+v, %v", settled, err)
	}
	sub, _, err := s.client.Continue(ctx, conv, revisionOf(t, s.client, conv), ModelRef{Provider: "cli-claude", ModelID: "default"}, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	again, err := s.client.Submit(ctx, conv, Input{Content: "and again", Capability: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, again); err != nil || settled.Status != "done" {
		t.Fatalf("the next message settled %+v, %v; want done", settled, err)
	}
	if got := rec.seen(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("the CLI turns said pinned = %v, want [true false]: the continued run's, then an ordinary message's", got)
	}
}

func reasonOf(err error) string {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Reason
	}
	return ""
}

func entriesJSON(t *testing.T, c *Client, conv int64) string {
	t.Helper()
	raw, err := c.Entries(ctxFor(t), conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAContinueThatCommitsLateCannotChangeTheModelAMentionSwitched(t *testing.T) {
	for name, mentioned := range map[string]ModelRef{"another model": mentionedModel, "the model already in use": scriptModel} {
		t.Run(name, func(t *testing.T) {
			s := startSidecar(t, append(threeScriptModels(t), "TASKTO_AGENT_TEST_CONTINUE_PAUSE_MS=1500")...)
			ctx := ctxFor(t)
			conv := failedConversation(t, s)
			after := lastEntryID(t, s.client, conv)
			read := revisionOf(t, s.client, conv)
			late := make(chan error, 1)
			go func() {
				_, _, err := s.client.Continue(ctx, conv, read, backupModel, after)
				late <- err
			}()
			waitForOutput(t, s, "test: the continue is waiting before its commit")

			switched, err := s.client.SetModel(ctx, conv, read, mentioned)
			if err != nil {
				t.Fatalf("the mention's switch: %v", err)
			}
			if err := <-late; conflictReason(err) != "model_changed" {
				t.Fatalf("the continue that committed after the mention's switch: err = %v, want a 409 with reason model_changed", err)
			}
			if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != mentioned || got.Revision != switched {
				t.Fatalf("the model is %+v, %v after the late continue; want the mention's %+v at revision %d", got, err, mentioned, switched)
			}
			sub, err := s.client.Submit(ctx, conv, Input{Content: "hello", ExpectRevision: &switched})
			if err != nil {
				t.Fatalf("the mention's send: %v", err)
			}
			if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
				t.Fatalf("the mention settled %+v, %v; want done", settled, err)
			}
			if got := lastAssistantProvider(t, s.client, conv); got != mentioned.Provider {
				t.Fatalf("the mention's reply came from %q, want %q", got, mentioned.Provider)
			}
		})
	}
}

func TestAMentionIsRefusedWhenAContinueLandedBetweenItsSwitchAndItsSend(t *testing.T) {
	s := startSidecar(t, threeScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	switched, err := s.client.SetModel(ctx, conv, revisionOf(t, s.client, conv), mentionedModel)
	if err != nil {
		t.Fatal(err)
	}
	before := lastEntryID(t, s.client, conv)
	sub, _, err := s.client.Continue(ctx, conv, switched, backupModel, before)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v", settled, err)
	}
	entries := lastEntryID(t, s.client, conv)
	if _, err := s.client.Submit(ctx, conv, Input{Content: "hello", ExpectRevision: &switched}); conflictReason(err) != "model_changed" {
		t.Fatalf("a send that expected the revision the continue had replaced: err = %v, want a 409 with reason model_changed", err)
	}
	if now := lastEntryID(t, s.client, conv); now != entries {
		t.Fatalf("a refused send added entry %d", now)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != backupModel || got.Revision != switched+1 {
		t.Fatalf("the model is %+v, %v after the refused send; want the continue's %+v at revision %d", got, err, backupModel, switched+1)
	}
}

func TestAModelWriteThatExpectsAnotherRevisionIsRefused(t *testing.T) {
	s := startSidecar(t, threeScriptModels(t)...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := revisionOf(t, s.client, conv)
	if _, err := s.client.SetModel(ctx, conv, read+1, mentionedModel); conflictReason(err) != "model_changed" {
		t.Fatalf("a write that expected a revision the conversation never had: err = %v, want a 409 with reason model_changed", err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel || got.Revision != read {
		t.Fatalf("a refused write left the model %+v, %v; want script at revision %d", got, err, read)
	}
	next, err := s.client.SetModel(ctx, conv, read, mentionedModel)
	if err != nil || next != read+1 {
		t.Fatalf("a write that expected the revision it read: revision %d, %v; want %d", next, err, read+1)
	}
	if _, err := s.client.SetModel(ctx, conv, read, backupModel); conflictReason(err) != "model_changed" {
		t.Fatalf("a second write on the old revision: err = %v, want a 409 with reason model_changed", err)
	}
}

func TestEveryModelWriteMustSendTheRevisionItExpects(t *testing.T) {
	s := startSidecar(t, threeScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	read := revisionOf(t, s.client, conv)
	after := lastEntryID(t, s.client, conv)
	path := "/conversations/" + itoa(conv)
	for name, call := range map[string]func() error{
		"model": func() error {
			return s.client.do(ctx, http.MethodPut, path+"/model", map[string]any{"provider": "backup", "modelId": "script-1"}, nil)
		},
		"continue": func() error {
			return s.client.do(ctx, http.MethodPost, path+"/continue", map[string]any{"model": backupModel, "afterEntry": after}, nil)
		},
		"model with a revision that is no number": func() error {
			return s.client.do(ctx, http.MethodPut, path+"/model", map[string]any{"provider": "backup", "modelId": "script-1", "expectRevision": "1"}, nil)
		},
		"continue with a negative revision": func() error {
			return s.client.do(ctx, http.MethodPost, path+"/continue", map[string]any{"model": backupModel, "afterEntry": after, "expectRevision": -1}, nil)
		},
	} {
		if err := call(); statusCode(err) != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel || got.Revision != read {
		t.Fatalf("refused writes left the model %+v, %v; want script at revision %d", got, err, read)
	}
}

func TestACLIFallbackThatReadItsRevisionBeforeAMentionIsRefused(t *testing.T) {
	s := startSidecar(t, scriptModels(t,
		map[string]any{"id": "mentioned", "kind": "script"},
		map[string]any{"id": "cli-claude", "kind": "cli", "name": "Claude", "cli": "claude", "models": []any{map[string]any{"id": "default", "name": "Default"}}},
		map[string]any{"id": "cli-codex", "kind": "cli", "name": "Codex", "cli": "codex", "models": []any{map[string]any{"id": "default", "name": "Default"}}},
	)...)
	ctx := ctxFor(t)
	from := ModelRef{Provider: "cli-claude", ModelID: "default"}
	to := ModelRef{Provider: "cli-codex", ModelID: "default"}
	conv, err := s.client.CreateConversationOn(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	turnStarted := revisionOf(t, s.client, conv)
	switched, err := s.client.SetModel(ctx, conv, turnStarted, mentionedModel)
	if err != nil {
		t.Fatalf("the mention's switch: %v", err)
	}
	if _, err := s.client.SetModel(ctx, conv, turnStarted, to); conflictReason(err) != "model_changed" {
		t.Fatalf("the fallback's write with the revision its turn started on: err = %v, want a 409 with reason model_changed", err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != mentionedModel || got.Revision != switched {
		t.Fatalf("the model is %+v, %v after the fallback; want the mention's %+v at revision %d", got, err, mentionedModel, switched)
	}
}

func TestACLITurnStartsOnTheRevisionOfTheModelItRunsOn(t *testing.T) {
	rec := &turnRecorder{}
	s := startSidecar(t, append(scriptModels(t,
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "cli-claude", "kind": "cli", "name": "Claude", "cli": "claude", "models": []any{map[string]any{"id": "default", "name": "Default"}}},
	), "TASKTO_AGENT_TOOLS_SOCKET="+rec.serve(t))...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversationOn(ctx, ModelRef{Provider: "cli-claude", ModelID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	read := revisionOf(t, s.client, conv)
	if _, err := s.client.SetModel(ctx, conv, read, ModelRef{Provider: "cli-claude", ModelID: "default"}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.client.Submit(ctx, conv, Input{Content: "hello", Capability: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the CLI turn settled %+v, %v; want done", settled, err)
	}
	if got := rec.revisions(); len(got) != 1 || got[0] != read+1 {
		t.Fatalf("the CLI turn said it started on revisions %v, want [%d]: the one its conversation had", got, read+1)
	}
}

func TestARequestPastItsAdmitByTimeAdmitsNothing(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	after := lastEntryID(t, s.client, conv)
	read := revisionOf(t, s.client, conv)
	before := entriesJSON(t, s.client, conv)
	late := time.Now().Add(-time.Second).UnixMilli()
	path := "/conversations/" + itoa(conv)
	for name, call := range map[string]func() error{
		"submit": func() error {
			return s.client.do(ctx, http.MethodPost, path+"/submit", map[string]any{"content": "hello", "admitBy": late, "expectRevision": read}, nil)
		},
		"continue": func() error {
			return s.client.do(ctx, http.MethodPost, path+"/continue", map[string]any{"model": backupModel, "afterEntry": after, "expectRevision": read, "admitBy": late}, nil)
		},
	} {
		err := call()
		if statusCode(err) != http.StatusServiceUnavailable || reasonOf(err) != "too_late" {
			t.Errorf("%s past its admit-by time: err = %v, want 503 with reason too_late", name, err)
		}
	}
	if got := entriesJSON(t, s.client, conv); got != before {
		t.Fatalf("a request past its admit-by time changed the transcript:\n%s\n%s", before, got)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel || got.Revision != read {
		t.Fatalf("the model is %+v, %v; want script at revision %d", got, err, read)
	}
	sub, _, err := s.client.Continue(ctx, conv, read, backupModel, after)
	if err != nil {
		t.Fatalf("a continue with no admit-by time: %v", err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v", settled, err)
	}
}

func TestAContinueThatCommitsAfterItsAdmitByTimeIsRefused(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	s := startSidecar(t, append(twoScriptModels(t), "TASKTO_AGENT_TEST_CONTINUE_GATE="+gate)...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	after := lastEntryID(t, s.client, conv)
	read := revisionOf(t, s.client, conv)
	before := entriesJSON(t, s.client, conv)

	deadline, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, _, err := s.client.Continue(deadline, conv, read, backupModel, after); err == nil {
		t.Fatal("a continue paused in the sidecar answered before its deadline")
	}
	waitForOutput(t, s, "test: the continue is waiting before its commit")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, s, "test: the continue ended")
	if got := entriesJSON(t, s.client, conv); got != before {
		t.Fatalf("a continue that committed after its admit-by time changed the transcript:\n%s\n%s", before, got)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got.ModelRef != scriptModel || got.Revision != read {
		t.Fatalf("the model is %+v, %v after the late continue; want script at revision %d", got, err, read)
	}
}
