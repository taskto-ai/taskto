package agenttools

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

// space returns a space as its writer sees it, to read the knowledge base around the tools.
func (w *world) space(id, workspace int64) access.Space {
	return access.Space{ID: id, WorkspaceID: workspace, Access: access.Write}
}

// seed puts a file in a space as alice, without the tools.
func (w *world) seed(space, workspace int64, name string, content []byte) knowledge.Result {
	w.t.Helper()
	res, err := w.kb.Put(context.Background(), w.space(space, workspace), name, content, "",
		knowledge.Author{Name: "Alice", Email: "alice@example.com"}, "")
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

func TestDocToolsDefinitions(t *testing.T) {
	readOnly := map[string]bool{}
	for _, d := range Definitions() {
		readOnly[d.Name] = d.ReadOnly
	}
	// Only the reading ones run without asking the owner: moving, deleting and restoring
	// ask, like write_doc.
	for tool, want := range map[string]bool{"read_doc": true, "doc_history": true, "move_doc": false, "delete_doc": false, "restore_doc": false, "write_doc": false} {
		if got, ok := readOnly[tool]; !ok || got != want {
			t.Errorf("%s: read_only = %v, defined %v, want %v", tool, got, ok, want)
		}
	}
}

// A document is changed, moved, deleted and brought back, and its history says who did what.
func TestDocHistoryMoveDeleteRestore(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sp := map[string]any{"space_id": w.shared}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range sp {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	v1 := w.ok(w.aliceConv, "write_doc", with(map[string]any{"path": "plan.md", "content": "first", "message": "Draft"}))
	v2 := w.ok(w.aliceConv, "write_doc", with(map[string]any{
		"path": "plan.md", "content": "second", "base_version": v1["doc"].(map[string]any)["version"], "message": "Rewrite",
	}))

	h := w.ok(w.aliceConv, "doc_history", with(map[string]any{"path": "plan.md"}))
	commits := h["commits"].([]any)
	if len(commits) != 2 {
		t.Fatalf("history = %v", h)
	}
	newest, oldest := commits[0].(map[string]any), commits[1].(map[string]any)
	if newest["commit"] != v2["commit"] || newest["change"] != "update" || newest["message"] != "Rewrite" || newest["agent"] != AgentName ||
		newest["author"].(map[string]any)["email"] != "alice@example.com" {
		t.Fatalf("newest commit = %v", newest)
	}
	if oldest["commit"] != v1["commit"] || oldest["change"] != "create" {
		t.Fatalf("oldest commit = %v", oldest)
	}
	// Paging: one at a time, with the next page's cursor.
	page := w.ok(w.aliceConv, "doc_history", with(map[string]any{"path": "plan.md", "limit": 1}))
	if len(page["commits"].([]any)) != 1 || page["next"] != v2["commit"] {
		t.Fatalf("first page = %v", page)
	}
	page = w.ok(w.aliceConv, "doc_history", with(map[string]any{"path": "plan.md", "limit": 1, "before": page["next"]}))
	if got := page["commits"].([]any); len(got) != 1 || got[0].(map[string]any)["commit"] != v1["commit"] {
		t.Fatalf("second page = %v", page)
	}

	// The older version is read at its commit; now is the newer.
	if got := w.ok(w.aliceConv, "read_doc", with(map[string]any{"path": "plan.md", "at": oldest["commit"]})); got["content"] != "first" || got["at"] != oldest["commit"] {
		t.Fatalf("read at the first commit = %v", got)
	}
	if got := w.ok(w.aliceConv, "read_doc", with(map[string]any{"path": "plan.md"})); got["content"] != "second" || got["at"] != nil {
		t.Fatalf("read now = %v", got)
	}

	// A stale base_version is a conflict, and nothing moves.
	w.status(http.StatusConflict, w.aliceConv, "move_doc", with(map[string]any{"from": "plan.md", "to": "old/plan.md", "base_version": v1["doc"].(map[string]any)["version"]}))
	moved := w.ok(w.aliceConv, "move_doc", with(map[string]any{
		"from": "plan.md", "to": "archive/plan.md", "base_version": v2["doc"].(map[string]any)["version"], "message": "Archive the plan",
	}))
	if moved["doc"].(map[string]any)["path"] != "archive/plan.md" {
		t.Fatalf("move_doc = %v", moved)
	}
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", with(map[string]any{"path": "plan.md"}))
	// The destination must not exist; a document doesn't move into itself.
	w.ok(w.aliceConv, "write_doc", with(map[string]any{"path": "other.md", "content": "x"}))
	w.status(http.StatusConflict, w.aliceConv, "move_doc", with(map[string]any{"from": "other.md", "to": "archive/plan.md"}))
	w.status(http.StatusBadRequest, w.aliceConv, "move_doc", with(map[string]any{"from": "other.md", "to": "other.md"}))
	// A folder moves whole.
	w.ok(w.aliceConv, "move_doc", with(map[string]any{"from": "archive", "to": "kept"}))
	if got := w.ok(w.aliceConv, "read_doc", with(map[string]any{"path": "kept/plan.md"})); got["content"] != "second" {
		t.Fatalf("read after the folder moved = %v", got)
	}

	// Delete, and bring it back from the history of its old path.
	w.status(http.StatusBadRequest, w.aliceConv, "delete_doc", with(map[string]any{"path": "kept"})) // a folder
	w.status(http.StatusConflict, w.aliceConv, "delete_doc", with(map[string]any{"path": "kept/plan.md", "base_version": v1["doc"].(map[string]any)["version"]}))
	deleted := w.ok(w.aliceConv, "delete_doc", with(map[string]any{"path": "kept/plan.md", "message": "Not needed"}))
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", with(map[string]any{"path": "kept/plan.md"}))
	h = w.ok(w.aliceConv, "doc_history", with(map[string]any{"path": "kept/plan.md"}))
	if c := h["commits"].([]any)[0].(map[string]any); c["commit"] != deleted["commit"] || c["change"] != "delete" {
		t.Fatalf("history after the delete = %v", h)
	}
	restored := w.ok(w.aliceConv, "restore_doc", with(map[string]any{"path": "plan.md", "commit": v1["commit"], "message": "Back to the draft"}))
	if restored["doc"].(map[string]any)["path"] != "plan.md" {
		t.Fatalf("restore_doc = %v", restored)
	}
	if got := w.ok(w.aliceConv, "read_doc", with(map[string]any{"path": "plan.md"})); got["content"] != "first" {
		t.Fatalf("read after the restore = %v", got)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "restore_doc", with(map[string]any{"path": "plan.md", "commit": "nope"}))
	w.status(http.StatusNotFound, w.aliceConv, "restore_doc", with(map[string]any{"path": "never.md", "commit": v1["commit"]}))

	// Every change is the user's, made by the agent.
	all, err := w.kb.History(ctx, w.space(w.shared, w.one), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all.Commits {
		if c.Author.Email != "alice@example.com" || c.Agent != AgentName {
			t.Errorf("commit %+v isn't alice's, made by the agent", c)
		}
	}
	// Without a path, the history is the space's.
	if got := w.ok(w.aliceConv, "doc_history", sp)["commits"].([]any); len(got) != len(all.Commits) {
		t.Fatalf("the space's history has %d commits, want %d", len(got), len(all.Commits))
	}
	w.status(http.StatusBadRequest, w.aliceConv, "doc_history", with(map[string]any{"limit": 51}))
	w.status(http.StatusBadRequest, w.aliceConv, "doc_history", with(map[string]any{"before": "nope"}))
	w.status(http.StatusBadRequest, w.aliceConv, "doc_history", with(map[string]any{"purge": true}))
}

// Plain-text files are read; binaries, large files and unversioned ones are not.
func TestReadDocTextFiles(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	read := func(path string) (int, map[string]any) {
		return w.call(w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": path})
	}
	w.seed(sp, w.one, "data/rows.csv", []byte("a,b\n1,2\n"))
	w.seed(sp, w.one, "src/main.go", []byte("package main\n"))
	w.seed(sp, w.one, "photo.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	w.seed(sp, w.one, "latin1.txt", []byte("caf\xe9"))
	w.seed(sp, w.one, "big.txt", []byte(strings.Repeat("x", maxTextFile+1)))
	w.seed(sp, w.one, "song.mp3", []byte("ID3"))
	w.seed(sp, w.one, "just-over.txt", []byte(strings.Repeat("y", readChunk+10)))

	if code, out := read("data/rows.csv"); code != http.StatusOK || out["content"] != "a,b\n1,2\n" || out["size"] != float64(8) {
		t.Fatalf("csv = %d %v", code, out)
	}
	if code, out := read("src/main.go"); code != http.StatusOK || out["content"] != "package main\n" {
		t.Fatalf("go file = %d %v", code, out)
	}
	for _, name := range []string{"photo.png", "latin1.txt", "big.txt", "song.mp3"} {
		if code, out := read(name); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want a refusal", name, code, out)
		}
	}
	// Pieces, as for a document.
	code, out := read("just-over.txt")
	if code != http.StatusOK || len(out["content"].(string)) != readChunk || out["next_offset"] != float64(readChunk) {
		t.Fatalf("first piece = %d %v", code, out["next_offset"])
	}
	out = w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "just-over.txt", "offset": readChunk})
	if out["content"] != strings.Repeat("y", 10) || out["next_offset"] != nil {
		t.Fatalf("last piece = %v", out)
	}
	// A folder, or a file that isn't there.
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "data"})
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "nothing.txt"})
	// A commit that isn't one, and one that doesn't exist.
	w.status(http.StatusBadRequest, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "src/main.go", "at": "main"})
	w.status(http.StatusNotFound, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "src/main.go", "at": strings.Repeat("a", 40)})
}

// An unversioned file's older versions' bytes are gone: restoring one is refused, not a crash.
func TestRestoreDocUnversionedIsGone(t *testing.T) {
	w := newWorld(t)
	first := w.seed(w.alicePrivate, w.one, "song.mp3", []byte("ID3 first"))
	if _, err := w.kb.Put(context.Background(), w.space(w.alicePrivate, w.one), "song.mp3", []byte("ID3 second"), first.Doc.Version,
		knowledge.Author{Name: "Alice", Email: "alice@example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	w.status(http.StatusGone, w.aliceConv, "restore_doc", map[string]any{"space_id": w.alicePrivate, "path": "song.mp3", "commit": first.Commit})
}

// The new tools reach only what the user may: another person's space, another workspace's, and
// a space they may only read, are refused, and nothing changes.
func TestDocToolsAreIsolated(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	secret := w.seed(w.bobPrivate, w.one, "secret.md", []byte("bob's"))
	shared := w.seed(w.shared, w.one, "team.md", []byte("team"))
	elsewhere := w.seed(w.alicePrivate2, w.two, "elsewhere.md", []byte("two"))

	// Not bob's private space, not a space of another workspace (the conversation's), not one
	// that doesn't exist: not found, the same for every tool.
	for _, space := range []int64{w.bobPrivate, w.alicePrivate2, w.alicePrivate2 + 1000} {
		for tool, args := range map[string]map[string]any{
			"doc_history": {"path": "secret.md"},
			"read_doc":    {"path": "secret.md", "at": secret.Commit},
			"move_doc":    {"from": "secret.md", "to": "mine.md"},
			"delete_doc":  {"path": "secret.md"},
			"restore_doc": {"path": "secret.md", "commit": secret.Commit},
		} {
			args["space_id"] = space
			if out := w.status(http.StatusNotFound, w.aliceConv, tool, args); out["error"] != "not found" {
				t.Errorf("%s in space %d: %v", tool, space, out)
			}
		}
	}
	// Alice's conversation in the other workspace can't reach this workspace's shared space.
	for tool, args := range map[string]map[string]any{
		"doc_history": {"path": "team.md"},
		"move_doc":    {"from": "team.md", "to": "x.md"},
		"delete_doc":  {"path": "team.md"},
		"restore_doc": {"path": "team.md", "commit": shared.Commit},
	} {
		args["space_id"] = w.shared
		w.status(http.StatusNotFound, w.aliceConv2, tool, args)
	}
	// And her conversation here can't reach the other workspace's space, though she is a member.
	w.status(http.StatusNotFound, w.aliceConv, "doc_history", map[string]any{"space_id": w.alicePrivate2})
	w.status(http.StatusNotFound, w.aliceConv, "restore_doc", map[string]any{"space_id": w.alicePrivate2, "path": "elsewhere.md", "commit": elsewhere.Commit})

	// Bob views the shared space: he reads its history and old versions, and changes nothing.
	if h := w.ok(w.bobConv, "doc_history", map[string]any{"space_id": w.shared, "path": "team.md"}); len(h["commits"].([]any)) != 1 {
		t.Fatalf("bob's view of the history = %v", h)
	}
	if got := w.ok(w.bobConv, "read_doc", map[string]any{"space_id": w.shared, "path": "team.md", "at": shared.Commit}); got["content"] != "team" {
		t.Fatalf("bob's read at a commit = %v", got)
	}
	for tool, args := range map[string]map[string]any{
		"move_doc":    {"from": "team.md", "to": "x.md"},
		"delete_doc":  {"path": "team.md"},
		"restore_doc": {"path": "team.md", "commit": shared.Commit},
	} {
		args["space_id"] = w.shared
		w.status(http.StatusForbidden, w.bobConv, tool, args)
	}
	// An archived space is read-only for its editor too.
	w.exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.shared)
	w.status(http.StatusForbidden, w.aliceConv, "delete_doc", map[string]any{"space_id": w.shared, "path": "team.md"})
	w.exec(`UPDATE spaces SET archived_at = NULL WHERE id = ?`, w.shared)

	// Nothing was changed: each space has the one commit it started with.
	for _, c := range []struct {
		id, workspace int64
		name          string
	}{{w.bobPrivate, w.one, "secret.md"}, {w.shared, w.one, "team.md"}, {w.alicePrivate2, w.two, "elsewhere.md"}} {
		h, err := w.kb.History(ctx, w.space(c.id, c.workspace), "", "", 0)
		if err != nil || len(h.Commits) != 1 {
			t.Errorf("space %d: history = %+v, %v", c.id, h, err)
		}
		if _, err := w.kb.Read(ctx, w.space(c.id, c.workspace), c.name, ""); err != nil {
			t.Errorf("space %d: %s: %v", c.id, c.name, err)
		}
	}
	// Purging history is no tool.
	w.status(http.StatusNotFound, w.aliceConv, "remove_old_versions", map[string]any{"space_id": w.alicePrivate, "path": "x.md"})
}

// A chunk of characters that JSON writes as six bytes is shortened to fit the reply, and the
// next piece continues after it, so the whole text is read.
func TestReadDocEscapedChunkFits(t *testing.T) {
	w := newWorld(t)
	text := strings.Repeat("<", readChunk) + "end"
	w.seed(w.alicePrivate, w.one, "angles.txt", []byte(text))
	var got strings.Builder
	offset := 0
	for i := 0; i < 100; i++ {
		out := w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": w.alicePrivate, "path": "angles.txt", "offset": offset})
		got.WriteString(out["content"].(string))
		next, more := out["next_offset"].(float64)
		if !more {
			break
		}
		offset = int(next)
	}
	if got.String() != text {
		t.Fatalf("read %d bytes, want %d", got.Len(), len(text))
	}
}

// The built-in agent asks no one before it deletes, so delete_doc refuses what history can't bring
// back: an unversioned file's bytes. A document it deletes is kept by its history.
func TestDeleteDocRefusesUnversionedFiles(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	w.seed(sp, w.one, "song.mp3", []byte("ID3 audio"))
	w.seed(sp, w.one, "backup.zip", []byte("PK archive"))
	w.seed(sp, w.one, "plan.md", []byte("plan"))

	for _, name := range []string{"song.mp3", "backup.zip"} {
		out := w.status(http.StatusBadRequest, w.aliceConv, "delete_doc", map[string]any{"space_id": sp, "path": name})
		if msg, _ := out["error"].(string); !strings.Contains(msg, "web app") {
			t.Errorf("%s: the refusal doesn't say who deletes it: %v", name, out)
		}
		_, f, err := w.kb.Open(context.Background(), w.space(sp, w.one), name, "")
		if err != nil {
			t.Fatalf("%s is gone: %v", name, err)
		}
		_ = f.Close()
	}
	w.ok(w.aliceConv, "delete_doc", map[string]any{"space_id": sp, "path": "plan.md"})
}

// A rename that would turn a versioned file into an unversioned one is refused: it would take the
// file's older versions out of history.
func TestMoveDocRefusesAStorageChange(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	first := w.seed(sp, w.one, "draft.md", []byte("one"))
	if _, err := w.kb.Put(context.Background(), w.space(sp, w.one), "draft.md", []byte("two"), first.Doc.Version,
		knowledge.Author{Name: "Alice", Email: "alice@example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "move_doc", map[string]any{"space_id": sp, "from": "draft.md", "to": "draft.zip"})
	if got := w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "draft.md", "at": first.Commit}); got["content"] != "one" {
		t.Fatalf("the first version = %v", got)
	}
	h := w.ok(w.aliceConv, "doc_history", map[string]any{"space_id": sp, "path": "draft.md"})
	if len(h["commits"].([]any)) != 2 {
		t.Fatalf("history = %v", h)
	}
	w.ok(w.aliceConv, "move_doc", map[string]any{"space_id": sp, "from": "draft.md", "to": "draft.txt"})
}

// A destination that is an existing file's name in another Unicode form is a conflict: the
// move would overwrite it.
func TestMoveDocUnicodeEquivalentDestination(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	w.seed(sp, w.one, "café.md", []byte("the cafe"))
	w.seed(sp, w.one, "draft.md", []byte("draft"))
	w.status(http.StatusConflict, w.aliceConv, "move_doc", map[string]any{"space_id": sp, "from": "draft.md", "to": "café.md"})
	if got := w.ok(w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "café.md"}); got["content"] != "the cafe" {
		t.Fatalf("the existing document = %v", got)
	}
}

// read_doc returns the text or refuses: a binary under a Markdown name, and an offset inside a
// character, would otherwise come back as replacement characters.
func TestReadDocNeverReturnsCorruptedText(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	w.seed(sp, w.one, "blob.bin", []byte("\x89PNG\r\n\x1a\n\x00\xff"))
	w.status(http.StatusBadRequest, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "blob.bin"})
	w.ok(w.aliceConv, "move_doc", map[string]any{"space_id": sp, "from": "blob.bin", "to": "blob.md"}) // both are versioned
	w.status(http.StatusBadRequest, w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "blob.md"})

	w.seed(sp, w.one, "word.txt", []byte("aéb"))
	read := func(offset int) (int, map[string]any) {
		return w.call(w.aliceConv, "read_doc", map[string]any{"space_id": sp, "path": "word.txt", "offset": offset})
	}
	for offset, want := range map[int]string{0: "aéb", 1: "éb", 3: "b", 4: ""} {
		if code, out := read(offset); code != http.StatusOK || out["content"] != want {
			t.Errorf("offset %d = %d %v, want %q", offset, code, out, want)
		}
	}
	if code, out := read(2); code != http.StatusBadRequest {
		t.Errorf("an offset inside é = %d %v, want a refusal", code, out)
	}
	if code, out := read(5); code != http.StatusBadRequest {
		t.Errorf("an offset past the end = %d %v, want a refusal", code, out)
	}
}

// restore_doc, which the built-in agent calls with no one to ask, refuses to replace an
// unversioned file with different content: the current bytes would be removed for good.
func TestRestoreDocKeepsUnversionedBytes(t *testing.T) {
	w := newWorld(t)
	sp := w.alicePrivate
	first := w.seed(sp, w.one, "archive.zip", []byte("PK first"))
	w.seed(sp, w.one, "copy.zip", []byte("PK first")) // keeps the first version's bytes on disk
	if _, err := w.kb.Put(context.Background(), w.space(sp, w.one), "archive.zip", []byte("PK second"), first.Doc.Version,
		knowledge.Author{Name: "Alice", Email: "alice@example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "restore_doc", map[string]any{"space_id": sp, "path": "archive.zip", "commit": first.Commit})
	_, f, err := w.kb.Open(context.Background(), w.space(sp, w.one), "archive.zip", "")
	if err != nil {
		t.Fatalf("archive.zip is gone: %v", err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "PK second" {
		t.Fatalf("archive.zip = %q", b)
	}
}
