package conversations

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/agentrt"
)

type linkEnv struct {
	*env
	ws int64
}

func (l linkEnv) withTranscript(c *client, title string, lines ...string) Conversation {
	l.t.Helper()
	conv := c.create(l.ws, map[string]any{"title": title})
	agent := conv.agentIDForTest(l.t, l.env)
	l.rt.mu.Lock()
	defer l.rt.mu.Unlock()
	if l.rt.replies == nil {
		l.rt.replies = map[int64][]agentrt.Message{}
	}
	for _, line := range lines {
		l.rt.replies[agent] = append(l.rt.replies[agent], agentrt.Message{Role: "assistant", Text: line})
	}
	return conv
}

func (l linkEnv) link(id int64) string { return fmt.Sprintf("/w/%d/conversations/%d", l.ws, id) }

func (l linkEnv) send(c *client, conv Conversation, content string) SendReply {
	l.t.Helper()
	return decode[SendReply](l.t, c.expect(http.StatusAccepted, http.MethodPost, path(l.ws, fmt.Sprintf("/%d/messages", conv.ID)), map[string]any{"content": content}))
}

func (l linkEnv) lastInput(conv Conversation) agentrt.Input {
	l.t.Helper()
	l.rt.mu.Lock()
	defer l.rt.mu.Unlock()
	inputs := l.rt.inputs[conv.agentIDForTest(l.t, l.env)]
	if len(inputs) == 0 {
		l.t.Fatal("the agent runtime got no message")
	}
	return inputs[len(inputs)-1]
}

func entryOf(t *testing.T, in agentrt.Input) inputEntryData {
	t.Helper()
	var d inputEntryData
	if err := json.Unmarshal(in.Entry.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

type inputEntryData struct {
	V        int    `json:"v"`
	Author   int64  `json:"author"`
	Text     string `json:"text"`
	Attached []struct {
		Conversation int64  `json:"conversation"`
		Title        string `json:"title"`
	} `json:"attached"`
}

func newLinkEnv(t *testing.T) (linkEnv, *client) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	return linkEnv{e, ws}, owner
}

func TestALinkedConversationGivesTheAgentItsTranscript(t *testing.T) {
	l, owner := newLinkEnv(t)
	earlier := l.withTranscript(owner, `Say "hi"`, "we agreed on the plan", "next step is the invoice")
	current := owner.create(l.ws, nil)

	typed := "continue from " + l.link(earlier.ID)
	reply := l.send(owner, current, typed)
	if len(reply.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", reply.Skipped)
	}

	in := l.lastInput(current)
	for _, want := range []string{typed, "we agreed on the plan", "next step is the invoice", fmt.Sprintf(`Context from the conversation "Say 'hi'" (%d)`, earlier.ID)} {
		if !strings.Contains(in.Content, want) {
			t.Fatalf("the agent got %q, want it to contain %q", in.Content, want)
		}
	}
	d := entryOf(t, in)
	if d.Text != typed || d.Author != owner.id || d.V != 1 {
		t.Fatalf("entry data = %+v, want the typed text and its author", d)
	}
	if len(d.Attached) != 1 || d.Attached[0].Conversation != earlier.ID || d.Attached[0].Title != `Say "hi"` {
		t.Fatalf("attached = %+v, want the earlier conversation", d.Attached)
	}
	if strings.Contains(string(in.Entry.Data), "we agreed") {
		t.Fatalf("the entry data holds the transcript: %s", in.Entry.Data)
	}
}

func TestAMessageWithNoLinkIsSentAsTyped(t *testing.T) {
	l, owner := newLinkEnv(t)
	current := owner.create(l.ws, nil)
	l.send(owner, current, "just a question")
	in := l.lastInput(current)
	d := entryOf(t, in)
	if in.Content != "just a question" || d.Text != "" || len(d.Attached) != 0 {
		t.Fatalf("content %q, entry %+v: want the text as typed and no attachments", in.Content, d)
	}
}

func TestALinkToAnotherUsersConversationIsSkippedLikeAMissingOne(t *testing.T) {
	l, owner := newLinkEnv(t)
	other := l.invite(owner, l.ws, "other@example.com")
	theirs := l.withTranscript(other, "Theirs", "the other person's secret plan")
	current := owner.create(l.ws, nil)

	reply := l.send(owner, current, fmt.Sprintf("see %s and %s", l.link(theirs.ID), l.link(999999)))

	want := []LinkSkip{{theirs.ID, "unavailable"}, {999999, "unavailable"}}
	if fmt.Sprint(reply.Skipped) != fmt.Sprint(want) {
		t.Fatalf("skipped = %+v, want %+v: someone else's conversation and a missing one look the same", reply.Skipped, want)
	}
	in := l.lastInput(current)
	if strings.Contains(in.Content, "secret plan") || strings.Contains(in.Content, "Theirs") || len(entryOf(t, in).Attached) != 0 {
		t.Fatalf("someone else's conversation reached the agent: %q", in.Content)
	}
}

func TestAConversationWithAttachedTextIsReadOnlyByItsOwner(t *testing.T) {
	l, owner := newLinkEnv(t)
	other := l.invite(owner, l.ws, "other@example.com")
	earlier := l.withTranscript(owner, "Earlier", "private detail from earlier")
	current := owner.create(l.ws, nil)
	l.send(owner, current, "continue from "+l.link(earlier.ID))

	messages := path(l.ws, fmt.Sprintf("/%d/messages", current.ID))
	owner.expect(http.StatusOK, http.MethodGet, messages, nil)
	rec := other.expect(http.StatusNotFound, http.MethodGet, messages, nil)
	if strings.Contains(rec.Body.String(), "private detail") {
		t.Fatalf("another member's read of the conversation held the attached text: %s", rec.Body.String())
	}
	other.expect(http.StatusNotFound, http.MethodGet, path(l.ws, fmt.Sprintf("/%d/entries", current.ID)), nil)
	other.expect(http.StatusNotFound, http.MethodPost, path(l.ws, fmt.Sprintf("/%d/messages", current.ID)), map[string]any{"content": "continue from " + l.link(earlier.ID)})
}

func TestALinkNamingAnotherWorkspaceAttachesNothing(t *testing.T) {
	l, owner := newLinkEnv(t)
	earlier := l.withTranscript(owner, "Earlier", "line of the earlier one")
	current := owner.create(l.ws, nil)
	elsewhere := l.workspace(owner)

	reply := l.send(owner, current, fmt.Sprintf("/w/%d/conversations/%d", elsewhere, earlier.ID))

	if in := l.lastInput(current); strings.Contains(in.Content, "line of the earlier one") || len(reply.Skipped) != 0 {
		t.Fatalf("a link to workspace %d attached a conversation of workspace %d: %q, skipped %+v", elsewhere, l.ws, in.Content, reply.Skipped)
	}
}

func TestAnAbsoluteLinkMustBeOnThePublicURL(t *testing.T) {
	l, owner := newLinkEnv(t)
	l.svc.SetPublicURL("https://taskto.example")
	earlier := l.withTranscript(owner, "Earlier", "line of the earlier one")
	current := owner.create(l.ws, nil)

	l.send(owner, current, fmt.Sprintf("https://other.example%s", l.link(earlier.ID)))
	if in := l.lastInput(current); strings.Contains(in.Content, "line of the earlier one") {
		t.Fatalf("a link on another host attached a conversation: %q", in.Content)
	}
	l.send(owner, current, fmt.Sprintf("https://taskto.example%s", l.link(earlier.ID)))
	if in := l.lastInput(current); !strings.Contains(in.Content, "line of the earlier one") {
		t.Fatalf("a link on the public URL attached nothing: %q", in.Content)
	}
}

func TestTheConversationItselfIsNeverAttached(t *testing.T) {
	l, owner := newLinkEnv(t)
	current := l.withTranscript(owner, "This one", "an earlier line here")
	reply := l.send(owner, current, "as in "+l.link(current.ID))
	if in := l.lastInput(current); in.Content != "as in "+l.link(current.ID) || len(reply.Skipped) != 0 {
		t.Fatalf("content %q, skipped %+v: a link to the conversation itself must change nothing", in.Content, reply.Skipped)
	}
}

func TestAtMostThreeLinksAreAttached(t *testing.T) {
	l, owner := newLinkEnv(t)
	var links []string
	for i := range 4 {
		c := l.withTranscript(owner, fmt.Sprintf("Old %d", i), fmt.Sprintf("marker-%d", i))
		links = append(links, l.link(c.ID))
	}
	current := owner.create(l.ws, nil)
	l.send(owner, current, strings.Join(links, " "))
	in := l.lastInput(current)
	for i := range 3 {
		if !strings.Contains(in.Content, fmt.Sprintf("marker-%d", i)) {
			t.Fatalf("link %d is missing from %q", i, in.Content)
		}
	}
	if strings.Contains(in.Content, "marker-3") {
		t.Fatalf("a fourth link was attached: %q", in.Content)
	}
}

func TestATranscriptIsCutToItsLastLines(t *testing.T) {
	l, owner := newLinkEnv(t)
	var lines []string
	for i := range 600 {
		lines = append(lines, fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 80)))
	}
	earlier := l.withTranscript(owner, "Long", lines...)
	current := owner.create(l.ws, nil)

	l.send(owner, current, "from "+l.link(earlier.ID))

	in := l.lastInput(current)
	_, kept, _ := strings.Cut(in.Content, "as the person attached it:\n")
	if len(kept) > maxLinkedBytes || len(kept) < maxLinkedBytes-200 {
		t.Fatalf("kept %d bytes, want about %d", len(kept), maxLinkedBytes)
	}
	if !strings.HasPrefix(kept, "line ") || !strings.HasSuffix(kept, "line 0599 "+strings.Repeat("x", 80)) || strings.Contains(kept, "line 0000") {
		t.Fatalf("the cut is not the last whole lines: starts %q", kept[:30])
	}
}

func TestAllLinkedTranscriptsTogetherStayUnderTheTotal(t *testing.T) {
	l, owner := newLinkEnv(t)
	big := strings.Repeat(strings.Repeat("y", 99)+"\n", 400)
	first := l.withTranscript(owner, "One", big)
	second := l.withTranscript(owner, "Two", big)
	third := l.withTranscript(owner, "Three", big)
	current := owner.create(l.ws, nil)

	reply := l.send(owner, current, strings.Join([]string{l.link(first.ID), l.link(second.ID), l.link(third.ID)}, " "))

	in := l.lastInput(current)
	if len(in.Content) > maxLinkedTotalBytes+1024 {
		t.Fatalf("the message is %d bytes, want at most about %d", len(in.Content), maxLinkedTotalBytes)
	}
	if fmt.Sprint(reply.Skipped) != fmt.Sprint([]LinkSkip{{third.ID, "too_large"}}) {
		t.Fatalf("skipped = %+v, want the third as too_large", reply.Skipped)
	}
	if got := entryOf(t, in).Attached; len(got) != 2 {
		t.Fatalf("attached = %+v, want the first two", got)
	}
}

func TestAnUnreadableTranscriptSkipsTheLinkAndTheMessageIsStillSent(t *testing.T) {
	l, owner := newLinkEnv(t)
	earlier := l.withTranscript(owner, "Earlier", "some line")
	current := owner.create(l.ws, nil)
	l.rt.mu.Lock()
	l.rt.messagesErr = errors.New("the runtime is down")
	l.rt.mu.Unlock()

	reply := l.send(owner, current, "from "+l.link(earlier.ID))

	if fmt.Sprint(reply.Skipped) != fmt.Sprint([]LinkSkip{{earlier.ID, "unavailable"}}) {
		t.Fatalf("skipped = %+v, want unavailable", reply.Skipped)
	}
	if in := l.lastInput(current); in.Content != "from "+l.link(earlier.ID) || len(entryOf(t, in).Attached) != 0 {
		t.Fatalf("content %q: want the message as typed", in.Content)
	}
}

func TestAConversationWithNoTextIsSkipped(t *testing.T) {
	l, owner := newLinkEnv(t)
	empty := owner.create(l.ws, nil)
	current := owner.create(l.ws, nil)
	reply := l.send(owner, current, "from "+l.link(empty.ID))
	if fmt.Sprint(reply.Skipped) != fmt.Sprint([]LinkSkip{{empty.ID, "unavailable"}}) {
		t.Fatalf("skipped = %+v, want unavailable", reply.Skipped)
	}
}

func TestAccessIsCheckedAgainAfterTheTranscriptWasRead(t *testing.T) {
	l, owner := newLinkEnv(t)
	earlier := l.withTranscript(owner, "Earlier", "line that must not leak")
	current := owner.create(l.ws, nil)
	agent := earlier.agentIDForTest(t, l.env)
	l.rt.mu.Lock()
	l.rt.onMessages = func(conv int64) {
		if conv != agent {
			return
		}
		if _, err := l.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, owner.id); err != nil {
			t.Error(err)
		}
	}
	l.rt.mu.Unlock()

	reply := l.send(owner, current, "from "+l.link(earlier.ID))

	if fmt.Sprint(reply.Skipped) != fmt.Sprint([]LinkSkip{{earlier.ID, "unavailable"}}) {
		t.Fatalf("skipped = %+v, want unavailable", reply.Skipped)
	}
	if in := l.lastInput(current); strings.Contains(in.Content, "must not leak") || len(entryOf(t, in).Attached) != 0 {
		t.Fatalf("a conversation whose owner lost access while it was read reached the agent: %q", in.Content)
	}
}

func TestTheTypedTextMustFitBesideTheAttachments(t *testing.T) {
	l, owner := newLinkEnv(t)
	earlier := l.withTranscript(owner, "Earlier", "some line")
	current := owner.create(l.ws, nil)
	typed := strings.Repeat("a", 4000) + " " + l.link(earlier.ID)

	reply := l.send(owner, current, typed)

	if fmt.Sprint(reply.Skipped) != fmt.Sprint([]LinkSkip{{earlier.ID, "too_large"}}) {
		t.Fatalf("skipped = %+v, want too_large", reply.Skipped)
	}
	if in := l.lastInput(current); in.Content != typed || len(in.Entry.Data) > maxEntryData {
		t.Fatalf("want the message as typed and an entry that fits, got %d bytes of entry", len(in.Entry.Data))
	}
}
