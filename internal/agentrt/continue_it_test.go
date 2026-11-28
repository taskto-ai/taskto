package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func twoScriptModels(t *testing.T) []string {
	t.Helper()
	config := map[string]any{"providers": []any{
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "backup", "kind": "script", "name": "Backup"},
	}}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"TASKTO_AGENT_PROVIDERS=" + path}
}

func conflictReason(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Code == 409 {
		return se.Reason
	}
	return ""
}

var (
	scriptModel = ModelRef{Provider: "script", ModelID: "script-1"}
	backupModel = ModelRef{Provider: "backup", ModelID: "script-1"}
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
	sub, _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv))
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

func TestContinueRunsOnItsOwnModelWhateverIsWrittenBeforeItsCommit(t *testing.T) {
	s := startSidecar(t, append(twoScriptModels(t), "TASKTO_AGENT_TEST_CONTINUE_PAUSE_MS=1500")...)
	ctx := ctxFor(t)
	conv := failedConversation(t, s)
	after := lastEntryID(t, s.client, conv)
	done := make(chan error, 1)
	var sub int64
	go func() {
		var err error
		sub, _, err = s.client.Continue(ctx, conv, backupModel, after)
		done <- err
	}()
	waitForOutput(t, s, "test: the continue is waiting before its commit")
	if err := s.client.SetModel(ctx, conv, scriptModel); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistantProvider(t, s.client, conv); got != "backup" {
		t.Fatalf("the continuation ran on %q after another tab chose script, want backup", got)
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
	if _, _, err := s.client.Continue(ctx, conv, scriptModel, stale); conflictReason(err) != "moved" {
		t.Fatalf("a continue of a failure the conversation moved past: err = %v, want a 409 with reason moved", err)
	}
	if after := lastEntryID(t, s.client, conv); after != before {
		t.Fatalf("a refused continue changed the conversation: last entry %d, was %d", after, before)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got != scriptModel {
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
	if _, _, err := s.client.Continue(ctx, conv, backupModel, note); conflictReason(err) != "moved" {
		t.Fatalf("a continue after the note's entry: err = %v, want a 409 with reason moved: the note is not the failed reply", err)
	}
	sub, _, err := s.client.Continue(ctx, conv, backupModel, failure)
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
	first, _, err := s.client.Continue(ctx, conv, backupModel, reply)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, first); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the continue settled %+v, %v; want unanswered", settled, err)
	}
	note := addNote(t, s, conv)
	for _, entry := range []int64{note, reply} {
		if _, _, err := s.client.Continue(ctx, conv, backupModel, entry); conflictReason(err) != "moved" {
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
	if _, _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv)); conflictReason(err) != "busy" {
		t.Fatalf("continue while a run is going: err = %v, want a 409 with reason busy", err)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got != scriptModel {
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
	if _, _, err := s.client.Continue(ctx, conv, scriptModel, 1); !notFound(err) {
		t.Fatalf("continue of a retired conversation: err = %v, want a 404", err)
	}
	if _, _, err := s.client.Continue(ctx, conv+1000, scriptModel, 1); !notFound(err) {
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
		_, _, err := s.client.Continue(ctx, conv, scriptModel, 1)
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
	sub, _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if err := s.client.SetModel(ctx, conv, scriptModel); conflictReason(err) != "busy" {
		t.Fatalf("a model write while the continued run is going: err = %v, want a 409 with reason busy", err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistantProvider(t, s.client, conv); got != "backup" {
		t.Fatalf("the generation after the tool ran on %q, want the continue's backup", got)
	}
	if err := s.client.SetModel(ctx, conv, scriptModel); err != nil {
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
	first, _, err := s.client.Continue(ctx, conv, backupModel, after)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, first); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the continue of an answered conversation settled %+v, %v; want unanswered", settled, err)
	}
	if now := lastEntryID(t, s.client, conv); now != after {
		t.Fatalf("the continue added entry %d: this test needs a run that adds none", now)
	}
	if _, _, err := s.client.Continue(ctx, conv, backupModel, after); conflictReason(err) != "moved" {
		t.Fatalf("a second continue of the entry the first one consumed: err = %v, want a 409 with reason moved", err)
	}
}

type turnRecorder struct {
	mu      sync.Mutex
	pinned  []bool
	prompts []string
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
			Pinned bool   `json:"pinned"`
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.pinned = append(r.pinned, body.Pinned)
		r.prompts = append(r.prompts, body.Prompt)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"seq":1,"type":"done","status":"ok","text":"ok"}` + "\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return path
}

func (r *turnRecorder) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
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
	sub, _, err := s.client.Continue(ctx, conv, ModelRef{Provider: "cli-claude", ModelID: "default"}, lastEntryID(t, s.client, conv))
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

func cliSidecar(t *testing.T, rec *turnRecorder) *sidecar {
	t.Helper()
	config := map[string]any{"providers": []any{
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "cli-claude", "kind": "cli", "name": "Claude Code", "cli": "claude", "models": []any{map[string]any{"id": "default", "name": "Default"}}},
	}}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return startSidecar(t, "TASKTO_AGENT_PROVIDERS="+path, "TASKTO_AGENT_TOOLS_SOCKET="+rec.serve(t))
}

func TestAFailureAfterAFinishedToolCallIsContinuedOnACLIWithTheToolResult(t *testing.T) {
	rec := &turnRecorder{}
	s := cliSidecar(t, rec)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := "fail-after-tool:script;tool:pause_safe:200"
	failed, err := s.client.Submit(ctx, conv, Input{Content: request, Capability: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, failed); err != nil || settled.Reason != "model_error" {
		t.Fatalf("the request settled %+v, %v; want model_error after its tool call finished", settled, err)
	}
	sub, _, err := s.client.Continue(ctx, conv, ModelRef{Provider: "cli-claude", ModelID: "default"}, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	prompts := rec.asked()
	if len(prompts) != 1 {
		t.Fatalf("the CLI worker ran %d turns, want 1", len(prompts))
	}
	for _, want := range []string{request, `{"tool":"pause_safe","result":"paused 200ms"}`} {
		if !strings.Contains(prompts[0], want) {
			t.Fatalf("the CLI's prompt %q lacks %q", prompts[0], want)
		}
	}
}

func TestASteerPlacedDuringAFailedRunIsContinuedOnACLIWithEveryRequestAndResult(t *testing.T) {
	for name, languages := range map[string][2]string{
		"one language":   {"", ""},
		"language moved": {"en", "pt-BR"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &turnRecorder{}
			s := cliSidecar(t, rec)
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
			first, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:1000", Capability: "cap", Language: languages[0]})
			if err != nil {
				t.Fatal(err)
			}
			waitForOps(t, w, "waited 200ms")
			steer := "fail-after-tool:script;tool:pause_safe:200"
			second, err := s.client.Submit(ctx, conv, Input{Content: steer, Capability: "cap", Language: languages[1], WhenBusy: "steer"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []int64{first, second} {
				if settled, err := s.client.Wait(ctx, id); err != nil || settled.Reason != "model_error" {
					t.Fatalf("submission %d settled %+v, %v; want model_error in the same run", id, settled, err)
				}
			}
			sub, _, err := s.client.Continue(ctx, conv, ModelRef{Provider: "cli-claude", ModelID: "default"}, lastEntryID(t, s.client, conv))
			if err != nil {
				t.Fatal(err)
			}
			if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
				t.Fatalf("the continue settled %+v, %v; want done", settled, err)
			}
			prompts := rec.asked()
			if len(prompts) != 1 {
				t.Fatalf("the CLI worker ran %d turns, want 1", len(prompts))
			}
			for _, want := range []string{"tool:pause_safe:1000", steer, `{"tool":"pause_safe","result":"paused 1000ms"}`, `{"tool":"pause_safe","result":"paused 200ms"}`} {
				if !strings.Contains(prompts[0], want) {
					t.Fatalf("the CLI's prompt %q lacks %q", prompts[0], want)
				}
			}
		})
	}
}

func TestACLITurnStartsAtItsOwnRequestNotAtAnEarlierFailedRun(t *testing.T) {
	const earlier = "fail-after-tool:script;tool:pause_safe:200"
	cli := ModelRef{Provider: "cli-claude", ModelID: "default"}
	failEarlier := func(t *testing.T, s *sidecar, conv int64) {
		t.Helper()
		id, err := s.client.Submit(ctxFor(t), conv, Input{Content: earlier, Capability: "cap"})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctxFor(t), id); err != nil || settled.Reason != "model_error" {
			t.Fatalf("the earlier request settled %+v, %v; want model_error after its tool call finished", settled, err)
		}
	}
	check := func(t *testing.T, rec *turnRecorder) {
		t.Helper()
		prompts := rec.asked()
		if len(prompts) != 1 {
			t.Fatalf("the CLI worker ran %d turns, want 1", len(prompts))
		}
		if !strings.Contains(prompts[0], "only the new request") {
			t.Fatalf("the CLI's prompt %q lacks the new request", prompts[0])
		}
		for _, unwanted := range []string{earlier, "pause_safe", "paused 200ms"} {
			if strings.Contains(prompts[0], unwanted) {
				t.Fatalf("the CLI's prompt %q carries %q of an earlier, separate run", prompts[0], unwanted)
			}
		}
	}

	t.Run("a new message sent on the CLI after a failed run", func(t *testing.T) {
		rec := &turnRecorder{}
		s := cliSidecar(t, rec)
		ctx := ctxFor(t)
		conv, err := s.client.CreateConversation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		failEarlier(t, s, conv)
		if err := s.client.SetModel(ctx, conv, cli); err != nil {
			t.Fatal(err)
		}
		id, err := s.client.Submit(ctx, conv, Input{Content: "only the new request", Capability: "cap"})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, id); err != nil || settled.Status != "done" {
			t.Fatalf("the new request settled %+v, %v; want done", settled, err)
		}
		check(t, rec)
	})

	t.Run("a second failed run continued on the CLI", func(t *testing.T) {
		rec := &turnRecorder{}
		s := cliSidecar(t, rec)
		ctx := ctxFor(t)
		conv, err := s.client.CreateConversation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		failEarlier(t, s, conv)
		id, err := s.client.Submit(ctx, conv, Input{Content: "fail:script;only the new request", Capability: "cap"})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, id); err != nil || settled.Reason != "model_error" {
			t.Fatalf("the new request settled %+v, %v; want its own model_error", settled, err)
		}
		sub, _, err := s.client.Continue(ctx, conv, cli, lastEntryID(t, s.client, conv))
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("the continue settled %+v, %v; want done", settled, err)
		}
		check(t, rec)
	})
}

func answered(t *testing.T, s *sidecar, conv int64, text string) bool {
	t.Helper()
	msgs, err := s.client.Messages(ctxFor(t), conv)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Role == "assistant" && m.Text == text {
			return true
		}
	}
	return false
}

func TestAContinueNeverPlacesAFollowUpQueuedBehindTheFailedRequest(t *testing.T) {
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
	failed, err := s.client.Submit(ctx, conv, Input{Content: "fail-after-tool:script;tool:pause_safe:1000"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	queued, err := s.client.Submit(ctx, conv, Input{Content: "second question"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, failed); err != nil || settled.Reason != "model_error" {
		t.Fatalf("the first request settled %+v, %v; want model_error", settled, err)
	}
	after := lastEntryID(t, s.client, conv)
	if _, _, err := s.client.Continue(ctx, conv, backupModel, after); conflictReason(err) != "queued" {
		t.Fatalf("a continue with a follow-up queued behind the failure: err = %v, want a 409 with reason queued", err)
	}
	if now := lastEntryID(t, s.client, conv); now != after {
		t.Fatalf("the refused continue added entry %d after %d", now, after)
	}
	if answered(t, s, conv, "echo: second question") {
		t.Fatal("the refused continue answered the queued follow-up")
	}
	third, err := s.client.Submit(ctx, conv, Input{Content: "third question"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{queued, third} {
		if settled, err := s.client.Wait(ctx, id); err != nil || settled.Status != "done" {
			t.Fatalf("submission %d settled %+v, %v; want done", id, settled, err)
		}
	}
	if !answered(t, s, conv, "echo: second question") {
		t.Fatal("the follow-up that stayed queued was never answered")
	}
}

func TestAContinueOfARunningConversationWithAQueuedMessageIsBusyNotQueued(t *testing.T) {
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
	queued, err := s.client.Submit(ctx, conv, Input{Content: "second question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv)); conflictReason(err) != "busy" {
		t.Fatalf("a continue while a run is going and a message waits behind it: err = %v, want a 409 with reason busy", err)
	}
	if err := s.client.Abort(ctx, conv); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, id := range []int64{running, queued} {
		if _, err := s.client.Wait(waitCtx, id); err != nil {
			t.Fatalf("submission %d did not settle: %v", id, err)
		}
	}
}

func TestAContinueOfTheFailedRequestRetriesItWhenNothingIsQueued(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv := failedConversationOf(t, s, "fail-after-tool:script;tool:pause_safe:200")
	sub, _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv))
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "finished pause_safe: paused 200ms" {
		t.Fatalf("the retried request answered %q, want the tool's result used by the backup model", got)
	}
}
