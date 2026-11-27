package server

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/quota"
)

func quotaStack(t *testing.T, limits quota.Limits) *stack {
	t.Helper()
	return newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		q := quota.New(sqlDB, limits, o.Search.Knowledge())
		o.Search.Knowledge().SetQuota(q.KnowledgeCheck())
		o.Quota = q
	})
}

func (s *stack) putClip(cookie string, ws, space int64, name string, n int) (int, map[string]any) {
	s.t.Helper()
	var seed int
	for _, c := range name {
		seed = seed*31 + int(c)
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = byte((i*131 + i>>8 + seed) & 0xff)
	}
	rec := s.request(http.MethodPut, s.docs(ws, space)+"/"+name, nil, sameOrigin(cookie), func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(n)
		r.Header.Set("Content-Type", "application/octet-stream")
	})
	return rec.Code, decode(rec)
}

func (s *stack) usage(cookie string, ws int64, tail string) (int, map[string]any) {
	s.t.Helper()
	return s.browser(cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/usage%s", ws, tail), nil)
}

func spaceUsage(t *testing.T, report map[string]any, id int64) map[string]any {
	t.Helper()
	list, _ := report["spaces"].([]any)
	for _, v := range list {
		if sp := v.(map[string]any); int64(sp["id"].(float64)) == id {
			return sp
		}
	}
	return nil
}

func TestWritesOverAPrivateSpaceQuotaAreRefusedAndFreedBytesCanBeUsedAgain(t *testing.T) {
	s := quotaStack(t, quota.Limits{User: 100 << 10})

	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "a.mp4", 60<<10); code != http.StatusCreated {
		t.Fatalf("first clip = %d %v", code, body)
	}
	code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "b.mp4", 60<<10)
	if code != http.StatusInsufficientStorage || body["code"] != "quota_user" || body["params"].(map[string]any)["max_mib"] != float64(1) {
		t.Fatalf("second clip = %d %v, want 507 quota_user", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPut, s.docs(s.one, s.bob.private)+"/note.md", map[string]any{"content": "# Note\n"}); code != http.StatusCreated {
		t.Fatalf("a note = %d %v", code, body)
	}
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "c.mp4", 200<<10); code != http.StatusInsufficientStorage || body["code"] != "quota_user" {
		t.Fatalf("a clip over the whole quota = %d %v", code, body)
	}

	if code, body := s.browser(s.bob.cookie, http.MethodDelete, s.docs(s.one, s.bob.private)+"/a.mp4", nil); code != http.StatusOK {
		t.Fatalf("delete = %d %v", code, body)
	}
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "b.mp4", 60<<10); code != http.StatusCreated {
		t.Fatalf("the clip after making room = %d %v", code, body)
	}

	if code, body := s.putClip(s.carol.cookie, s.one, s.secret, "big.mp4", 300<<10); code != http.StatusCreated {
		t.Fatalf("a shared space = %d %v", code, body)
	}
	if code, body := s.putClip(s.alice.cookie, s.one, s.alice.private, "mine.mp4", 90<<10); code != http.StatusCreated {
		t.Fatalf("alice's private space = %d %v", code, body)
	}
}

func TestWritesOverTheWorkspaceQuotaAreRefusedInEverySpaceOfIt(t *testing.T) {
	s := quotaStack(t, quota.Limits{Workspace: 200 << 10})

	for i, w := range []struct {
		who   person
		space int64
	}{{s.carol, s.secret}, {s.bob, s.bob.private}, {s.alice, s.alice.private}} {
		if code, body := s.putClip(w.who.cookie, s.one, w.space, "x.mp4", 60<<10); code != http.StatusCreated {
			t.Fatalf("clip %d = %d %v", i, code, body)
		}
	}
	for _, w := range []struct {
		who   person
		space int64
	}{{s.carol, s.secret}, {s.bob, s.team}, {s.alice, s.alice.private}} {
		code, body := s.putClip(w.who.cookie, s.one, w.space, "y.mp4", 60<<10)
		if code != http.StatusInsufficientStorage || body["code"] != "quota_workspace" {
			t.Fatalf("clip in space %d = %d %v, want 507 quota_workspace", w.space, code, body)
		}
	}
	if code, body := s.putClip(s.alice.cookie, s.two, s.alicePrivate2, "z.mp4", 150<<10); code != http.StatusCreated {
		t.Fatalf("workspace two = %d %v", code, body)
	}
}

func TestNoQuotaMeansNoLimit(t *testing.T) {
	s := quotaStack(t, quota.Limits{})
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "a.mp4", 300<<10); code != http.StatusCreated {
		t.Fatalf("a clip = %d %v", code, body)
	}
	code, rep := s.usage(s.alice.cookie, s.one, "")
	if code != http.StatusOK || rep["quota"].(map[string]any)["workspace"] != float64(0) {
		t.Fatalf("usage = %d %v", code, rep)
	}
}

func TestUsageViewShowsEverySpaceToAdminsOnly(t *testing.T) {
	s := quotaStack(t, quota.Limits{User: 1 << 20, Workspace: 10 << 20})
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "a.mp4", 50<<10); code != http.StatusCreated {
		t.Fatalf("clip = %d %v", code, body)
	}
	if code, body := s.putClip(s.carol.cookie, s.one, s.secret, "b.mp4", 20<<10); code != http.StatusCreated {
		t.Fatalf("clip = %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, s.docs(s.one, s.team)+"/plan.md", map[string]any{"content": "# Plan\n"}); code != http.StatusCreated {
		t.Fatalf("doc = %d %v", code, body)
	}

	code, rep := s.usage(s.alice.cookie, s.one, "")
	if code != http.StatusOK {
		t.Fatalf("admin = %d %v", code, rep)
	}
	if q := rep["quota"].(map[string]any); q["workspace"] != float64(10<<20) || q["user"] != float64(1<<20) {
		t.Fatalf("quota = %v", q)
	}
	bobs := spaceUsage(t, rep, s.bob.private)
	if bobs == nil || bobs["kind"] != "private" || bobs["knowledge_unversioned"] != float64(50<<10) || bobs["limit"] != float64(1<<20) ||
		bobs["total"].(float64) < float64(50<<10) || bobs["owner"].(map[string]any)["email"] != "bob@example.com" {
		t.Fatalf("bob's private space = %v", bobs)
	}
	secret := spaceUsage(t, rep, s.secret)
	if secret == nil || secret["kind"] != "shared" || secret["knowledge_unversioned"] != float64(20<<10) || secret["limit"] != float64(0) || secret["owner"] != nil {
		t.Fatalf("the shared space = %v", secret)
	}
	if team := spaceUsage(t, rep, s.team); team == nil || team["knowledge_versioned"].(float64) <= 0 || team["knowledge_unversioned"] != float64(0) {
		t.Fatalf("the team space = %v", team)
	}
	var sum float64
	for _, v := range rep["spaces"].([]any) {
		sum += v.(map[string]any)["total"].(float64)
	}
	if rep["total"] != sum || sum < 70<<10 {
		t.Fatalf("total = %v, the spaces' sum is %v", rep["total"], sum)
	}
	if spaceUsage(t, rep, s.alicePrivate2) != nil {
		t.Fatalf("workspace two's space is in workspace one's report: %v", rep)
	}
}

func TestMembersNeverSeeAnothersUsage(t *testing.T) {
	s := quotaStack(t, quota.Limits{User: 1 << 20})
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "a.mp4", 50<<10); code != http.StatusCreated {
		t.Fatalf("clip = %d %v", code, body)
	}

	if code, body := s.usage(s.carol.cookie, s.one, ""); code != http.StatusForbidden || body["code"] != "admin_only" || body["spaces"] != nil {
		t.Fatalf("a member's view of the report = %d %v", code, body)
	}
	code, mine := s.usage(s.carol.cookie, s.one, "/me")
	space, _ := mine["space"].(map[string]any)
	if code != http.StatusOK || space == nil || int64(space["id"].(float64)) != s.carol.private || space["total"] != float64(0) {
		t.Fatalf("carol's own = %d %v", code, mine)
	}
	if code, mine = s.usage(s.bob.cookie, s.one, "/me"); code != http.StatusOK ||
		int64(mine["space"].(map[string]any)["id"].(float64)) != s.bob.private || mine["space"].(map[string]any)["knowledge_unversioned"] != float64(50<<10) {
		t.Fatalf("bob's own = %d %v", code, mine)
	}
	if code, body := s.usage(s.bob.cookie, s.two, ""); code != http.StatusNotFound || body["spaces"] != nil {
		t.Fatalf("a stranger's view of workspace two = %d %v", code, body)
	}
	if code, body := s.usage(s.bob.cookie, s.two, "/me"); code != http.StatusNotFound || body["space"] != nil {
		t.Fatalf("a stranger's own in workspace two = %d %v", code, body)
	}
	if code, _ := s.usage("", s.one, ""); code != http.StatusUnauthorized {
		t.Fatalf("signed out = %d", code)
	}
	token, _ := s.newToken(s.alice.cookie, s.one, "usage")
	for _, tail := range []string{"", "/me"} {
		if code, body := s.api(token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/usage%s", s.one, tail), nil); code == http.StatusOK {
			t.Fatalf("a token read /usage%s: %v", tail, body)
		}
	}
}

func TestAnAdminSeesOnlyTheirWorkspacesUsage(t *testing.T) {
	s := quotaStack(t, quota.Limits{})
	if code, body := s.putClip(s.alice.cookie, s.two, s.alicePrivate2, "two.mp4", 30<<10); code != http.StatusCreated {
		t.Fatalf("clip = %d %v", code, body)
	}
	if code, body := s.putClip(s.bob.cookie, s.one, s.bob.private, "one.mp4", 40<<10); code != http.StatusCreated {
		t.Fatalf("clip = %d %v", code, body)
	}
	_, one := s.usage(s.alice.cookie, s.one, "")
	_, two := s.usage(s.alice.cookie, s.two, "")
	if spaceUsage(t, one, s.bob.private) == nil || spaceUsage(t, one, s.alicePrivate2) != nil {
		t.Fatalf("workspace one's report = %v", one)
	}
	if spaceUsage(t, two, s.alicePrivate2) == nil || spaceUsage(t, two, s.bob.private) != nil || len(two["spaces"].([]any)) != 1 {
		t.Fatalf("workspace two's report = %v", two)
	}
	s.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, s.two, s.carol.id)
	if code, _ := s.usage(s.carol.cookie, s.one, ""); code != http.StatusForbidden {
		t.Fatalf("an admin of workspace two reading one's = %d, want 403 (a member there)", code)
	}
	code, theirs := s.usage(s.carol.cookie, s.two, "")
	if code != http.StatusOK || spaceUsage(t, theirs, s.bob.private) != nil {
		t.Fatalf("carol's report of two = %d %v", code, theirs)
	}
}

func TestTheAgentsWriteToolIsRefusedOverQuota(t *testing.T) {
	s := quotaStack(t, quota.Limits{User: 2 << 10})
	token, _ := s.newToken(s.bob.cookie, s.one, "agent")
	var text bytes.Buffer
	for i := range 4000 {
		fmt.Fprintf(&text, "%x", uint32(i)*2654435761)
	}
	code, refused, out := s.tool(token, "write_doc", map[string]any{"space_id": s.bob.private, "path": "big.md", "content": text.String()})
	if code != http.StatusOK || !refused {
		t.Fatalf("write_doc over the quota = %d refused=%v %s", code, refused, out)
	}
	s.toolOK(token, "write_doc", map[string]any{"space_id": s.bob.private, "path": "small.md", "content": "# ok\n"})
}

type slowFiles struct {
	quota.Files
	hold func()
}

func (f slowFiles) Usage(sp access.Space) (int64, int64, error) {
	v, u, err := f.Files.Usage(sp)
	if f.hold != nil {
		f.hold()
	}
	return v, u, err
}

func TestWritesInDifferentSpacesCannotBothFitTheWorkspaceQuota(t *testing.T) {
	var gate struct {
		sync.Mutex
		arrived int
		both    chan struct{}
	}
	gate.both = make(chan struct{})
	meet := func() {
		gate.Lock()
		gate.arrived++
		mine := gate.arrived
		if mine == 2 {
			close(gate.both)
		}
		gate.Unlock()
		if mine <= 2 {
			select {
			case <-gate.both:
			case <-time.After(2 * time.Second):
			}
		}
	}
	s := newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		files := slowFiles{Files: o.Search.Knowledge(), hold: meet}
		q := quota.New(sqlDB, quota.Limits{Workspace: 100 << 10}, files)
		o.Search.Knowledge().SetQuota(q.KnowledgeCheck())
		o.Quota = q
	})

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, w := range []struct {
		who   person
		space int64
	}{{s.carol, s.secret}, {s.bob, s.team}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = s.putClip(w.who.cookie, s.one, w.space, "x.mp4", 60<<10)
		}()
	}
	wg.Wait()
	created, full := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusInsufficientStorage:
			full++
		}
	}
	if created != 1 || full != 1 {
		t.Fatalf("answers = %v, want one 201 and one 507", codes)
	}
	_, rep := s.usage(s.alice.cookie, s.one, "")
	if total := rep["total"].(float64); total > 100<<10 {
		t.Fatalf("the workspace stores %v bytes, over its 100 KiB quota", total)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPut, s.docs(s.one, s.team)+"/note.md", map[string]any{"content": "# Note\n"}); code != http.StatusCreated {
		t.Fatalf("a note = %d %v", code, body)
	}
}
