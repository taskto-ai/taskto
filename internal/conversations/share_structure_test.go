package conversations

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	boundedAnswer  = "the bounded answer of a long turn"
	truncationNote = "[warn] Output truncated to its beginning: 2501 lines, 40000 bytes dropped"
)

func harnessText(notes ...string) string {
	return "<harness>\n" + strings.Join(notes, "\n") + "\n</harness>"
}

func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func diagnostic(severity, code, message string) map[string]any {
	return map[string]any{"severity": severity, "code": code, "message": message}
}

func toolResultEntry(id int, isError bool, content []any, diagnostics []any) map[string]any {
	entry := map[string]any{"id": id, "kind": "pi.tool-result", "model": []any{
		map[string]any{"role": "toolResult", "toolCallId": "c1", "toolName": "cli_turn", "isError": isError, "content": content},
	}}
	if diagnostics != nil {
		entry["data"] = map[string]any{"diagnostics": diagnostics}
	}
	return entry
}

type viewerReads struct {
	snapshot, appended, history string
}

func readAsViewer(t *testing.T, entry func(id int) map[string]any) viewerReads {
	t.Helper()
	e, srv, owner, ws, conv := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	e.shareWith(owner, ws, conv.ID, reader)
	e.svc.heartbeat = time.Hour
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	agent := conv.agentIDForTest(t, e)
	e.rt.rawEntries = map[int64][]json.RawMessage{agent: {json.RawMessage(line(entry(1)))}}

	theirs := reader.openEvents(srv, ws, conv.ID)
	watch := e.rt.watch(t, 0)
	watch.send(t, line(map[string]any{"type": "value", "value": map[string]any{"entries": []any{entry(1)}, "docs": map[string]any{}}}))
	snapshot := theirs.next().data
	watch.send(t, line(map[string]any{"type": "ops", "ops": []any{[]any{"p", []any{"entries"}, 1, 0, []any{entry(2)}}}}))
	appended := theirs.next().data
	history := reader.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/entries", conv.ID)), nil).Body.String()
	return viewerReads{snapshot, appended, history}
}

func (r viewerReads) each(f func(where, body string)) {
	f("snapshot", r.snapshot)
	f("appended entry", r.appended)
	f("history", r.history)
}

func TestAViewerKeepsOnlyTheHarnessDiagnosticOfAStoppedOrFailedCLIResult(t *testing.T) {
	aborted := diagnostic("error", "aborted", "Tool cli_turn was aborted")
	const abortedNote = "[error] Tool cli_turn was aborted"
	abortedText := harnessText(abortedNote)
	noteShaped := harnessText("[error] looks like a note") + "\n" + cliTranscript()
	cases := []struct {
		name        string
		content     []any
		diagnostics []any
		kept        string
	}{
		{"streamed text that starts like a harness note", []any{textPart("<harness>\n" + cliTranscript()), textPart(abortedText)}, []any{aborted}, abortedNote},
		{"streamed text that is a whole note-shaped block", []any{textPart(noteShaped), textPart(abortedText)}, []any{aborted}, abortedNote},
		{"a diagnostic that is not the last part", []any{textPart(abortedText), textPart(cliTranscript())}, []any{aborted}, ""},
		{"an error with no structured diagnostics", []any{textPart(abortedText), textPart(cliTranscript())}, nil, ""},
		{"the CLI's own failure text", []any{textPart("the Claude Code turn failed: " + cliTranscript()), textPart(abortedText)}, []any{aborted}, abortedNote},
		{"a note that is not what the diagnostics render", []any{textPart(cliTranscript()), textPart(harnessText("[error] something else"))}, []any{aborted}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := readAsViewer(t, func(id int) map[string]any { return toolResultEntry(id, true, tc.content, tc.diagnostics) })
			reads.each(func(where, body string) {
				for _, secret := range ownerOnlyCLI {
					if strings.Contains(body, secret) {
						t.Fatalf("the viewer's %s carries %q: %s", where, secret, body)
					}
				}
				if tc.kept != "" && !strings.Contains(body, tc.kept) {
					t.Fatalf("the viewer's %s lost the harness's own note: %s", where, body)
				}
			})
		})
	}
}

func TestAViewerKeepsTheAnswerOfASuccessfulCLIResultThatHasATruncationNote(t *testing.T) {
	note := harnessText(truncationNote)
	entry := func(id int) map[string]any {
		return toolResultEntry(id, false, []any{textPart(boundedAnswer), textPart(note)},
			[]any{diagnostic("warn", "truncated", "Output truncated to its beginning: 2501 lines, 40000 bytes dropped")})
	}
	readAsViewer(t, entry).each(func(where, body string) {
		if !strings.Contains(body, boundedAnswer) || !strings.Contains(body, truncationNote) {
			t.Fatalf("the viewer's %s lost the answer or the truncation note: %s", where, body)
		}
	})
}

func TestAViewerLosesTheOutputOfASlotWithNoNameYet(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	e.shareWith(owner, ws, conv.ID, reader)
	e.svc.heartbeat = time.Hour
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }

	nameless := func(callID string) map[string]any {
		return map[string]any{"callId": callID, "status": "running", "output": ""}
	}
	emptyName := func(callID string) map[string]any {
		return map[string]any{"callId": callID, "name": "", "status": "running", "output": ""}
	}
	tools := func(slots ...any) string {
		return line(map[string]any{"type": "value", "value": map[string]any{"entries": []any{}, "docs": map[string]any{"pi.live": map[string]any{"tools": slots}}}})
	}
	set := func(path []any, value any) string {
		return line(map[string]any{"type": "ops", "ops": []any{[]any{"s", path, value}}})
	}
	splice := func(slots ...any) string {
		return line(map[string]any{"type": "ops", "ops": []any{[]any{"p", []any{"docs", "pi.live", "tools"}, 0, 0, slots}}})
	}
	cases := []struct {
		name  string
		setup string
	}{
		{"a slot with no name in the whole list", tools(nameless("n1"))},
		{"a slot with an empty name in the whole list", tools(emptyName("n1"))},
		{"a slot with no name set on its own", set([]any{"docs", "pi.live", "tools", 0}, nameless("n1"))},
		{"a slot with no name spliced in", splice(nameless("n1"))},
	}
	theirs := reader.openEvents(srv, ws, conv.ID)
	watch := e.rt.watch(t, 0)
	for _, tc := range cases {
		watch.send(t, tools(liveSlot("pause_safe", "p1")))
		theirs.next()
		watch.send(t, tc.setup)
		theirs.next()
		watch.send(t, appendOutput(0, cliTranscript()))
		got := theirs.next().data
		if !strings.Contains(got, `"ops":[]`) || strings.Contains(got, "private-notes") {
			t.Fatalf("%s: the viewer's stream kept the output of a slot that has no name: %s", tc.name, got)
		}
	}
}

func TestAViewerStreamEndsAtItsHeartbeatWhenAnotherConversationTookItsID(t *testing.T) {
	e, srv, admin, ws, conv := newStreamEnv(t)
	other := e.invite(admin, ws, "other@example.com")
	reader := e.invite(admin, ws, "reader@example.com")
	e.shareWith(admin, ws, conv.ID, reader)
	ticks := make(chan time.Time)
	e.svc.heartbeat = time.Hour
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	s := reader.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	e.replaceConversation(conv.ID, ws, other, reader)
	ticks <- time.Now()
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)
}
