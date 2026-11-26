package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/conversations"
)

// newApp makes an app, with a record, in space.
func (w *world) newApp(space int64, name string) (appID, rowID int64) {
	w.t.Helper()
	appID = exec(w.t, w.db, `INSERT INTO apps (space_id, name, definition) VALUES (?, ?, '{}')`, space, name)
	rowID = exec(w.t, w.db, `INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{}', ?)`, appID, w.bob)
	return appID, rowID
}

// attachment records a photo's row, as the store has it: ready on a record, or uploading.
func (w *world) attachment(id string, app, space, owner int64, row int64) {
	w.t.Helper()
	if row != 0 {
		exec(w.t, w.db, `INSERT INTO app_attachments (id, app_id, space_id, field, owner, status, size, received, content_type, row_id, attached_at)
			VALUES (?, ?, ?, 'photo', ?, 'ready', 5, 5, 'image/png', ?, '2026-01-01T00:00:00.000Z')`, id, app, space, owner, row)
		return
	}
	exec(w.t, w.db, `INSERT INTO app_attachments (id, app_id, space_id, field, owner, status, size, received) VALUES (?, ?, ?, 'photo', ?, 'uploading', 10, 5)`,
		id, app, space, owner)
}

func (w *world) filePath(sp access.Space, app int64, name string) string {
	return filepath.Join(sp.Dir(w.dir), access.ManagedDir, "app-files", fmt.Sprint(app), name)
}

func (w *world) putFile(sp access.Space, app int64, name, content string) {
	w.t.Helper()
	p := w.filePath(sp, app, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) hasFile(sp access.Space, app int64, name string) bool {
	_, err := os.Stat(w.filePath(sp, app, name))
	return err == nil
}

const (
	photoA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	photoB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	photoC = "cccccccccccccccccccccccccccccccc"
)

// An app moved from the person's private space to a shared one, with its files not yet moved (the
// move stopped, or its sweep hasn't run): the purge finishes the move, and the record keeps its photo.
func TestPurgeFinishesAnInterruptedAppMoveSoASharedPhotoSurvives(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, row := w.newApp(w.team.ID, "Receipts") // the app is in the shared space now
	// ...but its attachment still says the private space, where the bytes and the thumbnail are.
	w.attachment(photoA, app, w.private.ID, w.leaver, row)
	w.putFile(w.private, app, photoA, "picture")
	w.putFile(w.private, app, photoA+".thumb", "thumb")
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	var space int64
	if err := w.db.QueryRow(`SELECT space_id FROM app_attachments WHERE id = ?`, photoA).Scan(&space); err != nil {
		t.Fatalf("the shared record's photo was deleted with the private space: %v", err)
	}
	if space != w.team.ID {
		t.Errorf("the photo is recorded in space %d, want the shared space %d", space, w.team.ID)
	}
	for name, want := range map[string]string{photoA: "picture", photoA + ".thumb": "thumb"} {
		got, err := os.ReadFile(w.filePath(w.team, app, name))
		if err != nil || string(got) != want {
			t.Errorf("the shared space's %s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(w.private.Dir(w.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the private space remains: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM app_rows WHERE id = ?`, row); n != 1 {
		t.Error("the record is gone")
	}
}

// The purge doesn't go on while a shared photo is still recorded in the private space, whatever
// kept the move from finishing.
func TestPurgeStopsRatherThanDeleteASharedPhotoItCouldNotMove(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, row := w.newApp(w.team.ID, "Receipts")
	w.attachment(photoA, app, w.private.ID, w.leaver, row)
	// The bytes are a directory: they can't be copied as a file.
	if err := os.MkdirAll(w.filePath(w.private, app, photoA), 0o700); err != nil {
		t.Fatal(err)
	}
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge went on over a photo it couldn't move")
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, photoA); n != 1 {
		t.Fatal("the photo's row was deleted")
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account went")
	}
}

// What the effects of a shared app did stays, without who; what hadn't run yet can't, and goes.
func TestPurgeKeepsTheEffectHistoryOfASharedAppWithoutWho(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, row := w.newApp(w.team.ID, "Intake")
	op := exec(t, w.db, `INSERT INTO app_operations (app_id, user_id, operation, action, request_hash, record_id) VALUES (?, ?, 'op-1', 'add', 'h', ?)`, app, w.leaver, row)
	op2 := exec(t, w.db, `INSERT INTO app_operations (app_id, user_id, operation, action, request_hash, record_id) VALUES (?, ?, 'op-2', 'add', 'h', ?)`, app, w.bob, row)
	effect := func(operation int64, pos int, user int64, state string) int64 {
		return exec(t, w.db, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, state, next_attempt_at)
			VALUES (?, ?, ?, 'create_card', ?, 1, ?, 'insert', '{}', ?, '2026-01-01T00:00:00.000Z')`, app, operation, pos, row, user, state)
	}
	done := effect(op, 0, w.leaver, "done")
	failed := effect(op, 1, w.leaver, "failed")
	waiting := effect(op, 2, w.leaver, "pending")
	running := effect(op, 3, w.leaver, "running")
	others := effect(op2, 0, w.bob, "done")
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{done, failed} {
		var user sql.NullInt64
		if err := w.db.QueryRow(`SELECT user_id FROM app_effects WHERE id = ?`, id).Scan(&user); err != nil {
			t.Fatalf("effect %d was deleted with the account: %v", id, err)
		}
		if user.Valid {
			t.Errorf("effect %d still names user %d", id, user.Int64)
		}
	}
	for _, id := range []int64{waiting, running} {
		if n := w.count(`SELECT count(*) FROM app_effects WHERE id = ?`, id); n != 0 {
			t.Errorf("effect %d, which had not run, remains and would act for no one", id)
		}
	}
	var by int64
	if err := w.db.QueryRow(`SELECT user_id FROM app_effects WHERE id = ?`, others).Scan(&by); err != nil || by != w.bob {
		t.Errorf("someone else's effect: %d, %v", by, err)
	}
	var user sql.NullInt64
	if err := w.db.QueryRow(`SELECT user_id FROM app_operations WHERE id = ?`, op).Scan(&user); err != nil || user.Valid {
		t.Errorf("the operation's receipt: %v, %v", user, err)
	}
	if n := w.count(`SELECT count(*) FROM app_operations`); n != 2 {
		t.Errorf("%d operations remain, want 2", n)
	}
}

// An unfinished upload of the person in a shared app goes with its file; a photo already on a record,
// and another member's upload, stay.
func TestPurgeRemovesTheirUnfinishedUploadsInSharedApps(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, row := w.newApp(w.team.ID, "Site visits")
	w.attachment(photoA, app, w.team.ID, w.leaver, 0)   // theirs, being uploaded
	w.attachment(photoB, app, w.team.ID, w.leaver, row) // theirs, on a record
	w.attachment(photoC, app, w.team.ID, w.carol, 0)    // someone else's, being uploaded
	w.putFile(w.team, app, photoA+".part", "half a picture")
	w.putFile(w.team, app, photoB, "picture")
	w.putFile(w.team, app, photoC+".part", "carol's")
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, photoA); n != 0 {
		t.Error("their unfinished upload's row remains")
	}
	if w.hasFile(w.team, app, photoA+".part") {
		t.Error("their unfinished upload's file remains")
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ? AND row_id = ? AND owner IS NULL`, photoB, row); n != 1 {
		t.Error("the photo on a record was touched")
	}
	if !w.hasFile(w.team, app, photoB) {
		t.Error("the photo on a record lost its file")
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, photoC); n != 1 || !w.hasFile(w.team, app, photoC+".part") {
		t.Error("another member's upload was removed")
	}
}

// A mention written with zeros in front is the same person's, and goes the same way; it can't name
// whoever is given a number later.
func TestPurgeRewritesAZeroPaddedMention(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	padded := exec(t, w.db, `INSERT INTO card_comments (card_id, author_id, body) VALUES (?, ?, ?)`, w.card, w.bob,
		fmt.Sprintf("@[%s](user:%04d) please, and @[%s](user:0%d)", leaverName, w.leaver, leaverName, w.leaver))
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := w.db.QueryRow(`SELECT body FROM card_comments WHERE id = ?`, padded).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, leaverName) || body != "@[Deleted user](user:0) please, and @[Deleted user](user:0)" {
		t.Errorf("the comment reads %q", body)
	}
}

// The agent runtime's database keeps what was said until the server has it purged: a purge that
// can't reach the runtime stays unfinished, with the account and the record of what is to do, and
// finishes when it can.
func TestPurgeWaitsForTheAgentRuntimeToPurgeTheTranscripts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.agent.down = errors.New("the runtime is restarting")
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge went on without the runtime")
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account went while its conversations were still in the runtime")
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ?`, cleanAgentConversation); n != 2 {
		t.Fatalf("%d conversations recorded to purge, want 2", n)
	}
	if n := w.count(`SELECT count(*) FROM conversations WHERE owner_id = ?`, w.leaver); n != 2 {
		t.Fatal("the server forgot whose the conversations were")
	}

	w.agent.down = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if len(w.agent.purged) != 1 || w.agent.purged[0] != 101 {
		t.Errorf("purged = %v", w.agent.purged)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps are still recorded", n)
	}
}

// With the agent runtime off, the account goes (nothing can be asked of it), and what it keeps is
// recorded for when it is on: the purge says so, and a later start does it.
func TestPurgeWithTheAgentRuntimeOffRecordsTheStepAndASetAgentFinishesIt(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.svc.SetAgent(nil)
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ?`, cleanAgentConversation); n != 2 {
		t.Fatalf("%d conversations recorded, want 2", n)
	}
	var details string
	if err := w.db.QueryRow(`SELECT details FROM audit_events WHERE action = ?`, audit.AccountPurged).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"cleanups_pending":`) || strings.Contains(details, `"cleanups_pending":0`) {
		t.Errorf("the purge's event doesn't say steps are pending: %s", details)
	}
	if n, err := w.svc.Pending(ctx, w.leaver); err != nil || n < 2 {
		t.Fatalf("pending = %d, %v", n, err)
	}

	w.svc.SetAgent(func(context.Context) (conversations.Purger, error) { return w.agent, nil })
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.agent.purged) != 1 || w.agent.purged[0] != 101 {
		t.Errorf("purged = %v", w.agent.purged)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ?`, cleanAgentConversation); n != 0 {
		t.Errorf("%d conversation steps remain after the runtime was on", n)
	}
}

// The providers file and the key files hold the person's provider accounts: they are rewritten
// once the account is gone, and with no running agent runtime they are removed.
func TestPurgeRefreshesTheProvidersFileAndKeys(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	dir := filepath.Join(w.dir, "agent")
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"providers.json": "{}", "keys/mine.key": "not-a-real-key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"providers.json", "keys"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", name, err)
		}
	}

	// With the runtime's feed, it is asked to write them again from the database that no longer has
	// the accounts, after the account is gone; a failure leaves the step to try again.
	w2 := newWorld(t)
	calls, fail := 0, errors.New("the feed can't write")
	w2.svc.SetProviders(func(context.Context) error {
		calls++
		if n := w2.count(`SELECT count(*) FROM users WHERE id = ?`, w2.leaver); n != 0 {
			t.Error("the providers file was rewritten while the account was still there")
		}
		return fail
	})
	w2.deleteAccount()
	if err := w2.svc.Purge(ctx, w2.leaver); err != nil {
		t.Fatalf("a failure after the account went failed the purge: %v", err)
	}
	if n := w2.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ?`, cleanProviders); n != 1 {
		t.Fatalf("%d provider steps recorded after a failure, want 1", n)
	}
	fail = nil
	if err := w2.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || w2.count(`SELECT count(*) FROM purge_cleanups`) != 0 {
		t.Errorf("calls = %d, steps left = %d", calls, w2.count(`SELECT count(*) FROM purge_cleanups`))
	}
}

// What the CLI workers kept for the person goes even with the workers off: the local worker's
// directory under the data directory is removed, and the worker's own is recorded for when they are on.
func TestPurgeRemovesTheLocalCLIDirectoryWithWorkersOffAndRecordsTheRest(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	home := filepath.Join(w.dir, "cli", fmt.Sprintf("u%d", w.leaver), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "login"), []byte("a login"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(w.dir, "cli", fmt.Sprintf("u%d", w.carol), "home")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	w.cli.fail = cliaccounts.ErrOff // CLI workers are off
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "cli", fmt.Sprintf("u%d", w.leaver))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("their CLI directory remains: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("someone else's CLI directory: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ? AND target = ?`, cleanCLIHome, fmt.Sprint(w.leaver)); n != 1 {
		t.Fatal("the worker's own directory isn't recorded for when the workers are on")
	}
	// The workers come on: it is forgotten there.
	w.cli.fail = nil
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.cli.users) != 1 || w.cli.users[0] != w.leaver {
		t.Errorf("the CLI worker forgot %v", w.cli.users)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain", n)
	}
}

// The person's id is never given to another account.
func TestAPurgedAccountsIDIsNeverGivenAgain(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	newest := exec(t, w.db, `INSERT INTO users (email, name, password_hash) VALUES ('newest@example.com', 'Newest', 'x')`)
	exec(t, w.db, `INSERT INTO account_deletions (user_id, requested_at, purge_at) VALUES (?, '2026-01-01T00:00:00.000Z', '2026-01-15T00:00:00.000Z')`, newest)
	if err := w.svc.Purge(ctx, newest); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT high FROM user_ids`); int64(n) != newest {
		t.Fatalf("the highest id there has been is %d, want %d", n, newest)
	}
}

// Removing the person from a workspace while their purge is failing doesn't lose the job: the
// scheduler keeps trying it without a restart. Jobs of other kinds still go with the membership.
func TestLeavingAWorkspaceKeepsThePurgeJobButNotOtherJobs(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w.deleteAccount()
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.SchedulePurge(ctx, tx, w.leaver, w.ws, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		VALUES ('card.reminder', ?, ?, 'k', '2099-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`, w.leaver, w.ws)

	exec(t, w.db, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, w.ws, w.leaver)
	if n := w.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = ? AND user_id = ? AND state = 'pending'`, string(Kind), w.leaver); n != 1 {
		t.Fatalf("the purge job went with the membership: %d left", n)
	}
	if n := w.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = 'card.reminder'`); n != 0 {
		t.Errorf("another kind of job survived the membership: %d", n)
	}
}

// A purge whose tries ran out is scheduled again by the periodic reconcile, with no restart.
func TestMaintainSchedulesAPurgeWhoseTriesRanOut(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w.deleteAccount()
	if err := w.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	exec(t, w.db, `UPDATE scheduled_jobs SET state = 'failed', last_error = 'gave up' WHERE kind = ?`, string(Kind))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.svc.Maintain(runCtx, 20*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for w.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = ? AND state IN ('pending', 'running')`, string(Kind)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the failed purge job was never scheduled again")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
