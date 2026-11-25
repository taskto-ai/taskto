package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/conversations"
)

// fakeTranscripts is the agent runtime's transcripts, by its conversations' IDs.
type fakeTranscripts struct {
	mu    sync.Mutex
	text  map[int64]string
	err   error           // what every call answers, when set
	errOf map[int64]error // what the calls for one conversation answer, when set
	gone  map[int64]bool
	calls []int64
	// during, when set, runs inside a call, after it read the text and before it answers.
	during func(agentID int64)
}

func newFakeTranscripts() *fakeTranscripts {
	return &fakeTranscripts{text: map[int64]string{}, gone: map[int64]bool{}}
}

func (f *fakeTranscripts) Transcript(_ context.Context, agentID int64) (string, error) {
	f.mu.Lock()
	text, err, gone, during := f.text[agentID], f.err, f.gone[agentID], f.during
	if err == nil {
		err = f.errOf[agentID]
	}
	f.calls = append(f.calls, agentID)
	f.mu.Unlock()
	if during != nil {
		during(agentID)
	}
	switch {
	case err != nil:
		return "", err
	case gone:
		return "", conversations.ErrGone
	}
	return text, nil
}

func (f *fakeTranscripts) set(agentID int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text[agentID] = text
}

func (f *fakeTranscripts) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type conversationsReply struct {
	Results []struct {
		ConversationID int64     `json:"conversation_id"`
		Title          string    `json:"title"`
		Archived       bool      `json:"archived"`
		UpdatedAt      string    `json:"updated_at"`
		Snippet        []Segment `json:"snippet"`
	} `json:"results"`
	NextOffset *int `json:"next_offset"`
	Incomplete bool `json:"incomplete"`
}

func (r *conversationsReply) titles() []string {
	out := []string{}
	for _, c := range r.Results {
		out = append(out, c.Title)
	}
	return out
}

var nextAgent int64 = 5000

// conversation records a conversation of u in workspace, as the conversations package does, and
// what the agent runtime has of it.
func (w *world) conversation(u *user, workspace int64, title, text string, src *fakeTranscripts) int64 {
	w.e.t.Helper()
	nextAgent++
	var id int64
	if err := w.e.db.QueryRow(`INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, ?) RETURNING id`,
		workspace, u.id, nextAgent, title).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	src.set(nextAgent, text)
	return id
}

func (w *world) agentID(conversation int64) int64 {
	w.e.t.Helper()
	var id int64
	if err := w.e.db.QueryRow(`SELECT agent_id FROM conversations WHERE id = ?`, conversation).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

// mark is what a message, its reply, a rename or an archive do to a conversation.
func (w *world) mark(conversation int64) {
	w.e.t.Helper()
	if _, err := w.e.db.Exec(`UPDATE conversations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, conversation); err != nil {
		w.e.t.Fatal(err)
	}
}

func (w *world) searchConversations(u *user, workspace int64, q string) *conversationsReply {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/conversations?q=%s", workspace, url.QueryEscape(q)), nil)
	expect(w.e.t, rec, http.StatusOK)
	r := decode[conversationsReply](w.e.t, rec)
	return &r
}

func wantConversations(t *testing.T, who string, r *conversationsReply, want ...string) {
	t.Helper()
	got := r.titles()
	slices.Sort(got)
	slices.Sort(want)
	if len(want) == 0 {
		want = []string{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s found %q, want %q", who, got, want)
	}
}

// A conversation is its owner's alone: nobody else finds it, the workspace's admins and the
// other members of a space included, and not the same person in another workspace.
func TestConversationSearchIsOnlyOfTheCallersOwn(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Alice's trip", "we should plan the zanzibar trip", src)
	w.conversation(w.bob, w.ws, "Bob's trip", "bob wants zanzibar too", src)
	w.conversation(w.owner, w.ws, "Owner's notes", "zanzibar budget", src)

	wantConversations(t, "alice", w.searchConversations(w.alice, w.ws, "zanzibar"), "Alice's trip")
	wantConversations(t, "bob", w.searchConversations(w.bob, w.ws, "zanzibar"), "Bob's trip")
	wantConversations(t, "owner, an admin", w.searchConversations(w.owner, w.ws, "zanzibar"), "Owner's notes")
	wantConversations(t, "mallory", w.searchConversations(w.mallory, w.ws, "zanzibar"))

	// The same person in another workspace: each workspace has its own conversations.
	var other int64
	if err := w.e.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Elsewhere') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member'), (?, ?, 'member')`,
		other, w.alice.id, other, w.mallory.id); err != nil {
		t.Fatal(err)
	}
	w.conversation(w.alice, other, "Alice there", "zanzibar elsewhere", src)
	wantConversations(t, "alice here", w.searchConversations(w.alice, w.ws, "zanzibar"), "Alice's trip")
	wantConversations(t, "alice there", w.searchConversations(w.alice, other, "zanzibar"), "Alice there")
	// Another member of that workspace doesn't find hers, and one who isn't a member gets 404.
	wantConversations(t, "mallory there", w.searchConversations(w.mallory, other, "zanzibar"))
	expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/conversations?q=zanzibar", other), nil), http.StatusNotFound)
	// Not logged in: no search.
	expect(t, (&user{e: w.e}).do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/conversations?q=zanzibar", w.ws), nil), http.StatusUnauthorized)

	// Someone who leaves the workspace no longer finds theirs, and finds them again if they return.
	if _, err := w.e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.bob.id); err != nil {
		t.Fatal(err)
	}
	expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/conversations?q=zanzibar", w.ws), nil), http.StatusNotFound)
	// The statement itself asks for the membership too, whatever route is in front of it.
	if r, err := w.e.idx.SearchConversations(ctx, w.bob.id, w.ws, Query{Text: "zanzibar"}); err != nil || len(r.Results) != 0 {
		t.Fatalf("a former member found %+v, %v", r, err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, w.bob.id); err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "bob again", w.searchConversations(w.bob, w.ws, "zanzibar"), "Bob's trip")
}

// The index follows the transcript: what is said later is found once the conversation is marked
// updated, which a message and the end of its reply both do; and a rename and an archive are
// seen as they are, with no waiting.
func TestConversationSearchFollowsTheTranscript(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Holiday", "where do we go?\n", src)
	wantConversations(t, "before", w.searchConversations(w.alice, w.ws, "quokka"))
	wantConversations(t, "the first words", w.searchConversations(w.alice, w.ws, "where"), "Holiday")

	src.set(w.agentID(c), "where do we go?\nthe agent says: see the quokka\n")
	w.mark(c)
	r := w.searchConversations(w.alice, w.ws, "quokka")
	wantConversations(t, "after the reply", r, "Holiday")
	if len(r.Results[0].Snippet) == 0 || r.Results[0].Archived {
		t.Fatalf("result = %+v", r.Results[0])
	}

	// A rename is found by its title, an archive shows, and neither needs a word of the transcript.
	if _, err := w.e.db.Exec(`UPDATE conversations SET title = 'Quolls', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, c); err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "renamed", w.searchConversations(w.alice, w.ws, "quolls"), "Quolls")
	wantConversations(t, "the old title", w.searchConversations(w.alice, w.ws, "holiday"))
	if _, err := w.e.db.Exec(`UPDATE conversations SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, c); err != nil {
		t.Fatal(err)
	}
	r = w.searchConversations(w.alice, w.ws, "quokka")
	if len(r.Results) != 1 || !r.Results[0].Archived {
		t.Fatalf("an archived conversation = %+v", r)
	}
	if _, err := w.e.db.Exec(`UPDATE conversations SET archived_at = NULL WHERE id = ?`, c); err != nil {
		t.Fatal(err)
	}
	if r = w.searchConversations(w.alice, w.ws, "quokka"); len(r.Results) != 1 || r.Results[0].Archived {
		t.Fatalf("an unarchived conversation = %+v", r)
	}

	// A conversation that is removed takes its index with it.
	if _, err := w.e.db.Exec(`DELETE FROM conversations WHERE id = ?`, c); err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "removed", w.searchConversations(w.alice, w.ws, "quokka"))
	var n int
	if err := w.e.db.QueryRow(`SELECT (SELECT count(*) FROM search_conversations) + (SELECT count(*) FROM search_conversation_fts)
		+ (SELECT count(*) FROM search_conversation_queue)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("index rows left = %d, %v", n, err)
	}
}

// Text that may be old is never quoted: a conversation whose transcript can't be read now is
// left out, and the search says so; it is found when the agent runtime answers again. A
// conversation marked while its transcript was being read stays queued, and is not found until
// the new text is in.
func TestConversationSearchNeverQuotesWhatMayBeOld(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Trip", "first words about quokkas", src)

	src.err = errors.New("the agent runtime is starting")
	r := w.searchConversations(w.alice, w.ws, "quokkas")
	if len(r.Results) != 0 || !r.Incomplete || r.NextOffset != nil {
		t.Fatalf("with the agent runtime away = %+v", r)
	}
	src.err = nil
	if r := w.searchConversations(w.alice, w.ws, "quokkas"); len(r.Results) != 1 || r.Incomplete {
		t.Fatalf("with it back = %+v", r)
	}

	// A message arrives while the transcript is read: what was read is written, and the
	// conversation stays queued for the message.
	agent := w.agentID(c)
	src.set(agent, "first words about quokkas\nthen words about numbats")
	src.during = func(id int64) {
		if id == agent {
			w.mark(c)
		}
	}
	w.mark(c)
	r = w.searchConversations(w.alice, w.ws, "numbats")
	if len(r.Results) != 0 || !r.Incomplete {
		t.Fatalf("while marked again = %+v", r)
	}
	src.during = nil
	if r := w.searchConversations(w.alice, w.ws, "numbats"); len(r.Results) != 1 || r.Incomplete {
		t.Fatalf("once settled = %+v", r)
	}
}

// A conversation the agent runtime no longer has is no longer found by what was said in it.
func TestConversationSearchDropsConversationsTheAgentRuntimeLost(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Trip", "words about quokkas", src)
	wantConversations(t, "before", w.searchConversations(w.alice, w.ws, "quokkas"), "Trip")
	src.gone[w.agentID(c)] = true
	w.mark(c)
	r := w.searchConversations(w.alice, w.ws, "quokkas")
	if len(r.Results) != 0 || r.Incomplete {
		t.Fatalf("after losing it = %+v", r)
	}
	wantConversations(t, "its title", w.searchConversations(w.alice, w.ws, "trip"))
}

// A title that matches every word comes before text that does, and the others by when they
// changed; the search pages.
func TestConversationSearchOrderAndPages(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	old := w.conversation(w.alice, w.ws, "Plain", "talk of a quokka long ago", src)
	titled := w.conversation(w.alice, w.ws, "Quokka facts", "nothing else", src)
	recent := w.conversation(w.alice, w.ws, "Other", "a quokka again", src)
	for i, c := range []int64{old, titled, recent} {
		if _, err := w.e.db.Exec(`UPDATE conversations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', ?) WHERE id = ?`,
			fmt.Sprintf("+%d minutes", i), c); err != nil {
			t.Fatal(err)
		}
	}
	target := fmt.Sprintf("/api/workspaces/%d/search/conversations?q=quokka&limit=2", w.ws)
	first := decode[conversationsReply](t, w.alice.do(http.MethodGet, target, nil))
	if fmt.Sprint(first.titles()) != "[Quokka facts Other]" || first.NextOffset == nil || *first.NextOffset != 2 {
		t.Fatalf("first page = %+v", first)
	}
	second := decode[conversationsReply](t, w.alice.do(http.MethodGet, fmt.Sprintf("%s&offset=%d", target, *first.NextOffset), nil))
	if fmt.Sprint(second.titles()) != "[Plain]" || second.NextOffset != nil {
		t.Fatalf("second page = %+v", second)
	}
	expect(t, w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/conversations", w.ws), nil), http.StatusBadRequest)
}

// Only the newest part of a very long conversation is searched, and a server with no agent
// runtime to read from finds nothing.
func TestConversationSearchLimitsAndAbsence(t *testing.T) {
	w := newWorld(t)
	w.conversation(w.alice, w.ws, "Trip", "quokkas", newFakeTranscripts())
	// No source: nothing is searched, and nothing is "incomplete" either.
	if r := w.searchConversations(w.alice, w.ws, "quokkas"); len(r.Results) != 0 || r.Incomplete {
		t.Fatalf("with no agent runtime = %+v", r)
	}

	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	long := "oldest-word-here\n"
	for len(long) < maxConversationText+1000 {
		long += "filler line of the conversation\n"
	}
	long += "newest-word-here\n"
	c := w.conversation(w.alice, w.ws, "Long", long, src)
	_ = c
	wantConversations(t, "the newest", w.searchConversations(w.alice, w.ws, "newest-word-here"), "Long")
	wantConversations(t, "the oldest", w.searchConversations(w.alice, w.ws, "oldest-word-here"))
}

// The background sync reads every queued conversation, so a search finds them already
// indexed, and an agent runtime that isn't there is no failure.
func TestConversationsAreIndexedInTheBackground(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Trip", "words about quokkas", src)
	w.conversation(w.bob, w.ws, "Bob", "words about quokkas", src)

	src.err = errors.New("starting")
	w.e.idx.syncConversationsInBackground(ctx)
	src.err = nil
	before := src.called()
	w.e.idx.syncConversationsInBackground(ctx)
	if got := src.called() - before; got != 2 {
		t.Fatalf("the background sync read %d transcripts, want 2", got)
	}
	// Nothing is left to read when someone searches.
	before = src.called()
	wantConversations(t, "alice", w.searchConversations(w.alice, w.ws, "quokkas"), "Trip")
	if src.called() != before {
		t.Fatalf("the search read %d transcripts again", src.called()-before)
	}
	// Restarting queues only the conversations marked in the last day, and the never indexed.
	if err := w.e.idx.queueConversations(ctx); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue`).Scan(&queued); err != nil || queued != 2 {
		t.Fatalf("queued after a restart = %d, %v", queued, err)
	}
	if _, err := w.e.db.Exec(`DELETE FROM search_conversation_queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE conversations SET updated_at = '2020-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`DELETE FROM search_conversation_queue`); err != nil {
		t.Fatal(err)
	}
	if err := w.e.idx.queueConversations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued old, indexed conversations = %d, %v", queued, err)
	}
}

// An older read never replaces a newer one's text: two syncs read the same conversation, a mark
// comes between, the newer sync writes the text after it and takes the conversation off the
// queue, and only then does the older one finish: its text is dropped, the index keeps the newer
// text, and nothing is left in the queue to say otherwise.
func TestAnOlderTranscriptReadNeverReplacesANewerOne(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Trip", "old words about quokkas", src)
	agent := w.agentID(c)

	// The first sync reads the old text and is held, while a mark comes and a second sync reads
	// the new text and finishes; then the first goes on with what it read.
	held := &holdingSource{fakeTranscripts: src, text: "old words about quokkas"}
	held.during = func() {
		src.set(agent, "new words about numbats")
		w.mark(c)
		if _, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0); err != nil {
			t.Error(err)
		}
	}
	if _, err := w.e.idx.syncConversations(ctx, held, w.alice.id, w.ws, 0); err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "the newer text", w.searchConversations(w.alice, w.ws, "numbats"), "Trip")
	wantConversations(t, "the older text", w.searchConversations(w.alice, w.ws, "quokkas"))
	var queued int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queue = %d, %v", queued, err)
	}
}

// holdingSource answers with a fixed text it read before running during, as a read that is
// slow, and finishes late.
type holdingSource struct {
	*fakeTranscripts
	text   string
	err    error // what the read answers instead of text, when set
	during func()
	once   sync.Once
}

func (h *holdingSource) Transcript(_ context.Context, _ int64) (string, error) {
	text, err := h.text, h.err
	h.once.Do(h.during)
	return text, err
}

// reuseID removes conversation c, which is being read, and gives its ID to a new conversation of
// bob's (as the table does once the highest ID is gone), whose agent runtime conversation has text.
func (w *world) reuseID(c int64, src *fakeTranscripts, text string) int64 {
	w.e.t.Helper()
	if _, err := w.e.db.Exec(`DELETE FROM conversations WHERE id = ?`, c); err != nil {
		w.e.t.Fatal(err)
	}
	nextAgent++
	if _, err := w.e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, ?, 'Bob''s plans')`,
		c, w.ws, w.bob.id, nextAgent); err != nil {
		w.e.t.Fatal(err)
	}
	src.set(nextAgent, text)
	return nextAgent
}

// A read of a conversation that is removed meanwhile is never written onto another conversation
// that has taken its ID: not its text, and not its being gone.
func TestConversationIDsReusedNeverTakeAnotherOnesTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"a transcript", nil}, {"a conversation gone", conversations.ErrGone}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			src := newFakeTranscripts()
			w.e.idx.SetConversations(src)
			c := w.conversation(w.alice, w.ws, "Alice's trip", "alice's secret about quokkas", src)

			held := &holdingSource{fakeTranscripts: src, text: "alice's secret about quokkas", err: tc.err}
			held.during = func() { w.reuseID(c, src, "bob's words about numbats") }
			if _, err := w.e.idx.syncConversations(ctx, held, w.alice.id, w.ws, 0); err != nil {
				t.Fatal(err)
			}
			// Bob's conversation is still waiting to be indexed from his own transcript.
			var queued int
			if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue WHERE conversation_id = ?`, c).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("bob's conversation queued = %d, %v", queued, err)
			}
			var indexed int
			if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_fts WHERE rowid = ?`, c).Scan(&indexed); err != nil || indexed != 0 {
				t.Fatalf("bob's conversation has index rows from alice's read: %d, %v", indexed, err)
			}
			wantConversations(t, "bob, alice's words", w.searchConversations(w.bob, w.ws, "quokkas"))
			wantConversations(t, "bob, his words", w.searchConversations(w.bob, w.ws, "numbats"), "Bob's plans")
			wantConversations(t, "alice", w.searchConversations(w.alice, w.ws, "quokkas"))
		})
	}
}

// A disabled account finds nothing of its conversations, as every route to them refuses it, and
// finds them again when it is enabled.
func TestConversationSearchRefusesADisabledAccount(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Alice's trip", "plan the zanzibar trip", src)
	wantConversations(t, "alice", w.searchConversations(w.alice, w.ws, "zanzibar"), "Alice's trip")

	if _, err := w.e.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, w.alice.id); err != nil {
		t.Fatal(err)
	}
	if r, err := w.e.idx.SearchConversations(ctx, w.alice.id, w.ws, Query{Text: "zanzibar"}); err != nil || len(r.Results) != 0 {
		t.Fatalf("a disabled account found %+v, %v", r, err)
	}
	if _, err := w.e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, w.alice.id); err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "alice, enabled again", w.searchConversations(w.alice, w.ws, "zanzibar"), "Alice's trip")
}

// A mark of a conversation always has a number nobody had before, also after the queue's entry
// was taken off: an older read can't take for its own a mark that came later.
func TestConversationMarksAreNeverReused(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Trip", "words", src)
	revision := func() int64 {
		var n int64
		if err := w.e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, c).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seen := map[int64]bool{revision(): true}
	for range 4 {
		w.mark(c)
		if _, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0); err != nil {
			t.Fatal(err)
		}
		n := revision()
		if seen[n] {
			t.Fatalf("revision %d came twice", n)
		}
		seen[n] = true
	}
}

// A conversation that only its title matches has no snippet: the start of its text would be
// quoted as if it had matched.
func TestConversationSnippetIsOnlyOfWhatMatched(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Quokka facts", "nothing about the animal here, only the weather", src)
	r := w.searchConversations(w.alice, w.ws, "quokka")
	if len(r.Results) != 1 || len(r.Results[0].Snippet) != 0 {
		t.Fatalf("a title-only match = %+v", r)
	}
	w.conversation(w.alice, w.ws, "Weather", "we talked about the quokka at length", src)
	seen := false
	for _, c := range w.searchConversations(w.alice, w.ws, "quokka").Results {
		if c.Title == "Weather" {
			seen = true
			if !anyMatch(c.Snippet) {
				t.Fatalf("a body match = %+v", c)
			}
		}
	}
	if !seen {
		t.Fatal("the conversation whose text matches was not found")
	}
}

func anyMatch(segs []Segment) bool {
	for _, s := range segs {
		if s.Match {
			return true
		}
	}
	return false
}

// A text write is for a conversation still waiting: one that another sync already dealt with, at
// the same revision, is left alone.
func TestConversationIndexSkipsAConversationNoLongerQueued(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	c := w.conversation(w.alice, w.ws, "Trip", "words about quokkas", src)
	if _, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := w.e.db.QueryRow(`SELECT revision FROM search_conversation_revisions WHERE conversation_id = ?`, c).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	// A slower sync of the same revision brings other text, and finds the queue empty.
	err := w.e.idx.write(ctx, func(tx *sql.Tx) error {
		return indexConversation(ctx, tx, queuedConversation{id: c, agentID: w.agentID(c), revision: revision}, "words about numbats", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantConversations(t, "the first text", w.searchConversations(w.alice, w.ws, "quokkas"), "Trip")
	wantConversations(t, "the slower sync's text", w.searchConversations(w.alice, w.ws, "numbats"))
}

// The skip list of a long drain holds only conversations still queued after being read: with every
// conversation taken off the queue as it is read, it stays empty however many there are.
func TestConversationDrainSkipsOnlyWhatStaysQueued(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	for i := range conversationBatch*3 + 5 {
		w.conversation(w.alice, w.ws, fmt.Sprintf("Trip %d", i), "words", src)
	}
	if n, err := w.e.idx.syncConversations(ctx, src, 0, 0, 0); err != nil || n != conversationBatch*3+5 {
		t.Fatalf("the drain read %d, %v", n, err)
	}
	var queued int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_conversation_queue`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued after the drain = %d, %v", queued, err)
	}
	// One marked while it is read stays queued, and is not read twice by the same drain.
	c := w.conversation(w.alice, w.ws, "Marked", "words", src)
	agent := w.agentID(c)
	before := src.called()
	src.during = func(id int64) {
		if id == agent {
			w.mark(c)
		}
	}
	if _, err := w.e.idx.syncConversations(ctx, src, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if reads := src.called() - before; reads != 1 {
		t.Fatalf("a conversation marked as it was read was read %d times by one drain", reads)
	}
}

// One conversation whose transcript can't be read leaves it queued and the others are indexed; it
// is the agent runtime's own failure only after several in a row, which stops the sync.
func TestConversationSyncGoesOnPastOneFailure(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	good1 := w.conversation(w.alice, w.ws, "Good one", "words about quokkas", src)
	bad := w.conversation(w.alice, w.ws, "Bad", "words about quokkas", src)
	good2 := w.conversation(w.alice, w.ws, "Good two", "words about quokkas", src)
	_ = []int64{good1, good2}
	src.errOf = map[int64]error{w.agentID(bad): errors.New("this one's history can't be read")}
	_, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0)
	if err == nil {
		t.Fatal("the failure was not reported")
	}
	r := w.searchConversations(w.alice, w.ws, "quokkas")
	wantConversations(t, "the others", r, "Good one", "Good two")
	if !r.Incomplete {
		t.Fatalf("the failed one is left out and the search says so: %+v", r)
	}

	// The agent runtime as a whole not answering stops the sync at the first: the others would not
	// be answered either.
	src.errOf = nil
	src.err = fmt.Errorf("%w: %w", conversations.ErrNoTranscript, conversations.ErrRuntimeDown)
	for i := range 5 {
		w.conversation(w.alice, w.ws, fmt.Sprintf("Later %d", i), "words", src)
	}
	before := src.called()
	if _, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0); !errors.Is(err, conversations.ErrRuntimeDown) {
		t.Fatalf("error = %v", err)
	}
	if reads := src.called() - before; reads != 1 {
		t.Fatalf("the sync read %d transcripts after the agent runtime went away, want 1", reads)
	}

	// Failures of each conversation alone don't stop it: all of them are tried, and left queued.
	src.err = nil
	src.errOf = map[int64]error{}
	var later []int64
	rows, err := w.e.db.Query(`SELECT agent_id FROM conversations WHERE title LIKE 'Later%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		later = append(later, id)
	}
	_ = rows.Close()
	for _, id := range later {
		src.errOf[id] = fmt.Errorf("%w: refused", conversations.ErrNoTranscript)
	}
	before = src.called()
	if _, err := w.e.idx.syncConversations(ctx, src, w.alice.id, w.ws, 0); err == nil {
		t.Fatal("no error")
	}
	// Each of them, and the one that failed before, once.
	if reads := src.called() - before; reads != len(later)+1 {
		t.Fatalf("the sync read %d transcripts, want each of the %d that failed alone, once", reads, len(later)+1)
	}
}

// One search has one allowance of transcripts to read, however many times it looks again.
func TestConversationSearchSharesItsAllowanceAcrossAttempts(t *testing.T) {
	w := newWorld(t)
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	for i := range 6 {
		w.conversation(w.alice, w.ws, fmt.Sprintf("Trip %d", i), "words about quokkas", src)
	}
	w.e.idx.conversationSyncLimit = 4
	r := w.searchConversations(w.alice, w.ws, "quokkas")
	if !r.Incomplete || len(r.Results) != 4 {
		t.Fatalf("reply %+v: want 4 found, incomplete (two attempts of four would have read six)", r)
	}
	if r := w.searchConversations(w.alice, w.ws, "quokkas"); r.Incomplete || len(r.Results) != 6 {
		t.Fatalf("next search = %+v", r)
	}
}
