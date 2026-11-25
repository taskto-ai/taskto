package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/export"
)

func wsExportPath(ws int64) string { return exportPath(ws) + "/workspace" }

// startWorkspaceExport is the POST that starts an admin's export of a workspace.
func (s *stack) startWorkspaceExport(cookie string, ws int64) *http.Request {
	s.t.Helper()
	rec := s.request(http.MethodPost, wsExportPath(ws), map[string]any{}, sameOrigin(cookie))
	r := httptest.NewRequest(http.MethodGet, downloadOf(s.t, rec), nil)
	sameOrigin(cookie)(r)
	return r
}

// workspaceExport starts and downloads the workspace export; what the start refuses comes back as it is.
func (s *stack) workspaceExport(cookie string, ws int64) *httptest.ResponseRecorder {
	s.t.Helper()
	start := s.request(http.MethodPost, wsExportPath(ws), map[string]any{}, sameOrigin(cookie))
	if start.Code != http.StatusOK {
		return start
	}
	return s.request(http.MethodGet, downloadOf(s.t, start), nil, sameOrigin(cookie))
}

// serve runs a request against the stack's handler.
func serve(s *stack, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// workspaceFixture is a workspace whose admin, alice, is a member of Team (with bob) and of Old
// (archived), isn't a member of Secret (carol's), and whose members each have a private space.
type workspaceFixture struct {
	*stack
	old int64
}

func newWorkspaceFixture(t *testing.T, mod func(*Options)) *workspaceFixture {
	t.Helper()
	s := exportStack(t, map[int64][]string{1: {"alice-chat"}, 2: {"bob-chat"}}, mod)
	s.conversation(s.one, s.alice.id, 1, "Alice's chat")
	s.conversation(s.one, s.bob.id, 2, "Bob's chat")

	// Team: documents, a board with a card and a comment, an app with a record.
	s.put(s.bob.cookie, s.one, s.team, "plans/roadmap.md", "team-roadmap")
	code, tb := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", s.one, s.team), map[string]any{"name": "Team board"})
	if code != http.StatusCreated {
		t.Fatalf("team board = %d %v", code, tb)
	}
	s.card(s.bob.cookie, s.one, s.team, id(tb["id"]), "team-card", "team-comment")
	s.app(s.bob.cookie, s.team, "team-expense")

	// Old: a shared space that was archived.
	old := s.sharedSpace("Old", s.bob)
	s.put(s.bob.cookie, s.one, old, "old.md", "old-notes")
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, old), map[string]any{"archived": true}); code != http.StatusOK {
		t.Fatalf("archive = %d %v", code, body)
	}

	// What an admin may not read: a shared space they aren't in, other people's private spaces,
	// and their own private space (it is in their own export, not the workspace's).
	s.put(s.carol.cookie, s.one, s.secret, "hidden.md", "secret-doc")
	s.app(s.carol.cookie, s.secret, "secret-expense")
	s.put(s.bob.cookie, s.one, s.bob.private, "mine.md", "bob-private")
	s.put(s.carol.cookie, s.one, s.carol.private, "mine.md", "carol-private")
	s.put(s.alice.cookie, s.one, s.alice.private, "mine.md", "alice-private")
	s.card(s.bob.cookie, s.one, s.bob.private, s.scalar(`SELECT id FROM boards WHERE space_id = ?`, s.bob.private), "bob-private-card", "bob-private-comment")
	s.app(s.bob.cookie, s.bob.private, "bob-private-expense")
	s.put(s.alice.cookie, s.two, s.alicePrivate2, "two.md", "two-doc")
	return &workspaceFixture{stack: s, old: old}
}

// An admin's export of a workspace holds the shared spaces they are a member of and the member
// list, and nothing that the admin can't read in the app.
func TestWorkspaceExportHoldsOnlyWhatTheAdminCanRead(t *testing.T) {
	f := newWorkspaceFixture(t, nil)
	rec := f.workspaceExport(f.alice.cookie, f.one)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	files := unzip(t, rec)

	var mf struct {
		Format    int            `json:"format"`
		Scope     string         `json:"scope"`
		Counts    map[string]int `json:"counts"`
		Workspace struct{ ID int64 }
		User      struct{ ID int64 }
		Spaces    []struct {
			ID         int64   `json:"id"`
			Name       string  `json:"name"`
			Dir        string  `json:"dir"`
			ArchivedAt *string `json:"archived_at"`
		} `json:"spaces"`
		Skipped []any `json:"skipped"`
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Format != export.Format || mf.Scope != "workspace" || mf.Workspace.ID != f.one || mf.User.ID != f.alice.id || len(mf.Skipped) != 0 {
		t.Errorf("manifest = %+v", mf)
	}
	if len(mf.Spaces) != 2 {
		t.Fatalf("manifest spaces = %+v, want Old and Team", mf.Spaces)
	}
	dirs := map[string]string{}
	for _, sp := range mf.Spaces {
		dirs[sp.Name] = sp.Dir
		if (sp.Name == "Old") != (sp.ArchivedAt != nil) {
			t.Errorf("space %q archived_at = %v: archived spaces are flagged, others aren't", sp.Name, sp.ArchivedAt)
		}
	}
	for name, want := range map[string]string{"Team": fmt.Sprintf("spaces/%d-Team", f.team), "Old": fmt.Sprintf("spaces/%d-Old", f.old)} {
		if dirs[name] != want {
			t.Errorf("space %s is in %q, want %q", name, dirs[name], want)
		}
	}
	for k, v := range map[string]int{"members": 3, "spaces": 2, "documents": 2, "boards": 1, "cards": 1, "comments": 1, "apps": 1, "records": 1} {
		if mf.Counts[k] != v {
			t.Errorf("counts[%s] = %d, want %d (%v)", k, mf.Counts[k], v, mf.Counts)
		}
	}

	team, old := dirs["Team"], dirs["Old"]
	if got := string(files[team+"/documents/plans/roadmap.md"]); got != "team-roadmap" {
		t.Errorf("roadmap = %q (entries %v)", got, names(files))
	}
	if got := string(files[old+"/documents/old.md"]); got != "old-notes" {
		t.Errorf("an archived space's document = %q", got)
	}
	var sf struct {
		Name       string  `json:"name"`
		ArchivedAt *string `json:"archived_at"`
		Role       string  `json:"role"`
	}
	if err := json.Unmarshal(files[old+"/space.json"], &sf); err != nil || sf.ArchivedAt == nil || sf.Role != "editor" {
		t.Errorf("Old's space.json = %s (%v)", files[old+"/space.json"], err)
	}

	var board struct {
		Cards []struct {
			Title    string `json:"title"`
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
		} `json:"cards"`
	}
	var haveBoard, haveMD bool
	for name, b := range files {
		switch {
		case strings.HasPrefix(name, team+"/boards/") && strings.HasSuffix(name, ".json"):
			haveBoard = true
			if err := json.Unmarshal(b, &board); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(name, team+"/boards/") && strings.HasSuffix(name, ".md"):
			haveMD = strings.Contains(string(b), "team-card") && strings.Contains(string(b), "team-comment")
		}
	}
	if !haveBoard || len(board.Cards) != 1 || board.Cards[0].Title != "team-card" || board.Cards[0].Comments[0].Body != "team-comment" {
		t.Errorf("team board = %+v (have %v)", board, haveBoard)
	}
	if !haveMD {
		t.Errorf("the team board's Markdown is missing or lacks the card: %v", names(files))
	}

	var appFile struct {
		Live *struct {
			Definition json.RawMessage `json:"definition"`
		} `json:"live"`
		Rows []map[string]any `json:"records"`
	}
	for name, b := range files {
		if strings.HasPrefix(name, team+"/apps/") {
			if err := json.Unmarshal(b, &appFile); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(appFile.Rows) != 1 || appFile.Rows[0]["description"] != "team-expense" {
		t.Errorf("app records = %v", appFile.Rows)
	}
	if appFile.Live == nil || len(appFile.Live.Definition) < 10 {
		t.Errorf("the app's live definition is missing: %+v", appFile.Live)
	}
	if bytes.Contains(files[team+"/apps/"+firstWithPrefix(files, team+"/apps/")], []byte(`"draft"`)) {
		t.Error("the app's draft is in the export: only the live release is")
	}

	var members struct {
		Members []struct {
			Name, Email, Role string
		} `json:"members"`
	}
	if err := json.Unmarshal(files["members.json"], &members); err != nil || len(members.Members) != 3 {
		t.Fatalf("members.json = %s (%v)", files["members.json"], err)
	}
	roles := map[string]string{}
	for _, m := range members.Members {
		roles[m.Email] = m.Role
	}
	if roles["alice@example.com"] != "admin" || roles["bob@example.com"] != "member" || roles["carol@example.com"] != "member" {
		t.Errorf("members = %v", roles)
	}

	// Not one byte of what the admin can't read in the app, in any entry or any name.
	for name, b := range files {
		for _, hidden := range []string{"secret-doc", "secret-expense", "Secret", "bob-private", "carol-private", "alice-private", "two-doc", "Bob's chat", "Alice's chat", "alice-chat", "bob-chat"} {
			if bytes.Contains(b, []byte(hidden)) || strings.Contains(name, hidden) {
				t.Errorf("%s holds %q, which the admin can't read in the workspace's shared spaces", name, hidden)
			}
		}
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
			t.Errorf("unsafe entry name %q", name)
		}
		if strings.HasPrefix(name, "conversations/") || strings.HasPrefix(name, "documents/") {
			t.Errorf("entry %q: a workspace export has no conversations and no private documents", name)
		}
	}

	// It is on the audit log, as the admin's, as a count and nothing a person typed.
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_events WHERE action = 'workspace_export_started' AND actor_id = ? AND workspace_id = ? AND target_type = 'workspace' AND target_id = ?`,
		f.alice.id, f.one, f.one).Scan(&details); err != nil {
		t.Fatalf("no workspace_export_started event: %v", err)
	}
	if details != `{"spaces":2}` {
		t.Errorf("details = %s, want the number of spaces only", details)
	}
	if n := f.scalar(`SELECT count(*) FROM audit_events WHERE action = 'data_export_started'`); n != 0 {
		t.Errorf("%d personal exports recorded for a workspace export", n)
	}
}

func firstWithPrefix(files map[string][]byte, prefix string) string {
	for name := range files {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return ""
}

// Only an admin of the workspace may export it, with a session.
func TestWorkspaceExportIsForAdminsOnly(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	if rec := f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
	// Bob is a member, not an admin.
	rec := f.workspaceExport(f.bob.cookie, f.one)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a member = %d, want 403", rec.Code)
	}
	if bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Error("a member got a zip")
	}
	// Bob isn't in workspace two at all.
	if rec := f.workspaceExport(f.bob.cookie, f.two); rec.Code != http.StatusNotFound {
		t.Errorf("a non-member = %d, want 404", rec.Code)
	}
	// An access token, even an admin's, doesn't take a workspace out.
	token, _ := f.newToken(f.alice.cookie, f.one, "agent")
	if rec := f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}, bearer(token)); rec.Code != http.StatusForbidden {
		t.Errorf("an access token = %d, want 403", rec.Code)
	}
	// Nothing of that is recorded.
	if n := f.scalar(`SELECT count(*) FROM audit_events WHERE action = 'workspace_export_started'`); n != 0 {
		t.Errorf("%d exports recorded for requests that were all refused", n)
	}

	// An admin's address is no use to a member, even in the same workspace and with their own login.
	r := f.startWorkspaceExport(f.alice.cookie, f.one)
	bobRequest := r.Clone(r.Context())
	bobRequest.Header.Set("Cookie", "taskto_session="+f.bob.cookie)
	if rec := serve(f.stack, bobRequest); rec.Code != http.StatusForbidden || bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Errorf("alice's address used by bob = %d, want 403 and no zip", rec.Code)
	}
	// It stays good for its owner, once.
	if rec := serve(f.stack, r); rec.Code != http.StatusOK || !bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Errorf("the admin's own address = %d, want the zip", rec.Code)
	}
	if rec := serve(f.stack, r); rec.Code != http.StatusNotFound {
		t.Errorf("an address used twice = %d, want 404", rec.Code)
	}
}

// The two exports' addresses aren't interchangeable: a person's own export can't be turned into a
// workspace's, nor the other way.
func TestWorkspaceExportAndPersonalExportTicketsAreSeparate(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	token := func(addr string) string { return addr[strings.LastIndex(addr, "/")+1:] }
	personal := downloadOf(t, f.startExport(f.alice.cookie, f.one))
	if rec := f.request(http.MethodGet, wsExportPath(f.one)+"/"+token(personal), nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("a personal ticket at the workspace download = %d, want 404", rec.Code)
	}
	// The personal ticket is still good for what it was made for, which frees the person's slot.
	if rec := f.request(http.MethodGet, personal, nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusOK {
		t.Fatalf("personal download = %d", rec.Code)
	}
	workspace := downloadOf(t, f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}, sameOrigin(f.alice.cookie)))
	if rec := f.request(http.MethodGet, exportPath(f.one)+"/"+token(workspace), nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("a workspace ticket at the personal download = %d, want 404", rec.Code)
	}
	if rec := f.request(http.MethodGet, workspace, nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusOK {
		t.Errorf("the workspace ticket at its own download = %d", rec.Code)
	}
}

// An admin of one workspace can't export another, and a ticket for one is no use in the other.
func TestWorkspaceExportStaysInItsWorkspace(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	// Alice is an admin of two as well, with a shared space of her own there.
	spaceTwo := f.scalar(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Two shared') RETURNING id`, f.two)
	f.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, spaceTwo, f.alice.id)
	f.put(f.alice.cookie, f.two, spaceTwo, "two-shared.md", "two-shared-doc")

	files := unzip(t, f.workspaceExport(f.alice.cookie, f.two))
	if !bytes.Contains(files[fmt.Sprintf("spaces/%d-Two shared/documents/two-shared.md", spaceTwo)], []byte("two-shared-doc")) {
		t.Errorf("workspace two's export lacks its shared space: %v", names(files))
	}
	for name, b := range files {
		for _, other := range []string{"team-roadmap", "team-card", "old-notes", "Team", "Old", "bob@example.com", "carol@example.com"} {
			if bytes.Contains(b, []byte(other)) || strings.Contains(name, other) {
				t.Errorf("workspace two's export holds %q from workspace one, in %s", other, name)
			}
		}
	}
	// A ticket of workspace two, at workspace one's download address, opens nothing.
	addr := downloadOf(t, f.request(http.MethodPost, wsExportPath(f.two), map[string]any{}, sameOrigin(f.alice.cookie)))
	token := addr[strings.LastIndex(addr, "/")+1:]
	if rec := f.request(http.MethodGet, wsExportPath(f.one)+"/"+token, nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusNotFound {
		t.Errorf("workspace two's ticket in workspace one = %d, want 404", rec.Code)
	}
}

// An admin who is not in a shared space never gets it, and joining it is what puts it in the next export.
func TestWorkspaceExportNeedsMembershipOfASharedSpace(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	// Carol, who isn't an admin, still reads Secret; the admin sees its name and members but not its content.
	for name, b := range unzip(t, f.workspaceExport(f.alice.cookie, f.one)) {
		if bytes.Contains(b, []byte("secret-doc")) || strings.Contains(name, fmt.Sprintf("%d-Secret", f.secret)) {
			t.Errorf("the admin's export has Secret's content in %s", name)
		}
	}
	// The admin joins: the space is theirs to read, so it is in the export, and the join is on the
	// space's audit trail like any other.
	if code, body := f.browser(f.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", f.one, f.secret), map[string]any{"user_id": f.alice.id, "role": "viewer"}); code != http.StatusNoContent {
		t.Fatalf("join = %d %v", code, body)
	}
	files := unzip(t, f.workspaceExport(f.alice.cookie, f.one))
	if got := string(files[fmt.Sprintf("spaces/%d-Secret/documents/hidden.md", f.secret)]); got != "secret-doc" {
		t.Errorf("after joining, Secret's document = %q (%v)", got, names(files))
	}
}

// A space the admin leaves between the start and the download is left out, and the manifest says so.
func TestWorkspaceExportSkipsASpaceTheAdminLeft(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	r := f.startWorkspaceExport(f.alice.cookie, f.one)
	if code, body := f.browser(f.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.one, f.team, f.alice.id), nil); code != http.StatusNoContent {
		t.Fatalf("leave = %d %v", code, body)
	}
	rec := serve(f.stack, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	files := unzip(t, rec)
	for name, b := range files {
		if bytes.Contains(b, []byte("team-")) && name != "manifest.json" {
			t.Errorf("%s holds Team's content though the admin left Team", name)
		}
		if strings.Contains(name, fmt.Sprintf("%d-Team", f.team)) {
			t.Errorf("entry %q is of a space the admin left", name)
		}
	}
	var mf struct {
		Counts  map[string]int `json:"counts"`
		Skipped []struct {
			Kind, Reason string
			Space        int64
		} `json:"skipped"`
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil {
		t.Fatal(err)
	}
	if len(mf.Skipped) != 1 || mf.Skipped[0].Kind != "space" || mf.Skipped[0].Reason != "access_lost" || mf.Skipped[0].Space != f.team {
		t.Errorf("skipped = %+v, want Team left out as access_lost", mf.Skipped)
	}
	if mf.Counts["spaces"] != 1 {
		t.Errorf("counts = %v, want only Old", mf.Counts)
	}
	if got := string(files[fmt.Sprintf("spaces/%d-Old/documents/old.md", f.old)]); got != "old-notes" {
		t.Errorf("the other space's document = %q", got)
	}
}

// An admin demoted before the download gets nothing: the download asks again.
func TestWorkspaceExportEndsWhenTheAdminIsNoLongerOne(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	r := f.startWorkspaceExport(f.alice.cookie, f.one)
	f.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, f.one, f.alice.id)
	if rec := serve(f.stack, r); rec.Code != http.StatusForbidden || bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Errorf("a demoted admin's download = %d, want 403 and no zip", rec.Code)
	}
}

// The workspace export shares the person's one-at-a-time slot and the hour's count with the
// personal one.
func TestWorkspaceExportSharesTheLimits(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: 2} })
	first := f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}, sameOrigin(f.alice.cookie))
	downloadOf(t, first)
	// The first holds the slot until it is downloaded or expires.
	if rec := f.startExport(f.alice.cookie, f.one); rec.Code != http.StatusConflict {
		t.Errorf("a personal start beside a running workspace export = %d, want 409", rec.Code)
	}
	if rec := f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}, sameOrigin(f.alice.cookie)); rec.Code != http.StatusConflict {
		t.Errorf("a second workspace start = %d, want 409", rec.Code)
	}
	if rec := f.request(http.MethodGet, downloadOf(t, first), nil, sameOrigin(f.alice.cookie)); rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if rec := f.export(f.alice.cookie, f.one); rec.Code != http.StatusOK {
		t.Fatalf("second export = %d", rec.Code)
	}
	if rec := f.request(http.MethodPost, wsExportPath(f.one), map[string]any{}, sameOrigin(f.alice.cookie)); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a third in the hour = %d, want 429", rec.Code)
	}
}

// An app the admin has no role in stays out, as it is out of the app list: being a member of the space
// is not a way around an app's own roles.
func TestWorkspaceExportLeavesOutAnAppTheAdminHasNoRoleIn(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	closed := f.app(f.bob.cookie, f.team, "closed-expense")
	if code, body := f.browser(f.bob.cookie, http.MethodPut, fmt.Sprintf("%s/%d/roles", f.apps(f.one, f.team), closed), map[string]any{
		"grants": []map[string]any{{"user_id": f.bob.id, "roles": []string{"builder", "publisher", "user", "data_manager", "reader"}}},
	}); code != http.StatusOK {
		t.Fatalf("restrict the app = %d %v", code, body)
	}
	if code, body := f.browser(f.alice.cookie, http.MethodGet, fmt.Sprintf("%s/%d", f.apps(f.one, f.team), closed), nil); code != http.StatusNotFound {
		t.Fatalf("the admin opens the restricted app = %d %v: the test needs it closed to them", code, body)
	}
	for name, b := range unzip(t, f.workspaceExport(f.alice.cookie, f.one)) {
		if bytes.Contains(b, []byte("closed-expense")) {
			t.Errorf("%s holds a record of an app the admin can't open", name)
		}
	}
}

// onNth runs fn just before the nth statement from now that contains fragment.
func (s *stack) onNth(fragment string, n int, fn func()) {
	count := 0
	var arm func()
	arm = func() {
		count++
		if count == n {
			fn()
			return
		}
		s.hooks.BeforeQuery(fragment, arm)
	}
	s.hooks.BeforeQuery(fragment, arm)
}

// countStatements counts, until the returned stop is called, the statements that contain fragment.
func (s *stack) countStatements(fragment string) (count func() int) {
	n := 0
	var arm func()
	arm = func() {
		n++
		s.hooks.BeforeQuery(fragment, arm)
	}
	s.hooks.BeforeQuery(fragment, arm)
	return func() int { return n }
}

// aborted serves the download and says whether the connection was cut: the zip then has no end.
func aborted(s *stack, r *http.Request) (*httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	var got any
	func() {
		defer func() { got = recover() }()
		s.h.ServeHTTP(rec, r)
	}()
	err, _ := got.(error)
	return rec, errors.Is(err, http.ErrAbortHandler)
}

func (s *stack) demote(user int64) {
	s.t.Helper()
	s.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, s.one, user)
}

// A document is added to the zip only if the admin is still in the space once it is open: the
// membership is checked again after the file was opened, not only before.
func TestWorkspaceExportChecksTheSpaceAgainAfterOpeningADocument(t *testing.T) {
	s := exportStack(t, nil, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	s.put(s.bob.cookie, s.one, s.team, "plans/roadmap.md", "team-roadmap")
	r := s.startWorkspaceExport(s.alice.cookie, s.one)
	// The space is read for its row, its documents' listing and page, and the document's own check
	// (the fourth) before the file is opened: the next check of the admin after it is the one that
	// has to end the document's part in the zip.
	s.onNth("FROM spaces s", 4, func() {
		s.hooks.BeforeQuery("AND role = ?", func() {
			s.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, s.team, s.alice.id)
		})
	})
	rec := serve(s, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	files := unzip(t, rec)
	for name, b := range files {
		if bytes.Contains(b, []byte("team-roadmap")) || strings.HasSuffix(name, "roadmap.md") {
			t.Errorf("%s holds a document opened after the admin left the space", name)
		}
	}
	if !bytes.Contains(files["manifest.json"], []byte("access_lost")) {
		t.Errorf("the manifest doesn't say the space was left out: %s", files["manifest.json"])
	}
}

// An admin demoted while the export runs ends it at the next card or app, not only at the end: the
// export reads no more of a space's content once it knows the exporter is no admin.
func TestWorkspaceExportStopsReadingWhenTheAdminIsDemoted(t *testing.T) {
	for _, tt := range []struct{ name, fragment string }{
		{"cards", "FROM card_comments cc"},
		{"apps", "r.revision, r.data FROM app_rows r"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
			board := f.boardOf(f.team)
			f.card(f.bob.cookie, f.one, f.team, board, "second-card", "second-comment")
			f.card(f.bob.cookie, f.one, f.team, board, "third-card", "third-comment")
			f.app(f.bob.cookie, f.team, "second-expense")
			f.app(f.bob.cookie, f.team, "third-expense")

			r := f.startWorkspaceExport(f.alice.cookie, f.one)
			var after func() int
			// The first read of the content is in flight when the exporter is demoted: nothing is read
			// after it.
			f.onNth(tt.fragment, 1, func() {
				f.demote(f.alice.id)
				after = f.countStatements(tt.fragment)
			})
			rec, cut := aborted(f.stack, r)
			if !cut {
				t.Fatalf("the export of a demoted admin went on to the end (%d bytes)", rec.Body.Len())
			}
			if after == nil || after() != 0 {
				t.Errorf("%d reads of %s after the admin was demoted, want none", after(), tt.name)
			}
			if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
				t.Error("the zip of a demoted admin is whole")
			}
		})
	}
}

// The members are listed once each, in one order, however many pages there are.
func TestWorkspaceExportListsEveryMemberOnceAcrossPages(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	const extra = 503 // more than a page, with names that sort the other way from their IDs
	for i := range extra {
		uid := f.scalar(`INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x') RETURNING id`,
			fmt.Sprintf("member-%d@example.com", i), fmt.Sprintf("Member %04d", extra-i))
		f.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, f.one, uid)
	}
	files := unzip(t, f.workspaceExport(f.alice.cookie, f.one))
	var list struct {
		Members []struct {
			ID int64 `json:"id"`
		} `json:"members"`
	}
	if err := json.Unmarshal(files["members.json"], &list); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, m := range list.Members {
		seen[m.ID] = true
	}
	want := 3 + extra
	if len(list.Members) != want || len(seen) != want {
		t.Errorf("members.json has %d entries, %d different, want %d once each", len(list.Members), len(seen), want)
	}
}

// An app an admin can open only to set its roles right (it has roles and no builder, and none is
// theirs) is not exported: its records are for the people its roles name.
func TestWorkspaceExportLeavesOutAnAppTheAdminSeesOnlyToRecoverIt(t *testing.T) {
	f := newWorkspaceFixture(t, func(o *Options) { o.Export = export.Options{PerPerson: -1} })
	stranded := f.app(f.bob.cookie, f.team, "stranded-expense")
	f.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'data_manager')`, stranded, f.bob.id)
	if code, body := f.browser(f.alice.cookie, http.MethodGet, fmt.Sprintf("%s/%d", f.apps(f.one, f.team), stranded), nil); code != http.StatusOK {
		t.Fatalf("the admin opens the stranded app = %d %v: the test needs the way back open to them", code, body)
	}
	files := unzip(t, f.workspaceExport(f.alice.cookie, f.one))
	for name, b := range files {
		if strings.Contains(name, fmt.Sprintf("/apps/%d-", stranded)) || (bytes.Contains(b, []byte("stranded-expense")) && name != "manifest.json") {
			t.Errorf("%s holds an app the admin can only open to recover it", name)
		}
	}
	// The other app of the space, which they hold roles in, is there.
	found := false
	for name := range files {
		found = found || strings.Contains(name, fmt.Sprintf("/apps/%d-", f.scalar(`SELECT id FROM apps WHERE space_id = ? AND id != ? ORDER BY id LIMIT 1`, f.team, stranded)))
	}
	if !found {
		t.Errorf("the app the admin holds a role in is missing: %v", names(files))
	}
}
