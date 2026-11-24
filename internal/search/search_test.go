package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/knowledge"
)

var by = knowledge.Author{Name: "Owner", Email: "owner@example.com"}

func must[T any](t testing.TB) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func (w *world) find(sp access.Space, q string) Results {
	w.e.t.Helper()
	return must[Results](w.e.t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: q}))
}

func paths(res Results) []string {
	out := []string{}
	for _, r := range res.Results {
		out = append(out, r.Path)
	}
	return out
}

func wantPaths(t *testing.T, res Results, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := paths(res); !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func snippetText(r Result) (text, matched string) {
	var all, hits strings.Builder
	for _, s := range r.Snippet {
		all.WriteString(s.Text)
		if s.Match {
			hits.WriteString(s.Text + " ")
		}
	}
	return all.String(), strings.TrimSpace(hits.String())
}

// indexedCommit is the commit the index has for sp.
func (e *env) indexedCommit(sp access.Space) string {
	e.t.Helper()
	var c string
	_ = e.idx.db.QueryRowContext(ctx, `SELECT commit_id FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&c)
	return c
}

func TestIndexFollowsChanges(t *testing.T) {
	w := newWorld(t)
	kb, sp := w.e.kb, w.team

	created := must[knowledge.Result](t)(kb.Put(ctx, sp, "notes/plan.md", []byte("# Quarterly plan\n\nWe grow the orchard.\n"), "", by, ""))
	res := w.find(sp, "orchard")
	wantPaths(t, res, "notes/plan.md")
	r := res.Results[0]
	if r.Title != "Quarterly plan" || r.Commit != created.Commit || r.Version != created.Doc.Version || r.SpaceID != sp.ID ||
		r.Match != "text" || r.Score <= 0 {
		t.Fatalf("result = %+v", r)
	}
	if text, hits := snippetText(r); hits != "orchard" || !strings.Contains(text, "We grow the orchard") {
		t.Fatalf("snippet = %+v", r.Snippet)
	}
	if res.Mode != ModeFullText || res.Notice != noticeOff {
		t.Fatalf("mode = %q, notice %q", res.Mode, res.Notice)
	}

	// Update.
	updated := must[knowledge.Result](t)(kb.Put(ctx, sp, "notes/plan.md", []byte("# Yearly plan\n\nWe plant vineyards.\n"), created.Doc.Version, by, ""))
	wantPaths(t, w.find(sp, "orchard"))
	res = w.find(sp, "vineyards")
	wantPaths(t, res, "notes/plan.md")
	if r := res.Results[0]; r.Commit != updated.Commit || r.Title != "Yearly plan" {
		t.Fatalf("result = %+v", r)
	}

	// Move a document, then its folder.
	must[knowledge.Result](t)(kb.Move(ctx, sp, "notes/plan.md", "notes/plan-2026.md", "", by, ""))
	wantPaths(t, w.find(sp, "vineyards"), "notes/plan-2026.md")
	must[knowledge.Result](t)(kb.Put(ctx, sp, "notes/todo.md", []byte("prune the vineyards"), "", by, ""))
	must[knowledge.Result](t)(kb.Move(ctx, sp, "notes", "archive", "", by, ""))
	if got := paths(w.find(sp, "vineyards")); !sameSet(got, []string{"archive/plan-2026.md", "archive/todo.md"}) {
		t.Fatalf("after the folder's move: %q", got)
	}
	wantPaths(t, w.find(sp, "notes"))

	// Delete.
	deleted := must[knowledge.Result](t)(kb.Delete(ctx, sp, "archive/plan-2026.md", "", by, ""))
	wantPaths(t, w.find(sp, "vineyards"), "archive/todo.md")
	if c := w.e.indexedCommit(sp); c != deleted.Commit {
		t.Fatalf("indexed at %s, want %s", c, deleted.Commit)
	}
	var docs int
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT count(*) FROM search_docs WHERE space_id = ?`, sp.ID).Scan(&docs)
	var fts int
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT count(*) FROM search_fts`).Scan(&fts)
	if docs != 1 || fts != 1 {
		t.Fatalf("index holds %d documents and %d full-text rows, want 1 and 1", docs, fts)
	}
}

func TestIndexSyncsInTheBackground(t *testing.T) {
	w := newWorld(t)
	stop := w.e.idx.Start(ctx)
	defer stop()
	for i := range 3 {
		res := must[knowledge.Result](t)(w.e.kb.Put(ctx, w.team, fmt.Sprintf("d%d.md", i), []byte("background"), "", by, ""))
		// No search: the change itself has the space synced.
		waitFor(t, "the index to reach the change", func() bool { return w.e.indexedCommit(w.team) == res.Commit })
	}
}

func TestCatchUpAfterCrash(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "a.md", []byte("alpha"), "", by, ""))
	wantPaths(t, w.find(sp, "alpha"), "a.md")

	// Changes the index doesn't hear of: another Store on the same data directory, as if
	// the server had stopped before syncing, or someone had committed with their own tools.
	other := knowledge.New(w.e.dataDir, knowledge.Options{})
	must[knowledge.Result](t)(other.Put(ctx, sp, "b.md", []byte("bravo"), "", by, ""))
	last := must[knowledge.Result](t)(other.Delete(ctx, sp, "a.md", "", by, ""))
	if c := w.e.indexedCommit(sp); c == last.Commit || c == "" {
		t.Fatalf("index at %q, want behind %s", c, last.Commit)
	}
	wantPaths(t, w.find(sp, "bravo"), "b.md")
	wantPaths(t, w.find(sp, "alpha"))
	if c := w.e.indexedCommit(sp); c != last.Commit {
		t.Fatalf("index at %s, want %s", c, last.Commit)
	}

	// A sync that stopped half-way: its row is gone, and it left a document main doesn't
	// have and another at a version main has moved past. The next sync starts over.
	db := w.e.idx.db
	for _, q := range []string{
		`DELETE FROM search_spaces WHERE space_id = ?`,
		`UPDATE search_fts SET body = 'stale' WHERE rowid = (SELECT id FROM search_docs WHERE space_id = ? AND path = 'b.md')`,
		`UPDATE search_docs SET version = '0000000000000000000000000000000000000000' WHERE space_id = ? AND path = 'b.md'`,
	} {
		if _, err := db.ExecContext(ctx, q, sp.ID); err != nil {
			t.Fatal(err)
		}
	}
	var ghost int64
	if err := db.QueryRowContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title)
		VALUES (?, 'ghost.md', '1111111111111111111111111111111111111111', 1, 'ghost') RETURNING id`, sp.ID).Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (?, 'ghost.md', 'ghost', 'ghost')`, ghost); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, w.find(sp, "ghost"))
	wantPaths(t, w.find(sp, "stale"))
	wantPaths(t, w.find(sp, "bravo"), "b.md")

	// More files than one transaction writes, made while the server was down: startup
	// catches up without a search.
	for i := range syncBatch + 20 {
		must[knowledge.Result](t)(other.Put(ctx, sp, fmt.Sprintf("bulk/%03d.md", i), []byte("bulk"), "", by, ""))
	}
	head := must[string](t)(other.Head(ctx, sp))
	stop := w.e.idx.Start(ctx)
	waitFor(t, "startup to catch up", func() bool { return w.e.indexedCommit(sp) == head })
	stop()
	res := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "bulk", Limit: MaxLimit}))
	if len(res.Results) != MaxLimit || res.NextOffset == nil {
		t.Fatalf("%d results, next %v", len(res.Results), res.NextOffset)
	}
	res = must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "bulk", Limit: MaxLimit, Offset: *res.NextOffset}))
	if len(res.Results) != 20 || res.NextOffset != nil {
		t.Fatalf("%d results on the last page, next %v", len(res.Results), res.NextOffset)
	}
}

func TestSyncBatchesByBytes(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	w.e.idx.batchBytes = 10
	w.e.idx.syncWait = 10 * time.Millisecond
	var batches []int
	resync := false
	w.e.idx.onBatch = func(files, bytes int) {
		batches = append(batches, files)
		if !resync || len(batches) < 2 {
			return
		}
		// Every batch after the first finds the space marked partial, so a sync that
		// stopped here would start over; and searches meanwhile still see what was
		// indexed before.
		var partial bool
		_ = w.e.idx.db.QueryRowContext(ctx, `SELECT partial FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&partial)
		if !partial {
			t.Errorf("batch %d: the space isn't marked partial", len(batches))
		}
		if res := w.find(sp, "atext"); len(res.Results) != 1 || !strings.HasPrefix(res.Notice, noticeBehind) {
			t.Errorf("batch %d: a search found %q, notice %q", len(batches), paths(res), res.Notice)
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, name+".md", []byte("over ten bytes of "+name+"text"), "", by, ""))
	}
	must[string](t)(w.e.idx.Sync(ctx, sp))
	for _, name := range []string{"d", "e"} {
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, name+".md", []byte("over ten bytes of "+name+"text"), "", by, ""))
	}
	batches, resync = nil, true
	head := must[string](t)(w.e.idx.Sync(ctx, sp))
	if !slices.Equal(batches, []int{1, 1}) {
		t.Fatalf("batches of %v files, want one file each", batches)
	}
	var partial bool
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT partial FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&partial)
	if w.e.indexedCommit(sp) != head || partial {
		t.Fatalf("after the last batch: commit %q, partial %v", w.e.indexedCommit(sp), partial)
	}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		wantPaths(t, w.find(sp, name+"text"), name+".md")
	}

	// A sync that stopped half-way, leaving a row main doesn't have: the next one starts
	// over from main's files, though main hasn't moved, and drops it.
	err := w.e.idx.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE search_spaces SET partial = 1 WHERE space_id = ?`, sp.ID); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title)
			VALUES (?, 'ghost.md', 'v', 1, 'ghost') RETURNING id`, sp.ID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (?, 'ghost.md', 'ghost', 'ghost')`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	resync = false
	must[string](t)(w.e.idx.Sync(ctx, sp))
	wantPaths(t, w.find(sp, "ghost"))
}

func TestHybridPagesDontRepeatOrSkip(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	sp := w.team
	// Documents whose full-text and semantic ranks disagree, more of them than one page
	// reads of either ranking at shallow offsets.
	for i := range 90 {
		body := strings.Repeat("kumquat ", i%7+1) + strings.Repeat(fmt.Sprintf("filler%d ", i%13), i%11+1)
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, fmt.Sprintf("d%02d.md", i), []byte(body), "", by, ""))
	}
	must[string](t)(w.e.idx.Sync(ctx, sp))
	w.e.embedAll()
	whole := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "kumquat", Limit: MaxLimit}))
	if whole.Mode != ModeHybrid || len(whole.Results) != 90 {
		t.Fatalf("%s, %d results", whole.Mode, len(whole.Results))
	}
	var paged []string
	for offset := 0; ; {
		res := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "kumquat", Limit: 7, Offset: offset}))
		paged = append(paged, paths(res)...)
		for _, r := range res.Results {
			if _, matched := snippetText(r); matched == "" {
				t.Fatalf("%s has no marked snippet: %+v", r.Path, r.Snippet)
			}
		}
		if res.NextOffset == nil {
			break
		}
		offset = *res.NextOffset
	}
	if want := paths(whole); !slices.Equal(paged, want) {
		t.Fatalf("pages of 7 gave\n%q\nwant, as one page,\n%q", paged, want)
	}
}

func TestSearchDoesntWaitLongForARunningSync(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "old.md", []byte("kumquat old"), "", by, ""))
	wantPaths(t, w.find(sp, "kumquat"), "old.md")
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "new.md", []byte("kumquat new"), "", by, ""))

	// A long sync of the space is running: the search answers from what is indexed, soon,
	// and says so.
	w.e.idx.syncWait = 50 * time.Millisecond
	unlock := must[func()](t)(w.e.idx.lock(ctx, sp.ID))
	start := time.Now()
	res := w.find(sp, "kumquat")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the search took %s", d)
	}
	wantPaths(t, res, "old.md")
	if !strings.HasPrefix(res.Notice, noticeBehind) || res.Notices[0] != (Notice{Code: NoticeBehind}) {
		t.Fatalf("notice %q, %v", res.Notice, res.Notices)
	}
	unlock()
	res = w.find(sp, "kumquat")
	if !sameSet(paths(res), []string{"old.md", "new.md"}) || strings.Contains(res.Notice, noticeBehind) {
		t.Fatalf("after the sync: %q, notice %q", paths(res), res.Notice)
	}
}

func TestSearchLeavesLargeCatchUpsToTheWorker(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	w.e.idx.batchFiles = 2
	for _, name := range []string{"a", "b", "c"} {
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, name+".md", []byte("kumquat "+name), "", by, ""))
	}
	// Made as if outside the server: the worker hasn't heard of it.
	w.e.idx.dirtyMu.Lock()
	clear(w.e.idx.dirty)
	w.e.idx.dirtyMu.Unlock()

	// More than one batch changed: the search doesn't sync it, says so, and hands it over.
	res := w.find(sp, "kumquat")
	if len(res.Results) != 0 || !strings.HasPrefix(res.Notice, noticeBehind) {
		t.Fatalf("%+v", res)
	}
	w.e.idx.dirtyMu.Lock()
	_, handed := w.e.idx.dirty[sp.ID]
	w.e.idx.dirtyMu.Unlock()
	if !handed {
		t.Fatal("the catch-up wasn't handed to the worker")
	}
	stop := w.e.idx.Start(ctx)
	defer stop()
	waitFor(t, "the worker to catch up", func() bool { return len(w.find(sp, "kumquat").Results) == 3 })
	if res := w.find(sp, "kumquat"); strings.Contains(res.Notice, noticeBehind) {
		t.Fatalf("after the catch-up: %q", res.Notice)
	}
}

func TestNoNextPagePastTheDeepestOffset(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "real.md", []byte("bulk"), "", by, ""))
	must[string](t)(w.e.idx.Sync(ctx, sp))
	// More matches than the deepest page reaches, straight into the index (a commit each
	// would be slow); the space's indexed commit is unchanged, so no sync undoes them.
	err := w.e.idx.write(ctx, func(tx *sql.Tx) error {
		for i := range MaxOffset + 10 {
			var id int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title)
				VALUES (?, ?, 'v', 1, 't') RETURNING id`, sp.ID, fmt.Sprintf("bulk/%04d.md", i)).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (?, '', 't', 'bulk')`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	search := func(offset, limit int) Results {
		t.Helper()
		return must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "bulk", Limit: limit, Offset: offset}))
	}
	if res := search(MaxOffset-1, 1); res.NextOffset == nil || *res.NextOffset != MaxOffset {
		t.Fatalf("next %v, want %d", res.NextOffset, MaxOffset)
	}
	// More results follow, but past the deepest offset: no next page, which would be refused.
	if res := search(MaxOffset, 5); len(res.Results) != 5 || res.NextOffset != nil {
		t.Fatalf("%d results, next %v", len(res.Results), res.NextOffset)
	}
	if res := search(MaxOffset-50, 50); len(res.Results) != 50 || res.NextOffset == nil || *res.NextOffset != MaxOffset {
		t.Fatalf("%d results, next %v", len(res.Results), res.NextOffset)
	}
	if res := search(MaxOffset-10, 20); len(res.Results) != 20 || res.NextOffset != nil {
		t.Fatalf("%d results, next %v", len(res.Results), res.NextOffset)
	}
}

func TestUploadSnippets(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "reports/kumquat-harvest.pdf", []byte("%PDF-1.7 binary"), "", by, ""))
	check := func(mode string) {
		t.Helper()
		res := w.find(sp, "kumquat")
		if res.Mode != mode || len(res.Results) != 1 {
			t.Fatalf("%+v", res)
		}
		if text, matched := snippetText(res.Results[0]); !strings.Contains(text, "harvest") || matched != "kumquat" {
			t.Fatalf("%s search: snippet %q, matched %q", mode, text, matched)
		}
	}
	check(ModeFullText)
	w.setModel()
	check(ModeHybrid)
}

// Unversioned files (audio and video, large files) are found by their names like any
// upload, and follow their moves.
func TestUnversionedFilesByName(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	r := must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "talks/kumquat-keynote.mp4", []byte("not really a video"), "", by, ""))
	if r.Doc.Versioned {
		t.Fatal("the video is versioned")
	}
	wantPaths(t, w.find(sp, "kumquat"), "talks/kumquat-keynote.mp4")
	must[knowledge.Result](t)(w.e.kb.Move(ctx, sp, "talks/kumquat-keynote.mp4", "talks/zebra-keynote.mp4", "", by, ""))
	wantPaths(t, w.find(sp, "kumquat"))
	wantPaths(t, w.find(sp, "zebra"), "talks/zebra-keynote.mp4")
}

// A file that leaves git history gives every commit from its first version a new ID, and
// the commit the index reflects is gone: the index catches up from main's files, and its
// results name commits that exist.
func TestIndexCatchesUpAfterHistoryRewrite(t *testing.T) {
	w := newWorld(t)
	kb, sp := w.e.kb, w.team
	must[knowledge.Result](t)(kb.Put(ctx, sp, "art/cover.dat", []byte("layers"), "", by, ""))
	must[knowledge.Result](t)(kb.Put(ctx, sp, "notes/lemur.md", []byte("# Lemur\n\nAbout lemurs.\n"), "", by, ""))
	wantPaths(t, w.find(sp, "lemurs"), "notes/lemur.md")
	old := w.e.indexedCommit(sp)

	// Renamed to a kind that is always unversioned: its old versions leave history.
	moved := must[knowledge.Result](t)(kb.Move(ctx, sp, "art/cover.dat", "art/cover.psd", "", by, ""))
	if _, err := kb.Read(ctx, sp, "notes/lemur.md", old); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("the indexed commit is still there: %v", err)
	}
	if d := must[knowledge.Diff](t)(kb.Changes(ctx, sp, old)); !d.Full {
		t.Fatal("the changes since a commit that is gone aren't a full listing")
	}

	res := w.find(sp, "lemurs")
	wantPaths(t, res, "notes/lemur.md")
	if r := res.Results[0]; r.Commit != moved.Commit {
		t.Fatalf("result's commit = %s, want %s", r.Commit, moved.Commit)
	}
	if got := readAt(t, kb, sp, "notes/lemur.md", res.Results[0].Commit); got != "# Lemur\n\nAbout lemurs.\n" {
		t.Fatalf("at the result's commit: %q", got)
	}
	wantPaths(t, w.find(sp, "cover"), "art/cover.psd")
	if c := w.e.indexedCommit(sp); c != moved.Commit {
		t.Fatalf("indexed at %s, want %s", c, moved.Commit)
	}
}

func readAt(t *testing.T, kb *knowledge.Store, sp access.Space, name, at string) string {
	t.Helper()
	return string(must[knowledge.Doc](t)(kb.Read(ctx, sp, name, at)).Content)
}

func TestRanking(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	put := func(name, content string) {
		t.Helper()
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, name, []byte(content), "", by, ""))
	}
	put("guides/kumquat.md", "# Kumquat\n\nAll about the small citrus fruit.\n")
	put("notes/many.md", "# Notes\n\nkumquat jam, kumquat tea, candied kumquat.\n")
	put("notes/once.md", "# Misc\n\n"+strings.Repeat("Many other words about the garden. ", 30)+"And one kumquat.\n")
	put("notes/other.md", "---\ntitle: \"Front matter title\"\n---\n# Heading\n\nNothing here.\n")
	put("files/kumquat-photo.png", "\x89PNG kumquat zebra")
	put("files/report.pdf", "%PDF zebra")

	res := w.find(sp, "kumquat")
	got := paths(res)
	if len(got) != 4 || got[0] != "guides/kumquat.md" ||
		slices.Index(got, "notes/many.md") > slices.Index(got, "notes/once.md") || !slices.Contains(got, "files/kumquat-photo.png") {
		t.Fatalf("ranked %q", got)
	}
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i].Score > res.Results[i-1].Score {
			t.Fatalf("scores not descending: %+v", res.Results)
		}
	}
	for _, r := range res.Results {
		if r.Path == "notes/many.md" {
			if _, hits := snippetText(r); hits != "kumquat kumquat kumquat" {
				t.Fatalf("snippet = %+v", r.Snippet)
			}
		}
	}

	// Uploads by path only.
	wantPaths(t, w.find(sp, "zebra"))
	wantPaths(t, w.find(sp, "report"), "files/report.pdf")
	// Titles: front matter first.
	if res := w.find(sp, "front matter"); len(res.Results) != 1 || res.Results[0].Title != "Front matter title" {
		t.Fatalf("front matter: %+v", res.Results)
	}
	// Prefixes, every word, phrases.
	wantPaths(t, w.find(sp, "kumq"), paths(res)...)
	wantPaths(t, w.find(sp, "kumquat citrus"), "guides/kumquat.md")
	wantPaths(t, w.find(sp, `"small citrus fruit"`), "guides/kumquat.md")
	wantPaths(t, w.find(sp, `"fruit citrus"`))
	// Accents don't matter.
	wantPaths(t, w.find(sp, "kùmquat jäm"), "notes/many.md")

	// FTS5's syntax is never interpreted, so nothing is a syntax error.
	for _, q := range []string{`kumquat AND`, `NEAR(kumquat jam)`, `path:kumquat`, `kumquat*`, `"kumquat`, `-kumquat`, `(kumquat`, `^kumquat`} {
		if _, err := w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: q}); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	for _, q := range []Query{{Text: ""}, {Text: "  "}, {Text: "*** ---"}, {Text: strings.Repeat("a", MaxQueryLen+1)},
		{Text: "x", Limit: MaxLimit + 1}, {Text: "x", Offset: MaxOffset + 1}, {Text: "x", Limit: -1}} {
		if _, err := w.e.idx.Search(ctx, w.ws, []access.Space{sp}, q); err == nil {
			t.Fatalf("%+v: no error", q)
		}
	}

	// Pages.
	var all []string
	for offset := 0; ; {
		res := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{sp}, Query{Text: "kumquat", Limit: 1, Offset: offset}))
		all = append(all, paths(res)...)
		if res.NextOffset == nil {
			break
		}
		offset = *res.NextOffset
	}
	if !slices.Equal(all, got) {
		t.Fatalf("paged %q, want %q", all, got)
	}
}

// TestCrossUser checks that a search only ever finds what the caller may read: the
// workspace's search over their readable spaces, and a space's only if they can read it,
// in full-text and hybrid search alike.
func TestCrossUser(t *testing.T) {
	w := newWorld(t)
	for u, sp := range w.private {
		w.put(u, sp, "doc.md", "kumquat automobile in "+u.name+"'s private space")
	}
	w.put(w.owner, w.team, "doc.md", "kumquat automobile in team")
	w.put(w.owner, w.secret, "doc.md", "kumquat automobile in secret")
	w.put(w.owner, w.old, "doc.md", "kumquat automobile in old")
	w.finish()

	all := []access.Space{w.team, w.secret, w.old}
	for _, sp := range w.private {
		all = append(all, sp)
	}
	readable := map[*user][]int64{
		w.owner:   {w.private[w.owner].ID, w.team.ID, w.secret.ID, w.old.ID},
		w.alice:   {w.private[w.alice].ID, w.team.ID, w.old.ID},
		w.bob:     {w.private[w.bob].ID, w.team.ID},
		w.mallory: {w.private[w.mallory].ID},
	}
	// check searches for q as everyone. q matches byMeaning alone, or by full text; model
	// says whether the workspace has an embeddings model, optedIn whether every private
	// space's owner opted in. A private space is found by meaning, and searched
	// semantically, only once its owner opted in.
	check := func(q string, byMeaning, model, optedIn bool) {
		t.Helper()
		for u, canRead := range readable {
			shared := slices.DeleteFunc(slices.Clone(canRead), func(id int64) bool { return id == w.private[u].ID })
			want := canRead
			if byMeaning && !optedIn {
				want = shared
			}
			mode := ModeFullText
			if model && (optedIn || len(shared) > 0) {
				mode = ModeHybrid
			}
			rec := w.search(u, nil, q)
			expect(t, rec, http.StatusOK)
			res := decode[Results](t, rec)
			if res.Mode != mode {
				t.Fatalf("%s: mode %q, want %q (%s)", u.name, res.Mode, mode, res.Notice)
			}
			var got []int64
			for _, r := range res.Results {
				got = append(got, r.SpaceID)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("%s searching %q found spaces %v, want %v", u.name, q, got, want)
			}
			for _, sp := range all {
				rec := w.search(u, &sp, q)
				if !slices.Contains(canRead, sp.ID) {
					expect(t, rec, http.StatusNotFound)
					continue
				}
				expect(t, rec, http.StatusOK)
				res := decode[Results](t, rec)
				if !slices.Contains(want, sp.ID) {
					if len(res.Results) != 0 {
						t.Fatalf("%s found %q in space %d: %s", u.name, q, sp.ID, rec.Body)
					}
					continue
				}
				if len(res.Results) != 1 || res.Results[0].SpaceID != sp.ID {
					t.Fatalf("%s in space %d: %s", u.name, sp.ID, rec.Body)
				}
			}
		}
		// Someone who left the workspace has no search in it; nor does anyone outside it.
		expect(t, w.search(w.stranger, nil, q), http.StatusNotFound)
		expect(t, w.search(w.stranger, ptr(w.private[w.stranger]), q), http.StatusNotFound)
		expect(t, w.search(&user{e: w.e}, nil, q), http.StatusUnauthorized)
	}
	check("kumquat", false, false, false)

	// With semantic search, by meaning alone ("car" is in no document): private spaces
	// only once their owner opted in.
	w.setModel()
	// The shared spaces' editor consents for them, the archived one too.
	w.consent(w.owner, w.secret)
	w.consent(w.owner, w.old)
	w.e.embedAll()
	check("car", true, true, false)
	check("kumquat", false, true, false)
	for u := range readable {
		expect(t, u.do(http.MethodPut, embeddingURL(w.private[u]), map[string]bool{"enabled": true}), http.StatusOK)
	}
	w.e.embedAll()
	check("car", true, true, true)
	check("kumquat", false, true, true)

	// Search's own guard: spaces of another workspace, or without read access, are never
	// looked in, whatever a caller passes.
	foreign := w.team
	foreign.WorkspaceID++
	closed := w.secret
	closed.Access = access.None
	res := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{foreign, closed}, Query{Text: "kumquat"}))
	wantPaths(t, res)
	// Nothing searched: nothing to say about semantic search either.
	if res.Mode != ModeFullText || res.Notice != "" {
		t.Fatalf("no space searched: %+v", res)
	}
}

func ptr[T any](v T) *T { return &v }

func embeddingURL(sp access.Space) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/search/embedding", sp.WorkspaceID, sp.ID)
}

// spaceChunks counts sp's vectors.
func (e *env) spaceChunks(sp access.Space) int {
	e.t.Helper()
	var n int
	err := e.idx.db.QueryRowContext(ctx, `SELECT count(*) FROM search_chunks c JOIN search_docs d ON d.id = c.doc_id
		WHERE d.space_id = ?`, sp.ID).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSpacesEmbeddedOnlyWithConsent(t *testing.T) {
	w := newWorld(t)
	w.setModel() // and consents to it for the team space
	mine := w.private[w.alice]
	w.put(w.alice, mine, "fleet.md", "# Fleet\n\nMy zucchini automobile notes.\n")
	w.put(w.owner, w.team, "fleet.md", "# Fleet\n\nThe team automobile.\n")
	w.put(w.owner, w.secret, "fleet.md", "# Fleet\n\nThe secret mangosteen automobile.\n")
	for _, sp := range []access.Space{mine, w.team, w.secret} {
		must[string](t)(w.e.idx.Sync(ctx, sp))
	}
	w.e.embedAll()

	// Off by default, private and shared alike: only the team space, consented to, is
	// embedded; the others aren't waiting, and a search of only them doesn't send the
	// query out.
	if w.e.embed.sent("zucchini") || w.e.embed.sent("mangosteen") || w.e.spaceChunks(mine) != 0 ||
		w.e.spaceChunks(w.secret) != 0 || w.e.spaceChunks(w.team) == 0 {
		t.Fatalf("chunks: private %d, secret %d, team %d", w.e.spaceChunks(mine), w.e.spaceChunks(w.secret), w.e.spaceChunks(w.team))
	}
	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)
	if s := decode[settingsReply](t, w.owner.do(http.MethodGet, settings, nil)); s.Documents != 3 || s.Pending != 0 {
		t.Fatalf("settings = %+v", s)
	}
	calls := w.e.embed.calls.Load()
	for _, sp := range []access.Space{mine, w.secret} {
		if res := w.find(sp, "car"); res.Mode != ModeFullText || res.Notice != noticeNoConsent || len(res.Results) != 0 {
			t.Fatalf("space %d, no consent: %+v", sp.ID, res)
		}
	}
	if w.e.embed.calls.Load() != calls {
		t.Fatal("a search of only spaces without consent sent the query")
	}

	// A private space: only its owner sees or changes it; for anyone else it isn't there.
	for _, u := range []*user{w.owner, w.bob, w.mallory} {
		expect(t, u.do(http.MethodGet, embeddingURL(mine), nil), http.StatusNotFound)
		expect(t, u.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": true}), http.StatusNotFound)
	}
	expect(t, (&user{e: w.e}).do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": true}), http.StatusUnauthorized)
	expect(t, w.alice.do(http.MethodPut, embeddingURL(mine), map[string]any{}), http.StatusBadRequest)
	if c := decode[Consent](t, w.alice.do(http.MethodGet, embeddingURL(mine), nil)); c.Enabled || c.GivenBy != nil ||
		c.Endpoint == nil || c.Endpoint.BaseURL != w.e.embed.srv.URL+"/v1" || c.Endpoint.Model != fakeModel {
		t.Fatalf("before consent: %+v", c)
	}
	// A shared space: its editors change it, its viewers see it, others don't.
	expect(t, w.bob.do(http.MethodGet, embeddingURL(w.team), nil), http.StatusOK)
	expectCode(t, w.bob.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": false}), http.StatusForbidden, "consent_forbidden", nil)
	expect(t, w.mallory.do(http.MethodGet, embeddingURL(w.team), nil), http.StatusNotFound)
	expect(t, w.mallory.do(http.MethodPut, embeddingURL(w.secret), map[string]bool{"enabled": true}), http.StatusNotFound)
	// Being a workspace admin changes nothing: as a viewer of a space, the owner can't
	// consent for it, and once out of it, can't see it.
	viewing := w.createShared("Viewing")
	member := fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, viewing.ID, w.owner.id)
	expect(t, w.owner.do(http.MethodPatch, member, map[string]string{"role": "viewer"}), http.StatusNoContent)
	expect(t, w.owner.do(http.MethodPut, embeddingURL(viewing), map[string]bool{"enabled": true}), http.StatusForbidden)
	expect(t, w.owner.do(http.MethodDelete, member, nil), http.StatusNoContent)
	expect(t, w.owner.do(http.MethodGet, embeddingURL(viewing), nil), http.StatusNotFound)
	expect(t, w.owner.do(http.MethodPut, embeddingURL(viewing), map[string]bool{"enabled": true}), http.StatusNotFound)

	// Opted in: embedded, and found by meaning.
	rec := w.alice.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": true})
	expect(t, rec, http.StatusOK)
	if c := decode[Consent](t, rec); !c.Enabled || c.GivenBy == nil || *c.GivenBy != w.alice.id {
		t.Fatalf("reply %+v", c)
	}
	w.e.embedAll()
	if !w.e.embed.sent("zucchini") || w.e.spaceChunks(mine) == 0 {
		t.Fatal("not embedded after opting in")
	}
	if res := w.find(mine, "car"); res.Mode != ModeHybrid || len(res.Results) != 1 || res.Results[0].Match != "semantic" {
		t.Fatalf("private, on: %+v", res)
	}

	// Opted out: its vectors are gone, and it is full text again.
	expect(t, w.alice.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": false}), http.StatusOK)
	if n := w.e.spaceChunks(mine); n != 0 {
		t.Fatalf("%d chunks left after opting out", n)
	}
	if res := w.find(mine, "car"); res.Mode != ModeFullText || res.Notice != noticeNoConsent {
		t.Fatalf("private, off again: %+v", res)
	}
	if n := must[int](t)(w.e.idx.embedPending(ctx)); n != 0 {
		t.Fatalf("%d documents embedded after opting out", n)
	}

	// Opting out while a document is being embedded: its vectors aren't kept.
	expect(t, w.alice.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": true}), http.StatusOK)
	w.e.embedAll()
	w.put(w.alice, mine, "boat.md", "# Boat\n\nA sea vessel.\n")
	must[string](t)(w.e.idx.Sync(ctx, mine))
	release := w.e.embed.hold()
	calls = w.e.embed.calls.Load()
	done := make(chan error, 1)
	go func() {
		_, err := w.e.idx.embedPending(ctx)
		done <- err
	}()
	waitFor(t, "the worker to call the endpoint", func() bool { return w.e.embed.calls.Load() > calls })
	expect(t, w.alice.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": false}), http.StatusOK)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := w.e.spaceChunks(mine); n != 0 {
		t.Fatalf("%d chunks kept from an embedding finished after opting out", n)
	}

	// An admin repoints the account at another server: nothing goes there, no document and
	// no query, until a member of the space consents again, to it.
	c := decode[Consent](t, w.bob.do(http.MethodGet, embeddingURL(w.team), nil))
	if !c.Enabled || c.Endpoint == nil {
		t.Fatalf("team: %+v", c)
	}
	other := newFakeEmbeddings(t)
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/providers/%d", w.ws, c.Endpoint.Account),
		map[string]string{"base_url": other.srv.URL + "/v1"}), http.StatusOK)
	w.put(w.owner, w.team, "harbour.md", "# Harbour\n\nThe maritime quokka log.\n")
	must[string](t)(w.e.idx.Sync(ctx, w.team))
	w.e.embedAll()
	if res := w.find(w.team, "sea"); res.Mode != ModeFullText || res.Notice != noticeNoConsent {
		t.Fatalf("after the base URL changed: %+v", res)
	}
	if n := other.calls.Load(); n != 0 {
		t.Fatalf("the new endpoint was called %d times without consent", n)
	}
	changed := decode[Consent](t, w.bob.do(http.MethodGet, embeddingURL(w.team), nil))
	if changed.Enabled || changed.GivenBy == nil || changed.Endpoint.BaseURL != other.srv.URL+"/v1" {
		t.Fatalf("after the base URL changed: %+v", changed)
	}
	// Consent to the endpoint shown before the change isn't consent to the new one.
	rec = w.alice.do(http.MethodPut, embeddingURL(w.team), map[string]any{"enabled": true, "endpoint": c.Endpoint})
	expectCode(t, rec, http.StatusConflict, "consent_endpoint_changed", nil)
	if c := decode[Consent](t, w.bob.do(http.MethodGet, embeddingURL(w.team), nil)); c.Enabled || *c.GivenAt != *changed.GivenAt {
		t.Fatalf("after consent to the old endpoint: %+v", c)
	}
	// Alice, an editor, consents to it: the space is embedded again, by it alone.
	rec = w.alice.do(http.MethodPut, embeddingURL(w.team), map[string]any{"enabled": true, "endpoint": changed.Endpoint})
	expect(t, rec, http.StatusOK)
	if c := decode[Consent](t, rec); !c.Enabled || *c.GivenBy != w.alice.id {
		t.Fatalf("after consent to the new endpoint: %+v", c)
	}
	w.e.embedAll()
	if !other.sent("quokka") || !other.sent("team automobile") {
		t.Fatal("not embedded by the new endpoint")
	}
	if res := w.find(w.team, "sea"); res.Mode != ModeHybrid || len(res.Results) == 0 || res.Results[0].Path != "harbour.md" {
		t.Fatalf("consented again: %+v", res)
	}

	// Without a model, there is nothing to consent to.
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{"embeddings": nil}), http.StatusOK)
	expectCode(t, w.alice.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": true}), http.StatusConflict, "consent_no_model", nil)
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestSemanticSearch(t *testing.T) {
	w := newWorld(t)
	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)
	if s := decode[settingsReply](t, w.owner.do(http.MethodGet, settings, nil)); s.Embeddings != nil {
		t.Fatalf("settings = %+v", s)
	}
	account := w.addAccount("embed")
	set := func(u *user, account int64, model string) *httptest.ResponseRecorder {
		t.Helper()
		return u.do(http.MethodPut, settings, map[string]any{"embeddings": map[string]any{"account": account, "model": model}})
	}
	expect(t, set(w.alice, account, fakeModel), http.StatusForbidden)
	expect(t, set(w.owner, account, "no-such-model"), http.StatusBadRequest)
	expect(t, set(w.owner, account+100, fakeModel), http.StatusBadRequest)
	// A personal account never serves the workspace.
	rec := w.alice.do(http.MethodPost, "/api/me/providers", map[string]any{
		"name": "mine", "base_url": w.e.embed.srv.URL + "/v1", "api_key": fakeKey, "models": []map[string]string{{"id": "chat"}},
	})
	expect(t, rec, http.StatusCreated)
	personal := decode[struct{ ID int64 }](t, rec).ID
	expect(t, set(w.owner, personal, fakeModel), http.StatusBadRequest)

	expect(t, set(w.owner, account, fakeModel), http.StatusOK)
	w.consent(w.owner, w.team)
	s := decode[settingsReply](t, w.bob.do(http.MethodGet, settings, nil))
	if s.Embeddings == nil || s.Embeddings.Account != account || s.Embeddings.Model != fakeModel ||
		s.Embeddings.Provider != fmt.Sprintf("ws%d-embed", w.ws) {
		t.Fatalf("settings = %+v", s)
	}

	w.put(w.owner, w.team, "fleet.md", "# Fleet\n\nOur automobile maintenance schedule.\n")
	w.put(w.owner, w.team, "harbour.md", "# Harbour\n\nThe maritime log, and nothing else.\n")
	// Not embedded yet: hybrid, but it says what it can't find yet.
	res := w.find(w.team, "car")
	if res.Mode != ModeHybrid || res.Notice != fmt.Sprintf(noticePending, 2) ||
		!slices.Equal(res.Notices, []Notice{{Code: NoticePending, Count: 2}}) || len(res.Results) != 0 {
		t.Fatalf("before embedding: %+v", res)
	}
	if s := decode[settingsReply](t, w.bob.do(http.MethodGet, settings, nil)); s.Documents != 2 || s.Pending != 2 {
		t.Fatalf("settings = %+v", s)
	}
	w.e.embedAll()
	res = w.find(w.team, "car")
	if res.Mode != ModeHybrid || res.Notice != "" || len(res.Results) == 0 {
		t.Fatalf("after embedding: %+v", res)
	}
	if r := res.Results[0]; r.Path != "fleet.md" || r.Match != "semantic" || r.Commit == "" ||
		!strings.Contains(r.Snippet[0].Text, "automobile maintenance") {
		t.Fatalf("car: %+v", res.Results)
	}
	res = w.find(w.team, "maritime")
	if len(res.Results) == 0 || res.Results[0].Path != "harbour.md" || res.Results[0].Match != "both" {
		t.Fatalf("maritime: %+v", res.Results)
	}

	// A change is embedded again; until then its old vectors aren't used.
	doc := must[knowledge.Doc](t)(w.e.kb.Read(ctx, w.team, "fleet.md", ""))
	must[knowledge.Result](t)(w.e.kb.Put(ctx, w.team, "fleet.md", []byte("# Fleet\n\nSea, sea and sea: a ship on the ocean.\n"), doc.Version, by, ""))
	if res := w.find(w.team, "car"); slices.Contains(paths(res), "fleet.md") {
		t.Fatalf("stale vectors used: %+v", res.Results)
	}
	var stale int
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT count(*) FROM search_chunks c JOIN search_docs d ON d.id = c.doc_id
		WHERE d.path = 'fleet.md'`).Scan(&stale)
	if stale != 0 {
		t.Fatalf("%d vectors of the old version kept", stale)
	}
	w.e.embedAll()
	if res := w.find(w.team, "sea"); len(res.Results) == 0 || res.Results[0].Path != "fleet.md" {
		t.Fatalf("sea: %+v", res.Results)
	}

	// The query's embedding fails: full text, and it says so.
	w.e.embed.fail.Store(true)
	res = w.find(w.team, "maritime")
	if res.Mode != ModeFullText || res.Notice != noticeFailed || len(res.Results) != 1 {
		t.Fatalf("model down: %+v", res)
	}
	w.e.embed.fail.Store(false)

	// The operator tightens the egress policy to public addresses (the endpoint is on
	// 127.0.0.1): the query's embedding is refused before it connects, and the search says
	// why; so is setting a model again.
	open := w.e.idx.client
	w.e.idx.client = egress.MustParse("public").Client(time.Minute)
	calls := w.e.embed.calls.Load()
	res = w.find(w.team, "maritime")
	if res.Mode != ModeFullText || res.Notice != noticeRefused || len(res.Results) != 1 {
		t.Fatalf("refused by the egress policy: %+v", res)
	}
	rec = set(w.owner, account, fakeModel)
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "egress policy doesn't allow this endpoint: 127.0.0.1 is a loopback address") {
		t.Fatalf("setting a refused model: %s", rec.Body.String())
	}
	if w.e.embed.calls.Load() != calls {
		t.Fatal("a refused call reached the endpoint")
	}
	w.e.idx.client = open

	// Removing the model drops the vectors.
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{"embeddings": nil}), http.StatusOK)
	var chunks int
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT count(*) FROM search_chunks`).Scan(&chunks)
	if chunks != 0 {
		t.Fatalf("%d chunks left", chunks)
	}
	if res := w.find(w.team, "car"); res.Mode != ModeFullText || res.Notice != noticeOff {
		t.Fatalf("after removing the model: %+v", res)
	}
	// And so does deleting its account.
	expect(t, set(w.owner, account, fakeModel), http.StatusOK)
	w.e.embedAll()
	if res := w.find(w.team, "car"); res.Mode != ModeHybrid {
		t.Fatalf("model set again: %+v", res)
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/providers/%d", w.ws, account), nil), http.StatusNoContent)
	if res := w.find(w.team, "car"); res.Mode != ModeFullText {
		t.Fatalf("after deleting the account: %+v", res)
	}
}

func TestFullTextOnlyWithoutAModel(t *testing.T) {
	w := newWorld(t)
	w.put(w.owner, w.team, "fleet.md", "automobile")
	rec := w.search(w.bob, nil, "automobile")
	expect(t, rec, http.StatusOK)
	res := decode[Results](t, rec)
	if res.Mode != ModeFullText || res.Notice != noticeOff || !slices.Equal(res.Notices, []Notice{{Code: NoticeOff}}) || len(res.Results) != 1 {
		t.Fatalf("%s", rec.Body)
	}
	// Queries that can't be run say why, with a code.
	expectCode(t, w.search(w.bob, nil, " "), http.StatusBadRequest, "search_query_required", nil)
	expectCode(t, w.search(w.bob, nil, strings.Repeat("x", MaxQueryLen+1)), http.StatusBadRequest,
		"search_query_too_long", map[string]any{"max": float64(MaxQueryLen)})
	expectCode(t, w.search(w.bob, nil, `""`), http.StatusBadRequest, "search_query_no_words", nil)
	expectCode(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search?q=car&limit=-1", w.ws), nil),
		http.StatusBadRequest, "bad_request", nil)
	wantPaths(t, w.find(w.team, "car"))
	if n := must[int](t)(w.e.idx.embedPending(ctx)); n != 0 {
		t.Fatalf("%d documents embedded", n)
	}
	if w.e.embed.calls.Load() != 0 {
		t.Fatal("the embeddings endpoint was called")
	}
}

func TestEmbeddingRetries(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	clock := time.Now()
	w.e.idx.now = func() time.Time { return clock }
	w.e.embed.fail.Store(true)
	w.put(w.owner, w.team, "fleet.md", "automobile")
	must[string](t)(w.e.idx.Sync(ctx, w.team))

	if n := must[int](t)(w.e.idx.embedPending(ctx)); n != 1 {
		t.Fatalf("tried %d", n)
	}
	var tries int
	var after string
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT embed_tries, embed_after FROM search_docs`).Scan(&tries, &after)
	if tries != 1 || after <= clock.UTC().Format(timeFormat) {
		t.Fatalf("tries %d, after %s", tries, after)
	}
	// It waits, even once the endpoint is back.
	w.e.embed.fail.Store(false)
	if n := must[int](t)(w.e.idx.embedPending(ctx)); n != 0 {
		t.Fatalf("tried %d before its time", n)
	}
	clock = clock.Add(retryFirst)
	w.e.embedAll()
	if res := w.find(w.team, "car"); len(res.Results) != 1 || res.Notice != "" {
		t.Fatalf("after the retry: %+v", res)
	}
}

func TestAFailureDoesntDelayANewerVersion(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	w.put(w.owner, w.team, "fleet.md", "automobile")
	must[string](t)(w.e.idx.Sync(ctx, w.team))
	w.e.embed.fail.Store(true)
	release := w.e.embed.hold()
	done := make(chan error, 1)
	go func() {
		_, err := w.e.idx.embedPending(ctx)
		done <- err
	}()
	waitFor(t, "the worker to call the endpoint", func() bool { return w.e.embed.calls.Load() > 1 })
	// Changed while its old version is being embedded, which then fails.
	doc := must[knowledge.Doc](t)(w.e.kb.Read(ctx, w.team, "fleet.md", ""))
	must[knowledge.Result](t)(w.e.kb.Put(ctx, w.team, "fleet.md", []byte("a sedan"), doc.Version, by, ""))
	must[string](t)(w.e.idx.Sync(ctx, w.team))
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var tries int
	var after sql.NullString
	_ = w.e.idx.db.QueryRowContext(ctx, `SELECT embed_tries, embed_after FROM search_docs`).Scan(&tries, &after)
	if tries != 0 || after.Valid {
		t.Fatalf("the new version waits: tries %d, after %q", tries, after.String)
	}
	w.e.embed.fail.Store(false)
	w.e.embedAll()
	if res := w.find(w.team, "car"); len(res.Results) != 1 || res.Notice != "" {
		t.Fatalf("%+v", res)
	}
}

func TestWritesDontWaitForEmbeddings(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	release := w.e.embed.hold()
	stop := w.e.idx.Start(ctx)
	defer stop()
	start := time.Now()
	for i := range 5 {
		w.put(w.owner, w.team, fmt.Sprintf("d%d.md", i), "automobile")
	}
	waitFor(t, "the worker to call the endpoint", func() bool { return w.e.embed.calls.Load() > 0 })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("writes took %s", d)
	}
	release()
	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)
	waitFor(t, "every document to be embedded", func() bool {
		s := decode[settingsReply](t, w.owner.do(http.MethodGet, settings, nil))
		return s.Documents == 5 && s.Pending == 0
	})
}

func TestChunk(t *testing.T) {
	if got := chunk(""); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
	short := "# A\n\none\n\n## B\n\ntwo\n"
	if got := chunk(short); len(got) != 1 || got[0] != "# A\n\none\n\n## B\n\ntwo" {
		t.Fatalf("short sections are packed: %q", got)
	}
	long := "# A\n\n" + strings.Repeat("word ", 400) + "\n# B\n\n" + strings.Repeat("more ", 100) + "\n```\n# not a heading\n```\n"
	got := chunk(long)
	if len(got) < 3 {
		t.Fatalf("%d chunks", len(got))
	}
	for _, c := range got {
		if n := len([]rune(c)); n > chunkSize || n == 0 {
			t.Fatalf("chunk of %d characters", n)
		}
	}
	if last := got[len(got)-1]; !strings.HasPrefix(last, "# B") || !strings.Contains(last, "# not a heading") {
		t.Fatalf("last chunk %q", last)
	}
	if got := chunk(strings.Repeat("x", chunkSize*(maxChunks+5))); len(got) != maxChunks {
		t.Fatalf("%d chunks, want %d", len(got), maxChunks)
	}
}

func TestDocTitle(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"a/plan.md", "# Plan\ntext", "Plan"},
		{"a/plan.md", "text\n## Sub\n# Top #\n", "Top"},
		{"a/plan.md", "```\n# code\n```\nno title", "plan"},
		{"a/plan.md", "---\ntitle: 'Front'\n---\n# Heading", "Front"},
		{"a/plan.md", "---\ntitle: Unclosed\n# Heading", "Heading"},
		{".md", "", ".md"},
	} {
		if got := docTitle(c.name, c.body); got != c.want {
			t.Errorf("docTitle(%q, %q) = %q, want %q", c.name, c.body, got, c.want)
		}
	}
}

// A write whose context ends inside its transaction is rolled back before write returns, so no
// connection is left busy for the next writer or for closing the database.
func TestWriteCancelledInsideItsTransactionLeavesNoConnectionBusy(t *testing.T) {
	e := newEnv(t)
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		err := e.idx.write(ctx, func(tx *sql.Tx) error {
			cancel()
			_, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name) VALUES ('Cancelled')`)
			return err
		})
		if err == nil {
			t.Fatal("a write cancelled inside its transaction succeeded")
		}
		if inUse := e.idx.db.Stats().InUse; inUse != 0 {
			t.Fatalf("%d connections still busy after write returned", inUse)
		}
	}
}
