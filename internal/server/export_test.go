package server

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/export"
)

func exportPath(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/export", ws) }

// entriesRuntime is an agent runtime that has only conversations' entries, by the agent's ID.
type entriesRuntime struct {
	conversations.Runtime
	entries map[int64][]string // the entries' text, oldest first; entry IDs are 1, 2, 3…
}

func (e entriesRuntime) Entries(_ context.Context, conv, before int64, limit int) (json.RawMessage, error) {
	texts, ok := e.entries[conv]
	if !ok {
		return nil, fmt.Errorf("no conversation %d", conv)
	}
	type entry struct {
		ID    int64          `json:"id"`
		Kind  string         `json:"kind"`
		Model map[string]any `json:"model"`
	}
	var all []entry
	for i, text := range texts {
		if id := int64(i + 1); before == 0 || id < before {
			all = append(all, entry{ID: id, Kind: "pi.user", Model: map[string]any{"content": text}})
		}
	}
	page := struct {
		Entries []entry `json:"entries"`
		Before  int64   `json:"before,omitempty"`
	}{Entries: []entry{}}
	if len(all) > limit {
		page.Before = all[len(all)-limit-1].ID
		all = all[len(all)-limit:]
	}
	page.Entries = append(page.Entries, all...)
	return json.Marshal(page)
}

// exportStack is a stack whose conversations have a runtime with the entries of the agents' IDs.
func exportStack(t *testing.T, entries map[int64][]string, mod func(*Options)) *stack {
	t.Helper()
	return newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		o.Conversations = conversations.New(sqlDB, func(context.Context) (conversations.Runtime, error) {
			return entriesRuntime{entries: entries}, nil
		})
		if mod != nil {
			mod(o)
		}
	})
}

func (s *stack) conversation(ws, owner, agent int64, title string) int64 {
	s.t.Helper()
	return s.scalar(`INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, ?, ?) RETURNING id`, ws, owner, agent, title)
}

// put writes a document's text.
func (s *stack) put(cookie string, ws, space int64, name, content string) {
	s.t.Helper()
	if code, body := s.browser(cookie, http.MethodPut, s.docs(ws, space)+"/"+name, map[string]any{"content": content}); code != http.StatusCreated {
		s.t.Fatalf("put %s = %d %v", name, code, body)
	}
}

// putFile writes a document's bytes, as a browser uploads a file.
func (s *stack) putFile(cookie string, ws, space int64, name string, content []byte) {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodPut, s.docs(ws, space)+"/"+name, bytes.NewReader(content))
	r.Header.Set("Content-Type", "application/octet-stream")
	sameOrigin(cookie)(r)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("putFile %s = %d %s", name, rec.Code, rec.Body)
	}
}

// card makes a card on the board of a space, with a comment, and returns it.
func (s *stack) card(cookie string, ws, space, board int64, title, comment string) map[string]any {
	s.t.Helper()
	base := fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards", ws, space, board)
	code, c := s.browser(cookie, http.MethodPost, base, map[string]any{"title": title, "body": title + " body", "column": "todo"})
	if code != http.StatusCreated {
		s.t.Fatalf("create card = %d %v", code, c)
	}
	if code, body := s.browser(cookie, http.MethodPost, fmt.Sprintf("%s/%d/comments", base, id(c["id"])), map[string]any{"body": comment}); code != http.StatusCreated {
		s.t.Fatalf("comment = %d %v", code, body)
	}
	return c
}

func (s *stack) boardOf(space int64) int64 {
	return s.scalar(`SELECT id FROM boards WHERE space_id = ?`, space)
}

// unzip reads a response's zip into its entries' bytes by name.
func unzip(t *testing.T, rec *httptest.ResponseRecorder) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v (status %d, %.200s)", err, rec.Code, rec.Body)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if _, dup := out[f.Name]; dup {
			t.Fatalf("two entries named %q", f.Name)
		}
		out[f.Name] = b
	}
	return out
}

// startExport is the POST that starts an export.
func (s *stack) startExport(cookie string, ws int64) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.request(http.MethodPost, exportPath(ws), map[string]any{}, sameOrigin(cookie))
}

// downloadOf is the address a start gave.
func downloadOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Download  string `json:"download"`
		ExpiresIn int    `json:"expires_in"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Download == "" || got.ExpiresIn <= 0 {
		t.Fatalf("start answered %s (%v)", rec.Body, err)
	}
	return got.Download
}

// export starts an export and downloads it. What the start refuses is returned as it is.
func (s *stack) export(cookie string, ws int64) *httptest.ResponseRecorder {
	s.t.Helper()
	start := s.startExport(cookie, ws)
	if start.Code != http.StatusOK {
		return start
	}
	return s.request(http.MethodGet, downloadOf(s.t, start), nil, sameOrigin(cookie))
}

// downloadRequest starts an export and returns the request that downloads it.
func (s *stack) downloadRequest(cookie string, ws int64) *http.Request {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodGet, downloadOf(s.t, s.startExport(cookie, ws)), nil)
	sameOrigin(cookie)(r)
	return r
}

// A person's export holds what they own in the workspace and nothing else: not other people's
// private spaces, not shared spaces, not their own data in another workspace.
func TestExportHoldsOnlyWhatThePersonOwns(t *testing.T) {
	s := exportStack(t, map[int64][]string{
		1: {"alice-chat one", "alice-chat two"},
		2: {"bob-secret chat"},
		3: {"two-secret chat"},
		4: {"alice archived chat"},
	}, nil)
	aliceConv := s.conversation(s.one, s.alice.id, 1, "Alice's chat")
	s.conversation(s.one, s.bob.id, 2, "Bob's chat")
	s.conversation(s.two, s.alice.id, 3, "Alice's chat in two")
	archived := s.conversation(s.one, s.alice.id, 4, "Old one")
	s.exec(`UPDATE conversations SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, archived)

	// Alice's own: documents (one with a name that is trouble in a zip), a large file, cards, an app.
	s.put(s.alice.cookie, s.one, s.alice.private, "notes/plan.md", "# alice-plan\n")
	s.put(s.alice.cookie, s.one, s.alice.private, "notes/a:b*c.md", "alice-colon")
	media := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB: audio and video are never versioned
	s.putFile(s.alice.cookie, s.one, s.alice.private, "media/clip.mp4", media)
	live := s.card(s.alice.cookie, s.one, s.alice.private, s.alicePersonalBoard, "alice-card", "alice-comment")
	gone := s.card(s.alice.cookie, s.one, s.alice.private, s.alicePersonalBoard, "alice-archived-card", "alice-old-comment")
	if code, body := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards/%d/archive", s.one, s.alice.private, s.alicePersonalBoard, id(gone["id"])),
		map[string]any{"version": id(gone["version"])}); code != http.StatusOK {
		t.Fatalf("archive = %d %v", code, body)
	}
	s.app(s.alice.cookie, s.alice.private, "alice-expense")

	// What is not hers: others' private spaces, shared spaces she is in (Team) and isn't in
	// (Secret), and her own data in the other workspace.
	for _, other := range []struct {
		cookie string
		ws, sp int64
	}{{s.bob.cookie, s.one, s.bob.private}, {s.carol.cookie, s.one, s.carol.private}, {s.carol.cookie, s.one, s.secret}, {s.bob.cookie, s.one, s.team}, {s.alice.cookie, s.two, s.alicePrivate2}} {
		s.put(other.cookie, other.ws, other.sp, "hidden.md", "stranger-secret in "+strconv.FormatInt(other.sp, 10))
		if other.sp != s.team && other.sp != s.secret { // a shared space has no board until someone makes one
			s.card(other.cookie, other.ws, other.sp, s.boardOf(other.sp), "stranger-card", "stranger-comment")
		}
		if other.ws == s.one { // the example app helper works in workspace one
			s.app(other.cookie, other.sp, "stranger-expense")
		}
	}

	// Alice is a member of Team, which has a board with a card in it: shared content isn't hers to take.
	code, tb := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", s.one, s.team), map[string]any{"name": "Team board"})
	if code != http.StatusCreated {
		t.Fatalf("team board = %d %v", code, tb)
	}
	s.card(s.bob.cookie, s.one, s.team, id(tb["id"]), "stranger-team-card", "stranger-team-comment")

	rec := s.export(s.alice.cookie, s.one)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="taskto-export-`) || !strings.HasSuffix(cd, `.zip"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	files := unzip(t, rec)

	// Everything, as the person wrote it.
	if got := string(files["documents/notes/plan.md"]); got != "# alice-plan\n" {
		t.Errorf("plan.md = %q", got)
	}
	if !bytes.Equal(files["documents/media/clip.mp4"], media) {
		t.Errorf("the large file's bytes aren't whole: %d bytes, want %d", len(files["documents/media/clip.mp4"]), len(media))
	}
	var haveColon bool
	for name, b := range files {
		if strings.HasPrefix(name, "documents/notes/a") && string(b) == "alice-colon" {
			haveColon = true
			if strings.ContainsAny(name, `:*`) {
				t.Errorf("entry %q keeps characters that aren't safe in a file name", name)
			}
		}
	}
	if !haveColon {
		t.Errorf("the document with the awkward name is missing: %v", names(files))
	}

	var board struct {
		Cards []struct {
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			Column   string `json:"column"`
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
		} `json:"cards"`
	}
	boardJSON := fmt.Sprintf("boards/%d-Personal.json", s.alicePersonalBoard)
	if b, ok := files[boardJSON]; !ok {
		t.Fatalf("no %s in %v", boardJSON, names(files))
	} else if err := json.Unmarshal(b, &board); err != nil {
		t.Fatal(err)
	}
	if len(board.Cards) != 2 || board.Cards[0].Title != "alice-card" || board.Cards[0].Comments[0].Body != "alice-comment" ||
		board.Cards[1].Title != "alice-archived-card" || board.Cards[1].Column != "archived" || board.Cards[1].Comments[0].Body != "alice-old-comment" {
		t.Errorf("board = %+v, want her live and archived cards with their comments", board)
	}
	if md := string(files[strings.TrimSuffix(boardJSON, ".json")+".md"]); !strings.Contains(md, "alice-card") || !strings.Contains(md, "alice-comment") || !strings.Contains(md, "alice-old-comment") {
		t.Errorf("board markdown = %q", md)
	}
	_ = live

	var convo struct {
		Conversation struct {
			Title string `json:"title"`
		} `json:"conversation"`
		Transcript string            `json:"transcript"`
		Entries    []json.RawMessage `json:"entries"`
	}
	convFile := fmt.Sprintf("conversations/%d-Alice's chat.json", aliceConv)
	if b, ok := files[convFile]; !ok {
		t.Fatalf("no %s in %v", convFile, names(files))
	} else if err := json.Unmarshal(b, &convo); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	if convo.Transcript != "complete" || len(convo.Entries) != 2 || !strings.Contains(string(convo.Entries[0]), "alice-chat one") || !strings.Contains(string(convo.Entries[1]), "alice-chat two") {
		t.Errorf("conversation = %+v, want her two entries in order", convo)
	}

	var appFile struct {
		Rows []map[string]any `json:"records"`
	}
	for name, b := range files {
		if strings.HasPrefix(name, "apps/") {
			if err := json.Unmarshal(b, &appFile); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(appFile.Rows) != 1 || appFile.Rows[0]["description"] != "alice-expense" {
		t.Errorf("app records = %v, want her one expense", appFile.Rows)
	}

	var mf struct {
		Format    int            `json:"format"`
		Counts    map[string]int `json:"counts"`
		User      struct{ ID int64 }
		Workspace struct{ ID int64 }
		Skipped   []any `json:"skipped"`
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"documents": 3, "boards": 1, "cards": 2, "comments": 2, "conversations": 2, "apps": 1, "records": 1}
	for k, v := range want {
		if mf.Counts[k] != v {
			t.Errorf("manifest counts[%s] = %d, want %d (%v)", k, mf.Counts[k], v, mf.Counts)
		}
	}
	if mf.Format != export.Format || mf.User.ID != s.alice.id || mf.Workspace.ID != s.one || len(mf.Skipped) != 0 {
		t.Errorf("manifest = %+v", mf)
	}

	// Not one byte of anyone else's, or of her own in another workspace.
	for name, b := range files {
		for _, secret := range []string{"stranger", "bob-secret", "two-secret", "carol", "bob@", "Bob's chat", "Alice's chat in two"} {
			if bytes.Contains(b, []byte(secret)) || strings.Contains(name, secret) {
				t.Errorf("%s holds %q, which isn't hers in this workspace", name, secret)
			}
		}
	}
	for name := range files {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
			t.Errorf("unsafe entry name %q", name)
		}
	}

	// It is on the audit log: who took their data out of which workspace.
	if n := s.scalar(`SELECT count(*) FROM audit_events WHERE action = 'data_export_started' AND actor_id = ? AND workspace_id = ? AND target_id = ?`,
		s.alice.id, s.one, s.alice.id); n != 1 {
		t.Errorf("%d data_export_started events for alice's export, want 1", n)
	}

	// The other workspace's export is that workspace's alone.
	two := unzip(t, s.export(s.alice.cookie, s.two))
	for name, b := range two {
		if bytes.Contains(b, []byte("alice-plan")) || bytes.Contains(b, []byte("alice-expense")) || bytes.Contains(b, []byte("alice-chat")) {
			t.Errorf("workspace two's export holds workspace one's %s", name)
		}
	}
	if !bytes.Contains(two["documents/hidden.md"], []byte("stranger-secret")) {
		t.Errorf("workspace two's export lacks her own document there: %v", names(two))
	}
}

func names(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	return out
}

// Someone else's export is theirs: it names no one's data but their own, whoever asks.
func TestExportOfAnotherPersonHasNoneOfTheFirstsData(t *testing.T) {
	s := exportStack(t, map[int64][]string{1: {"alice-chat"}}, nil)
	s.conversation(s.one, s.alice.id, 1, "Alice's chat")
	s.put(s.alice.cookie, s.one, s.alice.private, "plan.md", "alice-plan")
	s.card(s.alice.cookie, s.one, s.alice.private, s.alicePersonalBoard, "alice-card", "alice-comment")
	s.app(s.alice.cookie, s.alice.private, "alice-expense")
	s.put(s.bob.cookie, s.one, s.team, "team.md", "team-doc")

	files := unzip(t, s.export(s.bob.cookie, s.one))
	for name, b := range files {
		if bytes.Contains(b, []byte("alice")) && !strings.HasPrefix(name, "manifest.json") {
			t.Errorf("bob's export has alice's data in %s", name)
		}
		if bytes.Contains(b, []byte("team-doc")) {
			t.Errorf("bob's export has a shared space's document in %s: his access to it isn't ownership", name)
		}
	}
	if len(files) == 0 || files["manifest.json"] == nil {
		t.Fatalf("bob's export = %v", names(files))
	}
	var mf struct{ User struct{ ID int64 } }
	_ = json.Unmarshal(files["manifest.json"], &mf)
	if mf.User.ID != s.bob.id {
		t.Errorf("manifest names user %d, want bob %d", mf.User.ID, s.bob.id)
	}
}

func TestExportNeedsAMemberWithASessionNotAToken(t *testing.T) {
	s := exportStack(t, nil, nil)
	if rec := s.request(http.MethodPost, exportPath(s.one), map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
	// Bob isn't in workspace two.
	if rec := s.export(s.bob.cookie, s.two); rec.Code != http.StatusNotFound {
		t.Errorf("a non-member = %d, want 404", rec.Code)
	} else if bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Error("a non-member got a zip")
	}
	token, _ := s.newToken(s.alice.cookie, s.one, "agent")
	if rec := s.request(http.MethodPost, exportPath(s.one), map[string]any{}, bearer(token)); rec.Code != http.StatusForbidden {
		t.Errorf("an access token = %d, want 403: tokens don't take a person's data out", rec.Code)
	}
	if n := s.scalar(`SELECT count(*) FROM audit_events WHERE action = 'data_export_started'`); n != 0 {
		t.Errorf("%d exports recorded for requests that were all refused", n)
	}
}

// A GET changes nothing, so a link, a cross-site navigation or a prefetch can't start an export, use
// up the person's hour of exports or write to the audit log: only the CSRF-guarded POST does.
func TestExportIsNotStartedByAGet(t *testing.T) {
	s := exportStack(t, nil, nil)
	cross := func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Cookie", "taskto_session="+s.alice.cookie)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rec := s.request(method, exportPath(s.one), nil, cross); (rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound) || bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
			t.Errorf("%s of the start route = %d, want 405 or 404 and no zip", method, rec.Code)
		}
	}
	// Guessing a download address is no use, and starts nothing either.
	if rec := s.request(http.MethodGet, exportPath(s.one)+"/guess", nil, cross); rec.Code != http.StatusNotFound {
		t.Errorf("GET of an unknown address = %d, want 404", rec.Code)
	}
	// The POST is guarded against other sites and needs JSON.
	if rec := s.request(http.MethodPost, exportPath(s.one), map[string]any{}, cross); rec.Code != http.StatusForbidden {
		t.Errorf("a cross-site POST = %d, want 403", rec.Code)
	}
	r := httptest.NewRequest(http.MethodPost, exportPath(s.one), strings.NewReader("x=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameOrigin(s.alice.cookie)(r)
	if rec := httptest.NewRecorder(); func() int { s.h.ServeHTTP(rec, r); return rec.Code }() != http.StatusUnsupportedMediaType {
		t.Errorf("a form POST wasn't refused as the wrong content type")
	}
	if n := s.scalar(`SELECT count(*) FROM audit_events WHERE action = 'data_export_started'`); n != 0 {
		t.Errorf("%d exports recorded for requests that started none", n)
	}
	// None of that used a slot: a real export still works, three times (the hour's limit).
	for i := range 3 {
		if rec := s.export(s.alice.cookie, s.one); rec.Code != http.StatusOK {
			t.Fatalf("export %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
}

// The download address is for one person, one session, one use and a short time.
func TestExportDownloadAddressIsSingleUseAndBound(t *testing.T) {
	s := exportStack(t, nil, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	addr := downloadOf(t, s.startExport(s.alice.cookie, s.one))

	// Another person, with the address, gets nothing, and the address stays good for its owner.
	if rec := s.request(http.MethodGet, addr, nil, sameOrigin(s.bob.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("bob with alice's address = %d, want 404", rec.Code)
	}
	// So does the same person from another login: the address is bound to the session.
	login := s.request(http.MethodPost, "/api/login", map[string]any{"email": "alice@example.com", "password": fakePassword})
	other := sessionCookie(login)
	if other == "" || other == s.alice.cookie {
		t.Fatalf("no second session: %d %s", login.Code, login.Body)
	}
	if rec := s.request(http.MethodGet, addr, nil, sameOrigin(other)); rec.Code != http.StatusNotFound {
		t.Errorf("alice's other session = %d, want 404", rec.Code)
	}
	// A HEAD isn't a download, and doesn't use the address.
	if rec := s.request(http.MethodHead, addr, nil, sameOrigin(s.alice.cookie)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD = %d, want 405", rec.Code)
	}
	rec := s.request(http.MethodGet, addr, nil, sameOrigin(s.alice.cookie))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "zip") {
		t.Fatalf("the owner's download = %d %s", rec.Code, rec.Body)
	}
	unzip(t, rec)
	// Once: a second download of the same address is gone.
	if rec := s.request(http.MethodGet, addr, nil, sameOrigin(s.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("second download = %d, want 404", rec.Code)
	}
	// Another workspace's path doesn't take the address either.
	addr2 := downloadOf(t, s.startExport(s.alice.cookie, s.one))
	elsewhere := strings.Replace(addr2, fmt.Sprintf("/workspaces/%d/", s.one), fmt.Sprintf("/workspaces/%d/", s.two), 1)
	if rec := s.request(http.MethodGet, elsewhere, nil, sameOrigin(s.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("an address under another workspace = %d, want 404", rec.Code)
	}
}

// A person who starts an export and never downloads it doesn't hold their slot for ever.
func TestExportUnusedAddressFreesTheSlot(t *testing.T) {
	s := exportStack(t, nil, func(o *Options) { o.Export = export.Options{PerPerson: -1, TicketTTL: 50 * time.Millisecond} })
	addr := downloadOf(t, s.startExport(s.alice.cookie, s.one))
	if rec := s.startExport(s.alice.cookie, s.one); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"export_running"`) {
		t.Fatalf("a second start while one waits = %d %s, want 409 export_running", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec := s.startExport(s.alice.cookie, s.one); rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot of an address nobody used is never freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := s.request(http.MethodGet, addr, nil, sameOrigin(s.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("an expired address = %d, want 404", rec.Code)
	}
}

func TestExportIsRateLimited(t *testing.T) {
	s := exportStack(t, nil, func(o *Options) { o.Export = export.Options{PerPerson: 2} })
	for i := range 2 {
		if rec := s.export(s.alice.cookie, s.one); rec.Code != http.StatusOK {
			t.Fatalf("export %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := s.export(s.alice.cookie, s.one)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"export_rate_limited"`) || rec.Header().Get("Retry-After") == "" {
		t.Errorf("third export = %d %s %v, want 429 export_rate_limited with Retry-After", rec.Code, rec.Body, rec.Header())
	}
	// A turned away export isn't one that began: only the two that did are on the audit log.
	if n := s.scalar(`SELECT count(*) FROM audit_events WHERE action = 'data_export_started' AND actor_id = ?`, s.alice.id); n != 2 {
		t.Errorf("%d data_export_started events, want 2", n)
	}
	// Bob has his own share.
	if rec := s.export(s.bob.cookie, s.one); rec.Code != http.StatusOK {
		t.Errorf("bob's export = %d, want 200: the limit is per person", rec.Code)
	}
}

// Without the agent runtime a conversation's history can't be read: the export says so, and still
// holds the rest.
func TestExportSaysWhenAHistoryIsMissing(t *testing.T) {
	s := newStack(t) // its conversations have no agent runtime
	cid := s.conversation(s.one, s.alice.id, 1, "Chat")
	s.put(s.alice.cookie, s.one, s.alice.private, "plan.md", "alice-plan")

	files := unzip(t, s.export(s.alice.cookie, s.one))
	if string(files["documents/plan.md"]) != "alice-plan" {
		t.Errorf("documents = %v", names(files))
	}
	var convo struct {
		Transcript string `json:"transcript"`
		Entries    []any  `json:"entries"`
	}
	if err := json.Unmarshal(files[fmt.Sprintf("conversations/%d-Chat.json", cid)], &convo); err != nil || convo.Transcript != "unavailable" || len(convo.Entries) != 0 {
		t.Errorf("conversation = %+v, %v", convo, err)
	}
	var mf struct {
		Skipped []struct{ Kind, Reason, Ref string } `json:"skipped"`
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil || len(mf.Skipped) != 1 || mf.Skipped[0].Kind != "transcript" || mf.Skipped[0].Ref != strconv.FormatInt(cid, 10) {
		t.Errorf("manifest skipped = %+v, %v", mf.Skipped, err)
	}
}

// discard is a response writer that keeps nothing, counting what it is given.
type discard struct {
	h    http.Header
	code int
	n    int64
}

func (d *discard) Header() http.Header { return d.h }
func (d *discard) WriteHeader(c int)   { d.code = c }
func (d *discard) Write(p []byte) (int, error) {
	d.n += int64(len(p))
	return len(p), nil
}

// A large file is copied through, not held: exporting 48 MiB takes a small fraction of that.
func TestExportStreamsLargeFiles(t *testing.T) {
	s := exportStack(t, nil, nil)
	const size = 48 << 20
	chunk := bytes.Repeat([]byte("taskto"), 1<<16) // 384 KiB
	var big bytes.Buffer
	for big.Len() < size {
		big.Write(chunk)
	}
	s.putFile(s.alice.cookie, s.one, s.alice.private, "media/big.mp4", big.Bytes())
	big.Reset()

	r := s.downloadRequest(s.alice.cookie, s.one)
	out := &discard{h: http.Header{}}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s.h.ServeHTTP(out, r)
	runtime.ReadMemStats(&after)
	if out.code != http.StatusOK || out.n < size {
		t.Fatalf("export = %d with %d bytes, want 200 with at least %d", out.code, out.n, size)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > size/3 {
		t.Errorf("exporting a %d MiB file allocated %d MiB: it should be streamed", size>>20, grew>>20)
	}
}

// cancelOnWrite cancels a request's context as soon as the first bytes are written to it, as a person
// closing the download does.
type cancelOnWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (c *cancelOnWrite) Write(p []byte) (int, error) {
	c.cancel()
	return c.ResponseRecorder.Write(p)
}

// A person who goes away stops the export: the zip is left without its end, and the connection is cut.
func TestExportStopsWhenThePersonGoes(t *testing.T) {
	s := exportStack(t, nil, nil)
	rnd := rand.NewChaCha8([32]byte{1})
	for i := range 4 {
		b := make([]byte, 300<<10)
		_, _ = rnd.Read(b) // incompressible, so the zip's first bytes go out early
		s.putFile(s.alice.cookie, s.one, s.alice.private, fmt.Sprintf("media/%d.mp4", i), b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := s.downloadRequest(s.alice.cookie, s.one).WithContext(ctx)
	rec := &cancelOnWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	var got any
	func() {
		defer func() { got = recover() }()
		s.h.ServeHTTP(rec, r)
	}()
	if err, _ := got.(error); !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("a cancelled export ended with %v, want the connection cut (http.ErrAbortHandler)", got)
	}
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
		t.Error("a cancelled export left a whole zip")
	}
	// The slot is free again: the person can export at once.
	if rec := s.export(s.alice.cookie, s.one); rec.Code != http.StatusOK {
		t.Errorf("export after a cancelled one = %d", rec.Code)
	}
}

// stalled is a connection that stops taking bytes: a write waits for the write deadline the server
// set, and fails when it passes. Without a deadline it waits as long as the test lets it.
type stalled struct {
	h        http.Header
	deadline time.Time
	hung     bool
}

func (s *stalled) Header() http.Header { return s.h }
func (s *stalled) WriteHeader(int)     {}
func (s *stalled) SetWriteDeadline(t time.Time) error {
	s.deadline = t
	return nil
}

func (s *stalled) Write([]byte) (int, error) {
	if s.deadline.IsZero() {
		s.hung = true
		time.Sleep(3 * time.Second)
		return 0, errors.New("no deadline was set")
	}
	time.Sleep(time.Until(s.deadline))
	return 0, os.ErrDeadlineExceeded
}

// A download whose connection stalls is ended by the idle deadline, and frees the person's slot.
func TestExportEndsWhenTheConnectionStalls(t *testing.T) {
	s := exportStack(t, nil, func(o *Options) { o.Export = export.Options{IdleTimeout: 150 * time.Millisecond} })
	s.put(s.alice.cookie, s.one, s.alice.private, "plan.md", "alice-plan")
	r := s.downloadRequest(s.alice.cookie, s.one)
	out := &stalled{h: http.Header{}}
	var got any
	func() {
		defer func() { got = recover() }()
		s.h.ServeHTTP(out, r)
	}()
	if out.hung {
		t.Fatal("a write was made with no deadline: a stalled connection would hold the export for ever")
	}
	if err, _ := got.(error); !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("a stalled export ended with %v, want the connection cut", got)
	}
	if rec := s.export(s.alice.cookie, s.one); rec.Code != http.StatusOK {
		t.Errorf("export after a stalled one = %d: the slot wasn't freed", rec.Code)
	}
}
