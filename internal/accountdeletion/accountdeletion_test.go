package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/schedule"
)

const (
	leaverEmail = "purged.person@example.test"
	leaverName  = "Purged Person"
)

type fakeAgent struct {
	mu      sync.Mutex
	retired []int64
	gone    map[int64]bool // conversations the runtime no longer has: 404
}

func (f *fakeAgent) Conversations(context.Context) ([]agentrt.ConversationInfo, error) {
	return nil, nil
}

func (f *fakeAgent) RetireConversation(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[id] {
		return &agentrt.StatusError{Code: http.StatusNotFound}
	}
	f.retired = append(f.retired, id)
	return nil
}

type fakeCLI struct {
	mu    sync.Mutex
	users []int64
	fail  error
}

func (f *fakeCLI) Forget(_ context.Context, user int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.users = append(f.users, user)
	return nil
}

// world: a workspace with an admin (bob), a member who leaves (leaver), and a bystander (carol),
// a shared space everyone writes in, and the leaver's private space with a document in it.
type world struct {
	t        *testing.T
	db       *sql.DB
	dir      string
	kb       *knowledge.Store
	agent    *fakeAgent
	cli      *fakeCLI
	svc      *Service
	sched    *schedule.Service
	ws       int64
	leaver   int64
	bob      int64
	carol    int64
	team     access.Space
	private  access.Space
	otherPrv access.Space // carol's private space, which must survive
	card     int64
	comment  int64
}

func exec(t *testing.T, sqlDB *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := sqlDB.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, db: sqlDB, dir: dir, agent: &fakeAgent{gone: map[int64]bool{}}, cli: &fakeCLI{}}
	w.kb = knowledge.New(dir, knowledge.Options{})
	user := func(email, name string) int64 {
		return exec(t, sqlDB, `INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x')`, email, name)
	}
	w.leaver, w.bob, w.carol = user(leaverEmail, leaverName), user("bob@example.com", "Bob"), user("carol@example.com", "Carol")
	w.ws = exec(t, sqlDB, `INSERT INTO workspaces (name, is_setup) VALUES ('Home', 1)`)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, w.ws, w.bob)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, w.leaver)
	exec(t, sqlDB, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, w.carol)
	teamID := exec(t, sqlDB, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Team')`, w.ws)
	for _, u := range []int64{w.leaver, w.bob, w.carol} {
		exec(t, sqlDB, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, teamID, u)
	}
	space := func(id int64) access.Space { return access.Space{ID: id, WorkspaceID: w.ws, Access: access.Write} }
	w.team = space(teamID)
	private := func(owner int64) access.Space {
		var id int64
		if err := sqlDB.QueryRow(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, w.ws, owner).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return space(id)
	}
	w.private, w.otherPrv = private(w.leaver), private(w.carol)

	put := func(sp access.Space, name, text, who, email string) {
		t.Helper()
		base := ""
		if cur, err := w.kb.Read(ctx, sp, name, ""); err == nil {
			base = cur.Version
		}
		if _, err := w.kb.Put(ctx, sp, name, []byte(text), base, knowledge.Author{Name: who, Email: email}, "Write "+name); err != nil {
			t.Fatal(err)
		}
	}
	put(w.private, "diary.md", "# Diary\nprivate thoughts\n", leaverName, leaverEmail)
	put(w.otherPrv, "notes.md", "# Carol's notes\n", "Carol", "carol@example.com")
	put(w.team, "plan.md", "# Plan\nv1\n", leaverName, leaverEmail)
	put(w.team, "plan.md", "# Plan\nv2\n", "Bob", "bob@example.com") // a later version by someone else, kept
	put(w.team, "minutes.md", "# Minutes\n", leaverName, leaverEmail)

	// A shared board with a card the leaver made and is assigned to, and comments: theirs, and
	// Bob's that mentions them.
	board := exec(t, sqlDB, `INSERT INTO boards (space_id, kind, name, created_by) VALUES (?, 'team', 'Work', ?)`, w.team.ID, w.leaver)
	w.card = exec(t, sqlDB, `INSERT INTO cards (board_id, title, col, rank, assignee_id, created_by, updated_by) VALUES (?, 'Ship it', 'todo', 'a', ?, ?, ?)`,
		board, w.leaver, w.leaver, w.leaver)
	exec(t, sqlDB, `INSERT INTO card_comments (card_id, author_id, body) VALUES (?, ?, 'I will do it')`, w.card, w.leaver)
	w.comment = exec(t, sqlDB, `INSERT INTO card_comments (card_id, author_id, body) VALUES (?, ?, ?)`, w.card, w.bob,
		fmt.Sprintf("thanks @[%s](user:%d), and @[Carol](user:%d) too", leaverName, w.leaver, w.carol))
	// Their private board, with a card.
	var pboard int64
	if err := sqlDB.QueryRow(`SELECT id FROM boards WHERE space_id = ?`, w.private.ID).Scan(&pboard); err != nil {
		t.Fatal(err)
	}
	exec(t, sqlDB, `INSERT INTO cards (board_id, title, col, rank) VALUES (?, 'Secret errand', 'todo', 'a')`, pboard)

	// Things that are only theirs.
	sess := exec(t, sqlDB, `INSERT INTO sessions (user_id, token_hash, expires_at) VALUES (?, 'h1', '2099-01-01T00:00:00.000Z')`, w.leaver)
	exec(t, sqlDB, `INSERT INTO push_subscriptions (user_id, session_id, endpoint, p256dh, auth_sealed) VALUES (?, ?, 'https://push.example.test/x', x'01', x'02')`, w.leaver, sess)
	exec(t, sqlDB, `INSERT INTO access_tokens (user_id, workspace_id, name, token_hash) VALUES (?, ?, 'laptop', 'th1')`, w.leaver, w.ws)
	exec(t, sqlDB, `INSERT INTO cli_accounts (user_id, cli, slot, state) VALUES (?, 'claude', 1, 'connected')`, w.leaver)
	exec(t, sqlDB, `INSERT INTO user_totp (user_id, secret, enabled_at) VALUES (?, x'01', '2026-01-01T00:00:00.000Z')`, w.leaver)
	exec(t, sqlDB, `INSERT INTO user_identities (issuer, subject, user_id) VALUES ('https://idp.example.test', 'sub-1', ?)`, w.leaver)
	exec(t, sqlDB, `INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, 101, 'about my diary')`, w.ws, w.leaver)
	exec(t, sqlDB, `INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, 102, 'gone already')`, w.ws, w.leaver)
	exec(t, sqlDB, `INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, 201, 'carol chat')`, w.ws, w.carol)
	w.agent.gone[102] = true
	exec(t, sqlDB, `INSERT INTO invites (workspace_id, token_hash, email, role, expires_at, created_by) VALUES (?, 'ih1', ?, 'member', '2099-01-01T00:00:00.000Z', ?)`,
		w.ws, leaverEmail, w.bob)
	if err := audit.Record(ctx, sqlDB, audit.Event{Workspace: w.ws, Actor: w.leaver, Action: audit.MemberRoleChanged,
		TargetType: audit.TargetUser, Target: w.carol, Details: audit.Details{"from": "member", "to": "admin"}}); err != nil {
		t.Fatal(err)
	}

	w.sched = schedule.New(sqlDB, access.New(sqlDB, nil), schedule.Options{Poll: 10 * time.Millisecond})
	w.svc = New(Options{DB: sqlDB, DataDir: dir, Scheduler: w.sched, Knowledge: w.kb, CLI: w.cli,
		Agent: func(context.Context) (conversations.Sidecar, error) { return w.agent, nil }})
	return w
}

func (w *world) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// deleteAccount does what a deletion request does to the rows, without the HTTP: the account is
// disabled and waits.
func (w *world) deleteAccount(by bool) {
	w.t.Helper()
	exec(w.t, w.db, `UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.leaver)
	exec(w.t, w.db, `INSERT INTO account_deletions (user_id, requested_at, purge_at, by_self) VALUES (?, '2026-01-01T00:00:00.000Z', '2026-01-15T00:00:00.000Z', ?)`, w.leaver, by)
}

// everythingAbout scans every table of the database for text that holds needle, and reports where.
func (w *world) everythingAbout(needle string) []string {
	w.t.Helper()
	var tables []string
	rows, err := w.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		w.t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	var found []string
	for _, tbl := range tables {
		r, err := w.db.Query(`SELECT * FROM "` + tbl + `"`) //nolint:gosec // G202: a table name read from sqlite_master
		if err != nil {
			continue // a virtual table's shadow
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				w.t.Fatal(err)
			}
			for i, v := range vals {
				var s string
				switch x := v.(type) {
				case string:
					s = x
				case []byte:
					s = string(x)
				}
				if strings.Contains(strings.ToLower(s), strings.ToLower(needle)) {
					found = append(found, tbl+"."+cols[i])
				}
			}
		}
		_ = r.Close()
	}
	return found
}

func TestPurgeDeletesWhatWasTheirsAndKeepsWhatWasShared(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.deleteAccount(true)
	privateDir := w.private.Dir(w.dir)
	if _, err := os.Stat(privateDir); err != nil {
		t.Fatalf("the private space's directory before: %v", err)
	}

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}

	// The account and everything that was theirs alone.
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 0 {
		t.Fatalf("the account remains")
	}
	if _, err := os.Stat(privateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the private space's directory remains: %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM spaces WHERE id = ` + fmt.Sprint(w.private.ID),
		`SELECT count(*) FROM boards WHERE space_id = ` + fmt.Sprint(w.private.ID),
		`SELECT count(*) FROM search_docs WHERE space_id = ` + fmt.Sprint(w.private.ID),
		`SELECT count(*) FROM conversations WHERE owner_id = ` + fmt.Sprint(w.leaver),
		`SELECT count(*) FROM sessions`,
		`SELECT count(*) FROM push_subscriptions`,
		`SELECT count(*) FROM access_tokens`,
		`SELECT count(*) FROM cli_accounts`,
		`SELECT count(*) FROM user_totp`,
		`SELECT count(*) FROM user_identities`,
		`SELECT count(*) FROM invites WHERE email = '` + leaverEmail + `'`,
		`SELECT count(*) FROM account_deletions`,
		`SELECT count(*) FROM memberships WHERE user_id = ` + fmt.Sprint(w.leaver),
	} {
		if n := w.count(q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	// Nothing in the database names them any more, in any table.
	for _, needle := range []string{leaverEmail, leaverName} {
		if found := w.everythingAbout(needle); len(found) > 0 {
			t.Errorf("%q is still in %v", needle, found)
		}
	}

	// The runtime's copies were retired (the one it no longer has isn't an error), and the CLI
	// worker's files forgotten.
	if len(w.agent.retired) != 1 || w.agent.retired[0] != 101 {
		t.Errorf("retired = %v, want [101]", w.agent.retired)
	}
	if len(w.cli.users) != 1 || w.cli.users[0] != w.leaver {
		t.Errorf("the CLI worker forgot %v", w.cli.users)
	}

	// Someone else's: their private space, conversation and files, untouched.
	if n := w.count(`SELECT count(*) FROM conversations WHERE owner_id = ?`, w.carol); n != 1 {
		t.Errorf("the bystander's conversations = %d", n)
	}
	if _, err := w.kb.Read(ctx, w.otherPrv, "notes.md", ""); err != nil {
		t.Errorf("the bystander's private document: %v", err)
	}

	// What they made for others stays, without them.
	var assignee, creator sql.NullInt64
	var deleted bool
	if err := w.db.QueryRow(`SELECT assignee_id, created_by, assignee_deleted FROM cards WHERE id = ?`, w.card).Scan(&assignee, &creator, &deleted); err != nil {
		t.Fatalf("the shared card was deleted: %v", err)
	}
	if assignee.Valid || creator.Valid || !deleted {
		t.Errorf("card assignee %v, creator %v, assignee_deleted %v", assignee, creator, deleted)
	}
	if n := w.count(`SELECT count(*) FROM card_comments WHERE card_id = ? AND author_id IS NULL AND body = 'I will do it'`, w.card); n != 1 {
		t.Errorf("their comment on the shared card: %d", n)
	}
	var body string
	if err := w.db.QueryRow(`SELECT body FROM card_comments WHERE id = ?`, w.comment).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("thanks @[Deleted user](user:%d), and @[Carol](user:%d) too", w.leaver, w.carol); body != want {
		t.Errorf("the mention reads %q, want %q", body, want)
	}
	// The documents they wrote in the shared space are there, now by "a deleted user".
	for name, text := range map[string]string{"plan.md": "v2", "minutes.md": "# Minutes"} {
		doc, err := w.kb.Read(ctx, w.team, name, "")
		if err != nil || !strings.Contains(string(doc.Content), text) {
			t.Errorf("shared document %s: %v %q", name, err, doc.Content)
		}
	}
	hist, err := w.kb.History(ctx, w.team, "plan.md", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	authors := map[string]bool{}
	for _, c := range hist.Commits {
		authors[c.Author.Name+" <"+c.Author.Email+">"] = true
	}
	if len(hist.Commits) != 2 || !authors["Bob <bob@example.com>"] ||
		!authors[knowledge.DeletedAuthorName+" <"+knowledge.DeletedAuthorEmail+">"] {
		t.Errorf("plan.md's history: %v", authors)
	}

	// The log: their events have no actor and say a deleted account did them; the purge is an
	// event of its own, with numbers and no names.
	var actorDeleted, actorNull int
	if err := w.db.QueryRow(`SELECT count(*), sum(actor_id IS NULL) FROM audit_events WHERE actor_deleted = 1`).Scan(&actorDeleted, &actorNull); err != nil {
		t.Fatal(err)
	}
	if actorDeleted < 1 || actorDeleted != actorNull {
		t.Errorf("events of a deleted actor: %d, with no actor: %d", actorDeleted, actorNull)
	}
	var details string
	var targetDeleted bool
	if err := w.db.QueryRow(`SELECT details, target_deleted FROM audit_events WHERE action = ?`, audit.AccountPurged).Scan(&details, &targetDeleted); err != nil {
		t.Fatalf("the purge's event: %v", err)
	}
	if !targetDeleted || !strings.Contains(details, `"private_spaces":1`) || !strings.Contains(details, `"conversations":2`) {
		t.Errorf("the purge's event: %s (target deleted %v)", details, targetDeleted)
	}
}

func TestPurgeIsIdempotentAndResumesAfterAFailure(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.deleteAccount(false)

	// A step that fails stops the purge before the account goes; the deletion can't be cancelled
	// any more, and nothing but what that step had reached is deleted.
	w.cli.fail = errors.New("the worker is down")
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge went on over a failed step")
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account went before its files did")
	}
	if n := w.count(`SELECT count(*) FROM account_deletions WHERE user_id = ? AND purge_started_at IS NOT NULL`, w.leaver); n != 1 {
		t.Fatal("a begun purge isn't marked")
	}

	w.cli.fail = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 0 {
		t.Fatal("the account remains after the retry")
	}
	// Run again, for an account that is gone: nothing happens, no error.
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatalf("a second purge: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = ?`, audit.AccountPurged); n != 1 {
		t.Errorf("%d purge events", n)
	}
}

func TestPurgeLeavesAnAccountWhoseDeletionWasCancelled(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	// No deletion row: it was cancelled, or never made. The job may still run.
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("an account that isn't deleted was purged")
	}
	if _, err := os.Stat(w.private.Dir(w.dir)); err != nil {
		t.Fatalf("its files: %v", err)
	}
	if len(w.agent.retired) != 0 || len(w.cli.users) != 0 {
		t.Fatalf("something was retired or forgotten")
	}
}

// The scheduler runs the purge when it is due, for an account that is disabled (by design), and
// not before.
func TestTheSchedulerRunsThePurgeForADisabledAccountWhenItIsDue(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	stop := w.sched.Start(context.Background())
	defer stop()

	w.deleteAccount(true)
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.SchedulePurge(context.Background(), tx, w.leaver, w.ws, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("purged before it was due")
	}

	// Due now.
	tx, err = w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.SchedulePurge(context.Background(), tx, w.leaver, w.ws, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduler didn't purge the account")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := w.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = ?`, string(Kind)); n != 0 {
		t.Errorf("%d purge jobs remain", n)
	}
}

func TestReconcileSchedulesAPurgeThatHasNoJob(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w.deleteAccount(true)
	if n := w.count(`SELECT count(*) FROM scheduled_jobs`); n != 0 {
		t.Fatalf("%d jobs before", n)
	}
	if err := w.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var runAt string
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(Kind), w.leaver).Scan(&runAt); err != nil {
		t.Fatalf("no purge job: %v", err)
	}
	if runAt != "2026-01-15T00:00:00.000Z" {
		t.Errorf("run_at = %s", runAt)
	}
	// Again: nothing changes.
	if err := w.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM scheduled_jobs`); n != 1 {
		t.Errorf("%d jobs after a second reconcile", n)
	}
}
