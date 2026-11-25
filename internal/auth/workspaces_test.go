package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

// createdWorkspace is the reply of POST /api/workspaces.
type createdWorkspace struct {
	Workspace Membership `json:"workspace"`
	me
}

func errorCode(t *testing.T, e *env, c *client, method, path string, body any, status int) string {
	t.Helper()
	rec := c.do(method, path, body)
	expect(t, rec, status)
	return decode[struct{ Code string }](t, rec).Code
}

func TestCreateWorkspaceMakesTheCallerItsFirstAdmin(t *testing.T) {
	e := newEnv(t, Options{})
	owner, home := e.setup()
	ownerID := e.userID(owner)

	rec := owner.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "  Side project  "})
	expect(t, rec, http.StatusCreated)
	got := decode[createdWorkspace](t, rec)
	if got.Workspace.WorkspaceName != "Side project" || got.Workspace.Role != RoleAdmin || got.Workspace.WorkspaceID == home {
		t.Fatalf("workspace = %+v", got.Workspace)
	}
	// The reply is /api/me with the new workspace in it, oldest first.
	if len(got.Workspaces) != 2 || got.Workspaces[0].WorkspaceID != home || got.Workspaces[1] != got.Workspace {
		t.Fatalf("workspaces = %+v", got.Workspaces)
	}
	if again := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)); len(again.Workspaces) != 2 {
		t.Fatalf("GET /api/me = %+v", again)
	}

	// What setup makes for a workspace, and nothing more: one membership, the caller's, as
	// admin; their private space; no invites, tokens or shared spaces; no other member.
	ws := got.Workspace.WorkspaceID
	if n := e.count(`SELECT count(*) FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'admin'`, ws, ownerID); n != 1 {
		t.Errorf("owner's admin memberships = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM memberships WHERE workspace_id = ?`, ws); n != 1 {
		t.Errorf("members = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM spaces WHERE workspace_id = ? AND kind = 'private' AND owner_id = ?`, ws, ownerID); n != 1 {
		t.Errorf("private spaces of the creator = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM spaces WHERE workspace_id = ?`, ws); n != 1 {
		t.Errorf("spaces = %d, want only the private one", n)
	}
	if n := e.count(`SELECT count(*) FROM invites WHERE workspace_id = ?`, ws) + e.count(`SELECT count(*) FROM access_tokens WHERE workspace_id = ?`, ws); n != 0 {
		t.Errorf("a new workspace has %d invites and tokens", n)
	}
	// The first workspace is untouched.
	if n := e.count(`SELECT count(*) FROM memberships WHERE workspace_id = ?`, home); n != 1 {
		t.Errorf("the first workspace has %d members", n)
	}

	// The audit event belongs to the new workspace, and says what let the caller in.
	ev := e.one(audit.WorkspaceCreated)
	wantEvent(t, ev, ws, ownerID, audit.TargetWorkspace, ws)
	if ev.Details["allowed_by"] != "admins" {
		t.Errorf("details = %v", ev.Details)
	}
	if n := len(e.events(audit.SetupCompleted)); n != 1 {
		t.Errorf("setup_completed events = %d, want 1", n)
	}
}

func TestWhoMayCreateAWorkspace(t *testing.T) {
	for name, tt := range map[string]struct {
		mode string
		// admin and member are whether the workspace's admin and a plain member may.
		admin, member bool
	}{
		"default":        {"", true, false},
		"admins":         {CreateByAdmins, true, false},
		"anyone":         {CreateByAnyone, true, true},
		"off":            {CreateByNobody, false, false},
		"an unknown one": {"everyone", true, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Options{WorkspaceCreation: tt.mode})
			owner, home := e.setup()
			member, _ := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")
			for who, c := range map[string]*client{"admin": owner, "member": member} {
				want := tt.admin
				if who == "member" {
					want = tt.member
				}
				// GET /api/me tells the web app whether to offer the action.
				if got := decode[me](t, c.do(http.MethodGet, "/api/me", nil)).CanCreateWorkspace; got != want {
					t.Errorf("%s: can_create_workspace = %v, want %v", who, got, want)
				}
				before := e.count(`SELECT count(*) FROM workspaces`)
				rec := c.do(http.MethodPost, "/api/workspaces", map[string]string{"name": who + "'s"})
				if want {
					expect(t, rec, http.StatusCreated)
					if n := e.count(`SELECT count(*) FROM workspaces`); n != before+1 {
						t.Errorf("%s: workspaces = %d, want %d", who, n, before+1)
					}
					continue
				}
				expect(t, rec, http.StatusForbidden)
				if code := decode[struct{ Code string }](t, rec).Code; code != string(apierr.CodeWorkspaceCreation) {
					t.Errorf("%s: code = %q", who, code)
				}
				if n := e.count(`SELECT count(*) FROM workspaces`); n != before {
					t.Errorf("%s: a refused call made a workspace (%d, was %d)", who, n, before)
				}
			}
		})
	}
}

// Under "admins", being an admin of any workspace is enough, however the person got there,
// and being only a member everywhere is not: so creating a workspace lets someone create
// another, and an invited member who is admin of nothing can't.
func TestCreatingAWorkspaceFollowsAdminship(t *testing.T) {
	e := newEnv(t, Options{})
	owner, home := e.setup()
	member, mi := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")
	expect(t, member.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Nope"}), http.StatusForbidden)

	// Promoted in the home workspace: now allowed.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/members/%d", home, mi.User.ID), map[string]string{"role": "admin"}), http.StatusNoContent)
	expect(t, member.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Yes"}), http.StatusCreated)
	// Demoted again, and the workspace they made is theirs: still an admin there.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/members/%d", home, mi.User.ID), map[string]string{"role": "member"}), http.StatusNoContent)
	expect(t, member.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Again"}), http.StatusCreated)
}

func TestCreateWorkspaceValidatesTheName(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	before := e.count(`SELECT count(*) FROM workspaces`)
	for name, body := range map[string]any{
		"empty":      map[string]string{"name": ""},
		"blank":      map[string]string{"name": " \t "},
		"missing":    map[string]string{},
		"too long":   map[string]string{"name": strings.Repeat("a", maxNameLen+1)},
		"not an obj": []string{"x"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := owner.do(http.MethodPost, "/api/workspaces", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
		})
	}
	if code := errorCode(t, e, owner, http.MethodPost, "/api/workspaces", map[string]string{"name": ""}, http.StatusBadRequest); code != string(apierr.CodeWorkspaceNameLength) {
		t.Errorf("code = %q", code)
	}
	if n := e.count(`SELECT count(*) FROM workspaces`); n != before {
		t.Errorf("a refused name made %d workspaces", n-before)
	}
	// The longest name is fine, and two workspaces may share a name.
	expect(t, owner.do(http.MethodPost, "/api/workspaces", map[string]string{"name": strings.Repeat("a", maxNameLen)}), http.StatusCreated)
	expect(t, owner.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Home"}), http.StatusCreated)
}

func TestCreateWorkspaceIsForALoggedInBrowser(t *testing.T) {
	e := newEnv(t, Options{WorkspaceCreation: CreateByAnyone})
	owner, home := e.setup()
	body := map[string]string{"name": "X"}

	expect(t, e.client().do(http.MethodPost, "/api/workspaces", body), http.StatusUnauthorized)

	// An access token acts in one workspace: it can't make another.
	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", home), map[string]string{"name": "agent"})
	expect(t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](t, rec).Token
	r := e.client().req(http.MethodPost, "/api/workspaces", body)
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Set("Authorization", "Bearer "+token)
	rec = e.client().send(r)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token made a workspace: %d %s", rec.Code, rec.Body)
	}

	// The CSRF guard covers it like any other state change.
	cross := owner.req(http.MethodPost, "/api/workspaces", body)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := owner.send(cross); rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-site request = %d, want 403", rec.Code)
	}
	if n := e.count(`SELECT count(*) FROM workspaces`); n != 1 {
		t.Errorf("workspaces = %d, want 1", n)
	}
}

func TestCreateWorkspaceIsIsolatedFromTheOthers(t *testing.T) {
	e := newEnv(t, Options{WorkspaceCreation: CreateByAnyone})
	owner, home := e.setup()
	member, mi := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")

	// The member makes a workspace of their own: the owner is no part of it, and the member is
	// no more than a member of the owner's.
	ws := decode[createdWorkspace](t, member.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Mine"})).Workspace.WorkspaceID
	mine := fmt.Sprintf("/api/workspaces/%d", ws)
	theirs := fmt.Sprintf("/api/workspaces/%d", home)

	for _, p := range []string{"/members", "/invites", "/tokens"} {
		expect(t, owner.do(http.MethodGet, mine+p, nil), http.StatusNotFound)
	}
	expect(t, owner.do(http.MethodPost, mine+"/invites", map[string]string{"email": "x@example.com"}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPatch, mine, map[string]string{"locale": "pt-BR"}), http.StatusNotFound)
	expect(t, member.do(http.MethodGet, theirs+"/invites", nil), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, theirs, map[string]string{"locale": "pt-BR"}), http.StatusForbidden)

	// Each roster has the right people.
	owners := decode[struct{ Members []Member }](t, owner.do(http.MethodGet, theirs+"/members", nil)).Members
	mines := decode[struct{ Members []Member }](t, member.do(http.MethodGet, mine+"/members", nil)).Members
	if len(owners) != 2 || len(mines) != 1 || mines[0].ID != mi.User.ID || mines[0].Role != RoleAdmin {
		t.Fatalf("rosters: home %+v, new %+v", owners, mines)
	}
	// Each one's /api/me lists the workspaces they are in, with their role in each.
	if got := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).Workspaces; len(got) != 1 || got[0].WorkspaceID != home {
		t.Errorf("owner's workspaces = %+v", got)
	}
	got := decode[me](t, member.do(http.MethodGet, "/api/me", nil)).Workspaces
	if len(got) != 2 || got[0].Role != RoleMember || got[1].Role != RoleAdmin {
		t.Errorf("member's workspaces = %+v", got)
	}

	// A token is bound to its workspace, also when its user has more than one.
	rec := member.do(http.MethodPost, theirs+"/tokens", map[string]string{"name": "agent"})
	expect(t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](t, rec).Token
	r := e.client().req(http.MethodGet, mine+"/members", nil)
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Set("Authorization", "Bearer "+token)
	if rec := e.client().send(r); rec.Code != http.StatusNotFound {
		t.Errorf("a token of the home workspace reads the new one: %d %s", rec.Code, rec.Body)
	}

	// Removing the member from the owner's workspace leaves their own.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("%s/members/%d", theirs, mi.User.ID), nil), http.StatusNoContent)
	expect(t, member.do(http.MethodGet, mine+"/members", nil), http.StatusOK)
	// The last admin of their own can't leave it either.
	expect(t, member.do(http.MethodDelete, fmt.Sprintf("%s/members/%d", mine, mi.User.ID), nil), http.StatusConflict)
}

// create makes a workspace as c, with the name the test gives, and returns the response.
func createAs(c *client, name string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "/api/workspaces", map[string]string{"name": name})
}

// With -workspace-creation=anyone a single account can't make workspaces without end: past a
// fixed number, whatever the time between them, it is refused (403 workspace_limit), and
// others are unaffected.
func TestAnAccountCreatesAtMostSomeWorkspaces(t *testing.T) {
	e := newEnv(t, Options{WorkspaceCreation: CreateByAnyone})
	owner, home := e.setup()
	member, _ := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")
	for i := 0; i < maxWorkspacesPerUser; i++ {
		expect(t, createAs(member, fmt.Sprintf("W%d", i)), http.StatusCreated)
		e.advance(workspaceWindow) // slowly enough that the rate limit never applies
	}
	before := e.count(`SELECT count(*) FROM workspaces`)
	rec := createAs(member, "One too many")
	expect(t, rec, http.StatusForbidden)
	got := decode[struct {
		Code   string
		Params map[string]any
	}](t, rec)
	if got.Code != string(apierr.CodeWorkspaceLimit) || got.Params["max"] != float64(maxWorkspacesPerUser) {
		t.Errorf("answer = %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM workspaces`); n != before {
		t.Errorf("a refused creation made %d workspaces", n-before)
	}
	if me := decode[me](t, member.do(http.MethodGet, "/api/me", nil)); !me.CanCreateWorkspace {
		// can_create_workspace is about the setting; the limit answers on the call.
		t.Errorf("can_create_workspace = false under anyone")
	}
	// Somebody else is not counted against them, nor is the cap shared.
	expect(t, createAs(owner, "Owner's own"), http.StatusCreated)
}

// A burst of creations is slowed down (429 too_many_workspaces with Retry-After), and
// allowed again as the oldest leaves the hour.
func TestCreatingWorkspacesIsRateLimitedPerAccount(t *testing.T) {
	e := newEnv(t, Options{WorkspaceCreation: CreateByAnyone})
	owner, home := e.setup()
	member, _ := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")
	other, _ := e.join(e.invite(owner, home, "other@example.com", RoleMember), "Other")
	for i := 0; i < workspaceBurst; i++ {
		expect(t, createAs(member, fmt.Sprintf("W%d", i)), http.StatusCreated)
		e.advance(time.Minute)
	}
	rec := createAs(member, "Too soon")
	expect(t, rec, http.StatusTooManyRequests)
	if code := decode[struct{ Code string }](t, rec).Code; code != string(apierr.CodeTooManyWorkspaces) {
		t.Errorf("code = %q", code)
	}
	// The first of the five was 5 minutes ago: it leaves the window in 55 minutes.
	if got := rec.Header().Get("Retry-After"); got != "3300" {
		t.Errorf("Retry-After = %q, want 3300", got)
	}
	expect(t, createAs(other, "Not limited by member"), http.StatusCreated)
	e.advance(54 * time.Minute)
	expect(t, createAs(member, "Still too soon"), http.StatusTooManyRequests)
	e.advance(2 * time.Minute)
	expect(t, createAs(member, "Now fine"), http.StatusCreated)
}

// The server's own workspace is the one setup made, and its admins are the operators:
// not an admin of a workspace they or others made later.
func TestOnlyAdminsOfTheSetupWorkspaceAreOperators(t *testing.T) {
	e := newEnv(t, Options{WorkspaceCreation: CreateByAnyone})
	owner, home := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, home, "member@example.com", RoleMember), "Member")
	admin, _ := e.join(e.invite(owner, home, "admin@example.com", RoleAdmin), "Admin")
	isOp := func(c *client) bool { return decode[me](t, c.do(http.MethodGet, "/api/me", nil)).IsOperator }
	if !isOp(owner) || !isOp(admin) || isOp(member) {
		t.Fatalf("operators: owner %v, admin %v, member %v", isOp(owner), isOp(admin), isOp(member))
	}
	// Making a workspace of their own makes someone its admin, not an operator.
	expect(t, createAs(member, "Mine"), http.StatusCreated)
	if isOp(member) {
		t.Fatal("a member who made a workspace is an operator")
	}
	// The setup workspace is marked, and only it; a created one carries its creator.
	if n := e.count(`SELECT count(*) FROM workspaces WHERE is_setup = 1 AND id = ? AND created_by = ?`, home, ownerID); n != 1 {
		t.Errorf("setup workspace marked %d times", n)
	}
	expect(t, createAs(owner, "Another"), http.StatusCreated)
	if n := e.count(`SELECT count(*) FROM workspaces WHERE is_setup = 1`); n != 1 {
		t.Errorf("%d setup workspaces", n)
	}
	// Demoted from the setup workspace, someone is no operator.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/members/%d", home, e.userID(admin)),
		map[string]string{"role": "member"}), http.StatusNoContent)
	if isOp(admin) {
		t.Fatal("a demoted admin is still an operator")
	}
}

// A name that could hide or reorder what a reader sees is refused, in a workspace's name and
// in setup's.
func TestNamesRefuseControlAndBidiCharacters(t *testing.T) {
	bad := map[string]string{
		"NUL":                    "Ho\x00me",
		"newline":                "Ho\nme",
		"tab":                    "Ho\tme",
		"escape":                 "Ho\x1bme",
		"DEL":                    "Ho\x7fme",
		"C1 control":             "Ho\u0085me",
		"line separator":         "Ho\u2028me",
		"paragraph separator":    "Ho\u2029me",
		"right-to-left override": "Ho\u202eme",
		"left-to-right embed":    "Ho\u202ame",
		"pop directional":        "Ho\u202cme",
		"left-to-right isolate":  "Ho\u2066me",
		"pop isolate":            "Ho\u2069me",
	}
	for name, s := range bad {
		if validName(s) {
			t.Errorf("validName(%s) = true", name)
		}
	}
	for _, s := range []string{"Home", "Ação — projeto", "日本語の名前", "O'Brien & Sons", "a b", "مرحبا"} {
		if !validName(s) {
			t.Errorf("validName(%q) = false", s)
		}
	}
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	before := e.count(`SELECT count(*) FROM workspaces`)
	for name, s := range bad {
		rec := createAs(owner, s)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: creating a workspace = %d, want 400", name, rec.Code)
		}
	}
	if n := e.count(`SELECT count(*) FROM workspaces`); n != before {
		t.Errorf("%d workspaces with a bad name were made", n-before)
	}

	// Setup, for the owner's name and the first workspace's.
	fresh := newEnv(t, Options{})
	for field, s := range map[string]string{"name": "Ow\u202ener", "workspace_name": "Ho\x00me"} {
		body := map[string]string{"email": "owner@example.com", "name": "Owner", "password": ownerPassword,
			"workspace_name": "Home", "setup_token": testSetupToken}
		body[field] = s
		expect(t, fresh.client().do(http.MethodPost, "/api/setup", body), http.StatusBadRequest)
	}
	if n := fresh.count(`SELECT count(*) FROM users`); n != 0 {
		t.Errorf("setup with a bad name made %d users", n)
	}
}
