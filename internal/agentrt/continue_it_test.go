package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.client.Submit(ctx, conv, Input{Content: "fail:script"})
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
	sub, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv))
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
		sub, err = s.client.Continue(ctx, conv, backupModel, after)
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
	sub, err := s.client.Continue(ctx, conv, backupModel, stale)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	before := lastEntryID(t, s.client, conv)
	if _, err := s.client.Continue(ctx, conv, scriptModel, stale); conflictReason(err) != "moved" {
		t.Fatalf("a second continue from the same failure: err = %v, want a 409 with reason moved", err)
	}
	if after := lastEntryID(t, s.client, conv); after != before {
		t.Fatalf("a refused continue changed the conversation: last entry %d, was %d", after, before)
	}
	if got, err := s.client.Model(ctx, conv); err != nil || got != backupModel {
		t.Fatalf("a refused continue changed the model to %+v, %v; want backup", got, err)
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
	if _, err := s.client.Continue(ctx, conv, backupModel, lastEntryID(t, s.client, conv)); conflictReason(err) != "busy" {
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
	if _, err := s.client.Continue(ctx, conv, scriptModel, 1); !notFound(err) {
		t.Fatalf("continue of a retired conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Continue(ctx, conv+1000, scriptModel, 1); !notFound(err) {
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
		_, err := s.client.Continue(ctx, conv, scriptModel, 1)
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
