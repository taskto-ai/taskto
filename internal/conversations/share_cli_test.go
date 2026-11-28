package conversations

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

var ownerOnlyCLI = []string{"asks to use", "private-notes", "not allowed", "the owner said no", "work laptop", "account 2", "usage limit"}

const cliAnswer = "the cli's answer"

func cliTranscript() string {
	return strings.Join([]string{
		"? Claude Code asks to use Bash: cat /home/owner/private-notes",
		"  ✗ not allowed (the owner said no)",
		"· Claude Code (work laptop) is at its usage limit until 14:00: this turn runs on Claude Code (account 2)",
	}, "\n")
}

func cliEntry(id int) map[string]any {
	return map[string]any{"id": id, "kind": "pi.assistant", "model": []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "c1", "name": "cli_turn", "arguments": map[string]any{"cli": "claude"}}}},
		map[string]any{"role": "toolResult", "toolCallId": "c1", "toolName": "cli_turn",
			"content": []any{map[string]any{"type": "text", "text": cliAnswer}},
			"details": map[string]any{"transcript": cliTranscript(), "apps": []any{map[string]any{"space": 3, "app": 4}}}},
	}}
}

func cliSlot(status string) map[string]any {
	return map[string]any{"callId": "c1", "name": "cli_turn", "status": status, "output": cliTranscript()}
}

func line(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestAViewerNeverReceivesTheOwnersCLIPermissionsOrAccountNotices(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	reader := e.invite(owner, ws, "reader@example.com")
	e.shareWith(owner, ws, conv.ID, reader)
	e.svc.heartbeat = time.Hour
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	agent := conv.agentIDForTest(t, e)
	e.rt.rawEntries = map[int64][]json.RawMessage{agent: {json.RawMessage(line(cliEntry(1)))}}

	live := map[string]any{"pi.live": map[string]any{"tools": []any{cliSlot("running")}}}
	lines := []string{
		line(map[string]any{"type": "value", "value": map[string]any{"entries": []any{cliEntry(1)}, "docs": live}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"a", []any{"docs", "pi.live", "tools", 0, "output"}, "  ✗ not allowed (the owner said no)\n"}}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"s", []any{"docs", "pi.live", "tools", 0, "output"}, cliTranscript()}}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"s", []any{"docs", "pi.live", "tools", 0}, cliSlot("done")}}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"s", []any{"docs", "pi.live"}, map[string]any{"tools": []any{cliSlot("done")}}}}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"p", []any{"entries"}, 1, 0, []any{cliEntry(2)}}}}),
		line(map[string]any{"type": "ops", "ops": []any{[]any{"r", map[string]any{"entries": []any{cliEntry(1)}, "docs": live}}}}),
	}
	plain := line(map[string]any{"type": "ops", "ops": []any{[]any{"a", []any{"docs", "pi.live", "generation", "message", "content", 0, "text"}, "visible to both"}}})

	mine := owner.openEvents(srv, ws, conv.ID)
	theirs := reader.openEvents(srv, ws, conv.ID)
	ownerWatch, readerWatch := e.rt.watch(t, 0), e.rt.watch(t, 1)
	for _, l := range append(lines, plain) {
		ownerWatch.send(t, l)
		readerWatch.send(t, l)
		if got := mine.next(); got.data != l {
			t.Fatalf("the owner's stream changed %s into %s", l, got.data)
		}
		got := theirs.next()
		for _, secret := range ownerOnlyCLI {
			if strings.Contains(got.data, secret) {
				t.Fatalf("the viewer's stream carries %q: %s", secret, got.data)
			}
		}
		if l == plain && got.data != plain {
			t.Fatalf("the viewer's stream changed an event that holds nothing of the CLI: %s", got.data)
		}
		if strings.HasPrefix(l, `{"type":"value"`) && (!strings.Contains(got.data, cliAnswer) || !strings.Contains(got.data, `"callId":"c1"`)) {
			t.Fatalf("the viewer lost the answer or the tool call from the snapshot: %s", got.data)
		}
	}

	one := path(ws, fmt.Sprintf("/%d/entries", conv.ID))
	body := reader.expect(http.StatusOK, http.MethodGet, one, nil).Body.String()
	for _, secret := range ownerOnlyCLI {
		if strings.Contains(body, secret) {
			t.Fatalf("the viewer's history carries %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, cliAnswer) || !strings.Contains(body, `"app":4`) {
		t.Fatalf("the viewer's history lost the answer or the saved app: %s", body)
	}
	if mine := owner.expect(http.StatusOK, http.MethodGet, one, nil).Body.String(); !strings.Contains(mine, "asks to use") || !strings.Contains(mine, "work laptop") {
		t.Fatalf("the owner's history lost its transcript: %s", mine)
	}
}
