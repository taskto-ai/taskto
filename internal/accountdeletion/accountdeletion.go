// Package accountdeletion purges a deleted account (docs/api.md#deleting-an-account).
//
// A person deletes their own account (package auth; nobody else can), and the account is switched
// off at once and kept for a grace period, which an operator can cancel. When
// it ends, the scheduler runs the purge here, as the server and for no one's access: the
// scheduler's kind is registered with RegisterServer, so the account being disabled doesn't hold
// the job.
//
// # What the purge deletes: what the person owned alone
//
//   - their private spaces (in every workspace): the documents, with the unversioned files and
//     the large files' bytes, the personal boards, and the private apps with their records and
//     photos, from disk and from the database, and the search index's rows with them;
//   - their conversations, and the agent runtime's copies (retired, then purged from its database:
//     Pi Durable has no way to delete a conversation, see package conversations);
//   - their CLI logins and shared-account mappings, with what the CLI worker kept for them
//     (sessions, working directories and the browser's profile), and the providers file and keys of
//     the agent runtime, which hold their provider accounts;
//   - their unfinished uploads in shared apps, with the files;
//   - their access tokens, sessions, push subscriptions, notification settings and email ledger,
//     two-factor data, single sign-on identities, reset links and the addresses of invitations to
//     them: all rows of the account, which the schema deletes with it;
//   - their name and address in the shared spaces' history, which become a placeholder.
//
// # What stays: what they made for others
//
// Everything in a shared space, and the audit log, stays. The rows that name the person (a card's
// creator, an assignee, a comment's author, a record's creator, an audit event's actor) lose the
// account by the schema's ON DELETE SET NULL, which the product shows as "Deleted user"; the
// assignee, a document's author and the audit actor, for which no account would read like no one,
// say so with a mark (cards.assignee_deleted, the placeholder author, audit_events.actor_deleted).
// A comment's mention becomes one of nobody (user 0), which no account can ever be. The photos of
// a shared app's records stay, even those whose files an interrupted move of the app left in the
// person's private space (finished first), and so does what the effects of a shared app did, without
// who. Their entries that hadn't run yet are cancelled: no one is left to act for.
//
// # Safety
//
// The purge is idempotent: each step finds what is left and finishes it, so a handler that was
// stopped or timed out runs again to the end. Once it has begun (account_deletions.purge_started_at)
// the deletion can't be cancelled. The account itself goes last, in one transaction with the audit
// events, so nothing that names the person survives a purge that completed. What is about files and
// other processes (the agent runtime's records, the CLI worker's directory, the providers file) is
// recorded first (purge_cleanups) and removed from the record when done: a step whose subsystem
// is off stays recorded after the account is gone and is done when it is on, and the purge says it
// completed only when none is left (the audit event's cleanups_pending, the log line).
package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/schedule"
)

// Kind is the scheduler's kind of a purge.
const Kind schedule.Kind = "account.purge"

// jobKey is the purge job's dedupe key: one per account.
const jobKey = "purge"

// Apps is what the purge needs of the apps: *apps.Service.
type Apps interface {
	// FinishMoves completes the moves of apps that involve the space, so no shared app's photo is
	// left in it, and fails if one is.
	FinishMoves(ctx context.Context, space int64) error
	// PurgeUploads removes the account's unfinished uploads and unattached photos, with files.
	// removed is called in the transaction that deletes each one's row.
	PurgeUploadsNoting(ctx context.Context, owner int64, removed func(ctx context.Context, tx *sql.Tx) error) (int, error)
}

// Knowledge is what the purge needs of the knowledge base: *knowledge.Store.
type Knowledge interface {
	// planned is called with the number of commits to rewrite before the rewrite is journaled.
	AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(n int) error) (int, error)
	// RemoveSpace removes a space's files, and refuses every later call on it until ForgetSpace.
	RemoveSpace(sp access.Space) error
	// ForgetSpace drops the refusal once the space's rows are deleted: a space made later may be
	// given its ID.
	ForgetSpace(sp access.Space)
}

// CLIWorkers is what the purge needs of the CLI accounts: *cliaccounts.Service.
type CLIWorkers interface {
	Forget(ctx context.Context, user int64) error
	// WorkerKind is where this server's worker keeps a user's files: "local", "container", or ""
	// when CLI workers are off (cliaccounts.WorkerKind).
	WorkerKind() string
}

// Browsers is what the purge needs of the agent browsers: browser.Browsers.
type Browsers interface {
	// Forget stops the account's browsers, closes their live views, refuses to start new ones while
	// it runs and removes their files. It returns nil only when the processes are gone and the files
	// are removed, and is safe to call again.
	Forget(ctx context.Context, user int64) error
}

// Options are what a Service needs.
type Options struct {
	// DB is the migrated database.
	DB *sql.DB
	// DataDir is the data directory.
	DataDir string
	// Scheduler runs the purges. Required.
	Scheduler *schedule.Service
	// Knowledge removes the private spaces' files and rewrites the shared ones' authors. Required.
	Knowledge Knowledge
	// Apps finishes apps' moves and removes uploads. Required once the server serves: the apps
	// service is made with the server's handler, so it may be set later (SetApps); a purge before
	// that fails, and is tried again, rather than skip what the apps keep.
	Apps Apps
	// Agent returns the agent runtime's client for purging conversations, or an error if it isn't
	// ready (the purge is tried again). Nil when the agent runtime is off: a conversation's records
	// stay recorded as a step to do when it is on.
	Agent func(ctx context.Context) (conversations.Purger, error)
	// CLI forgets the CLI worker's files of the person. Nil when CLI workers are off: a step that
	// needs a worker that isn't running stays recorded for when it is.
	CLI CLIWorkers
	// Browsers stops the account's agent browsers and removes their profile (a durable step, tried
	// again until it is done). Nil when this server has no agent browser to stop: local mode with the
	// browser off, or CLI workers off.
	Browsers Browsers
	// Clock is the time; time.Now by default.
	Clock func() time.Time
}

// Service schedules and runs the purges of deleted accounts.
type Service struct {
	o Options

	mu        sync.Mutex
	apps      Apps
	agent     func(ctx context.Context) (conversations.Purger, error)
	providers func(ctx context.Context) error

	// beforeSchedule, for tests, runs in Reconcile between the list of accounts without a job and
	// the transaction that makes each one's job.
	beforeSchedule func(user int64)

	// stepTimeout, for tests, replaces defaultAgentTimeout.
	stepTimeout time.Duration
}

// New returns the service. Attach it to the scheduler before the scheduler starts, and give it to
// the auth service (auth.Service.SetDeletionScheduler).
func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Service{o: o, agent: o.Agent, apps: o.Apps}
}

// SetAgent sets the agent runtime's client (Options.Agent), for a server that starts the agent
// runtime after the scheduler has the purge's handler. Call it before the server serves.
func (s *Service) SetAgent(agent func(ctx context.Context) (conversations.Purger, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agent = agent
}

// SetApps sets the apps service (Options.Apps).
func (s *Service) SetApps(a Apps) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps = a
}

func (s *Service) runtime(ctx context.Context) (conversations.Purger, error) {
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	if agent == nil {
		return nil, nil
	}
	return agent(ctx)
}

// Attach registers the purge's handler with the scheduler, which must not have started.
func (s *Service) Attach() error {
	return s.o.Scheduler.RegisterServer(Kind, s.handle)
}

// handle runs a purge. A failure is logged, with its error, by the scheduler, and tried again.
func (s *Service) handle(ctx context.Context, j schedule.Job) error {
	return s.Purge(ctx, j.UserID)
}

// SchedulePurge makes the purge of userID due at at, within tx (auth.DeletionScheduler). A purge
// scheduled again is moved to the new time.
func (s *Service) SchedulePurge(ctx context.Context, tx *sql.Tx, userID, workspaceID int64, at time.Time) error {
	_, err := s.o.Scheduler.Reschedule(ctx, tx, schedule.Job{Kind: Kind, UserID: userID, WorkspaceID: workspaceID, DedupeKey: jobKey, RunAt: at})
	return err
}

// CancelPurge removes the purge of userID, within tx (auth.DeletionScheduler).
func (s *Service) CancelPurge(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := s.o.Scheduler.Cancel(ctx, tx, userID, Kind, jobKey)
	return err
}

// Reconcile makes sure every account waiting to be deleted has its purge scheduled: one that was
// deleted while no scheduler was set, whose job was lost (a person's jobs go when they leave a
// workspace), or whose tries ran out. Call it at start, once Attach has run.
func (s *Service) Reconcile(ctx context.Context) error {
	rows, err := s.o.DB.QueryContext(ctx, `
		SELECT d.user_id FROM account_deletions d
		WHERE NOT EXISTS (SELECT 1 FROM scheduled_jobs j WHERE j.user_id = d.user_id AND j.kind = ? AND j.dedupe_key = ?
			AND j.state IN ('pending', 'running'))`, string(Kind), jobKey)
	if err != nil {
		return err
	}
	var todo []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		todo = append(todo, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, user := range todo {
		// The request is read again where the job is written, under the write lock: it may have been
		// cancelled and made again since the list above, and a deadline read before would put the
		// old one on the new request, or a job on a request that is gone.
		if s.beforeSchedule != nil {
			s.beforeSchedule(user)
		}
		scheduled := false
		err := s.write(ctx, func(tx *sql.Tx) error {
			scheduled = false
			var at string
			err := tx.QueryRowContext(ctx, `SELECT purge_at FROM account_deletions WHERE user_id = ?`, user).Scan(&at)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			var jobs int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_jobs WHERE user_id = ? AND kind = ? AND dedupe_key = ?
				AND state IN ('pending', 'running')`, user, string(Kind), jobKey).Scan(&jobs); err != nil {
				return err
			}
			if jobs > 0 {
				return nil // the request was made again, with its own job
			}
			ws, err := s.workspaceOf(ctx, tx, user)
			if err != nil || ws == 0 {
				return err // no workspace exists: nothing to file the job under
			}
			when, err := time.Parse(stampFormat, at)
			if err != nil {
				return err
			}
			if err := s.SchedulePurge(ctx, tx, user, ws, when); err != nil {
				return err
			}
			scheduled = true
			return nil
		})
		if err != nil {
			return err
		}
		if scheduled {
			slog.Info("account deletion: scheduled the purge of an account that had none", "user_id", user)
		}
	}
	return nil
}

// stampFormat is how the tables write a time.
const stampFormat = "2006-01-02T15:04:05.000Z"

// workspaceOf is the workspace a purge job of userID is filed under: the first they belong to, or
// the first there is. 0 if no workspace exists.
func (s *Service) workspaceOf(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID int64) (int64, error) {
	var ws sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT coalesce((SELECT m.workspace_id FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
			WHERE m.user_id = ? ORDER BY w.is_setup DESC, w.id LIMIT 1),
			(SELECT id FROM workspaces ORDER BY is_setup DESC, id LIMIT 1))`, userID).Scan(&ws)
	return ws.Int64, err
}

// Result is what a purge deleted, in numbers (the audit event's details).
type Result struct {
	PrivateSpaces     int64
	Conversations     int64
	AccessTokens      int64
	CLIAccounts       int64
	PushSubscriptions int64
	// DocumentsAnonymized and UploadsRemoved are done before the account's transaction, and can be
	// done by an earlier try than the one that completes: their counts are kept with the work
	// (account_deletions.uploads_removed, purge_anonymized), and these are the totals (docs/audit.md).
	DocumentsAnonymized int64
	MentionsAnonymized  int64
	UploadsRemoved      int64
	// CleanupsPending is how many of the purge's steps about files and other processes are still
	// recorded when the account goes: they wait for the agent runtime or the CLI workers to be on.
	CleanupsPending int64
}

// Purge deletes everything userID owned alone, and the account. It does nothing for an account
// that is gone or isn't waiting to be deleted (a cancelled deletion).
func (s *Service) Purge(ctx context.Context, userID int64) error {
	acct, ok, err := s.begin(ctx, userID)
	if err != nil || !ok {
		return err
	}
	res := Result{}

	// What is to go, read once the purge has begun: nothing new can be made by an account that
	// is switched off, and what the steps below delete is found again by a retry.
	private, err := s.spaces(ctx, `SELECT id, workspace_id, generation FROM spaces WHERE owner_id = ? ORDER BY id`, userID)
	if err != nil {
		return err
	}
	shared, err := s.spaces(ctx, `SELECT id, workspace_id, generation FROM spaces WHERE kind = 'shared' ORDER BY id`)
	if err != nil {
		return err
	}
	owned, err := conversations.OwnedBy(ctx, s.o.DB, userID)
	if err != nil {
		return err
	}

	// The steps about files and other processes, recorded before anything is deleted: a retry that
	// finds the rows gone could no longer say which conversations were theirs, and a step that has
	// to wait for the agent runtime or the CLI workers to be on is still known after the account.
	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, o := range owned {
			if err := s.record(ctx, tx, userID, cleanAgentConversation, strconv.FormatInt(o.AgentID, 10)); err != nil {
				return err
			}
		}
		// The browsers first: the CLI homes hold their directory, and are left until they are gone.
		if err := s.record(ctx, tx, userID, cleanBrowser, strconv.FormatInt(userID, 10)); err != nil {
			return err
		}
		return s.recordCLIHomes(ctx, tx, userID)
	})
	if err != nil {
		return err
	}

	// What an app moved out of (or into) a private space still holds there is moved first, so the
	// removal of the space below never takes a shared record's photo with it.
	s.mu.Lock()
	apps := s.apps
	s.mu.Unlock()
	if apps == nil {
		return errors.New("the apps service isn't set yet: the purge waits for it, so what the apps keep isn't skipped")
	}
	// Each space is tried even when another's fails (they are independent); the purge stops after
	// the loop, before anything is removed, if one did.
	var moveErrs []error
	for _, sp := range private {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := apps.FinishMoves(ctx, sp.ID); err != nil {
			moveErrs = append(moveErrs, fmt.Errorf("finishing the app moves of space %d: %w", sp.ID, err))
		}
	}
	if err := errors.Join(moveErrs...); err != nil {
		return err
	}
	// Their unfinished uploads in any app (a shared app's are theirs alone): with files. The photos
	// already on records stay with the records.
	// Each is counted in the transaction that removes it, so a retry neither loses nor repeats it.
	if _, err := apps.PurgeUploadsNoting(ctx, userID, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE account_deletions SET uploads_removed = uploads_removed + 1 WHERE user_id = ?`, userID)
		return err
	}); err != nil {
		return err
	}

	// The agent runtime's records, the CLI worker's directory: each is done now, or stays recorded
	// when what it waits for is off. One that is on and fails fails the purge, which is tried again.
	if _, err := s.runCleanups(ctx, userID); err != nil {
		return err
	}

	// Files before rows: a purge that stops between the two still finds the spaces to finish.
	// Every directory is tried even when one can't be removed; the rows stay until all are gone.
	var removeErrs []error
	for _, sp := range private {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.o.Knowledge.RemoveSpace(sp); err != nil {
			removeErrs = append(removeErrs, fmt.Errorf("removing private space %d: %w", sp.ID, err))
		}
	}
	if err := errors.Join(removeErrs...); err != nil {
		return err
	}
	// Their name and address in what they wrote in the shared spaces, of every workspace they have
	// been in: the history of a space they left holds them too.
	// Every repository is tried even when one fails; the purge stays pending if any did.
	var anonErrs []error
	for _, sp := range shared {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The space's count is kept before its rewrite is journaled: a rewrite that stops after that
		// is finished by the next try (or start), which finds nothing left to count.
		if _, err := s.o.Knowledge.AnonymizeAuthorsNoting(ctx, sp, []string{acct.email}, func(n int) error {
			_, err := s.o.DB.ExecContext(ctx, `INSERT INTO purge_anonymized (user_id, space_id, commits) VALUES (?, ?, ?)
				ON CONFLICT (user_id, space_id) DO UPDATE SET commits = excluded.commits`, userID, sp.ID, n)
			return err
		}); err != nil {
			anonErrs = append(anonErrs, fmt.Errorf("replacing the account's name in space %d's history: %w", sp.ID, err))
		}
	}
	if err := errors.Join(anonErrs...); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finishErr := s.finish(ctx, userID, private, acct, &res)
	// Whatever finish returned: a transaction that committed and then failed to report it has
	// deleted the rows, and a space made later with the ID must not stay refused until a restart.
	s.forgetGone(ctx, private)
	if finishErr != nil {
		return finishErr
	}
	// The providers file and the key files, now that the database has no account of theirs. A
	// failure leaves the step recorded: it is the account that is gone, and the step is tried again.
	if _, err := s.runCleanups(ctx, userID); err != nil {
		slog.Warn("account deletion: a step after the account was deleted failed; it is tried again", "user_id", userID, "err", err)
	}
	pending, err := s.Pending(ctx, userID)
	if err != nil {
		return err
	}
	if pending > 0 {
		slog.Warn("account deletion: purged an account, with steps still to do when the agent runtime or the CLI workers are on",
			"user_id", userID, "steps", pending)
	} else {
		slog.Info("account deletion: purged an account", "user_id", userID, "private_spaces", res.PrivateSpaces,
			"conversations", res.Conversations, "documents_anonymized", res.DocumentsAnonymized)
	}
	return nil
}

// forgetGone tells the knowledge base to forget the removal of each of spaces whose row is gone
// (knowledge.Store.ForgetSpace): the refusal is kept for a space that is still recorded, since the
// purge is then still to finish it, and a database that can't be read leaves it all as it is. The
// row is the space's by its ID and its generation (a number no space has twice): a space made since with the purged one's ID
// is another space, and the purged one is gone.
func (s *Service) forgetGone(ctx context.Context, spaces []access.Space) {
	for _, sp := range spaces {
		var one int
		err := s.o.DB.QueryRowContext(ctx, `SELECT 1 FROM spaces WHERE id = ? AND (? = 0 OR generation = ?)`,
			sp.ID, sp.Generation, sp.Generation).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			s.o.Knowledge.ForgetSpace(sp)
		}
	}
}

type account struct {
	email, name, created string
}

// begin marks the purge as begun and reads who it is for. ok is false when there is nothing to
// do: the account is gone, its deletion was cancelled, or the request in force isn't due yet. A
// job is only a reminder to look: it may be one of a request that was cancelled and made again, so
// the deadline that counts is the current request's, read here under the write lock, and a job that
// ran too early is moved to it.
func (s *Service) begin(ctx context.Context, userID int64) (acct account, ok bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		ok = false
		var purgeAt string
		err := tx.QueryRowContext(ctx, `
			SELECT u.email, u.name, u.created_at, d.purge_at
			FROM account_deletions d JOIN users u ON u.id = d.user_id WHERE d.user_id = ?`, userID).
			Scan(&acct.email, &acct.name, &acct.created, &purgeAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		due, err := time.Parse(stampFormat, purgeAt)
		if err != nil {
			return err
		}
		if s.o.Clock().Before(due) {
			ws, err := s.workspaceOf(ctx, tx, userID)
			if err != nil || ws == 0 {
				return err
			}
			slog.Info("account deletion: a purge job ran before its request is due; it is moved to the deadline", "user_id", userID)
			return s.SchedulePurge(ctx, tx, userID, ws, due)
		}
		ok = true
		_, err = tx.ExecContext(ctx, `UPDATE account_deletions SET purge_started_at = coalesce(purge_started_at, ?) WHERE user_id = ?`,
			s.o.Clock().UTC().Format(stampFormat), userID)
		return err
	})
	return acct, ok, err
}

func (s *Service) spaces(ctx context.Context, query string, args ...any) ([]access.Space, error) {
	rows, err := s.o.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []access.Space
	for rows.Next() {
		sp := access.Space{Access: access.Write}
		if err := rows.Scan(&sp.ID, &sp.WorkspaceID, &sp.Generation); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// finish deletes the rows, and the account last, with the audit events, in one transaction.
func (s *Service) finish(ctx context.Context, userID int64, private []access.Space, acct account, res *Result) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		// The workspaces they belong to, to say the purge in each one's log.
		var workspaces []int64
		rows, err := tx.QueryContext(ctx, `SELECT workspace_id FROM memberships WHERE user_id = ? ORDER BY workspace_id`, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			workspaces = append(workspaces, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(workspaces) == 0 {
			workspaces = []int64{0}
		}
		// The space's rows take the photos of the apps in it with them (the schema's cascade): a
		// photo of an app that lives elsewhere, still recorded here, is a shared record's, and the
		// purge stops rather than delete it. FinishMoves moved them before the files went.
		var stranded int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM app_attachments t JOIN apps a ON a.id = t.app_id
			JOIN spaces s ON s.id = t.space_id AND s.owner_id = ?
			WHERE a.space_id != t.space_id`, userID).Scan(&stranded); err != nil {
			return err
		}
		if stranded > 0 {
			return fmt.Errorf("%d attachments of apps in other spaces are still recorded in the account's private spaces", stranded)
		}
		// What the steps before this transaction did, over every try.
		if err := tx.QueryRowContext(ctx, `SELECT d.uploads_removed, (SELECT coalesce(sum(a.commits), 0) FROM purge_anonymized a WHERE a.user_id = d.user_id)
			FROM account_deletions d WHERE d.user_id = ?`, userID).Scan(&res.UploadsRemoved, &res.DocumentsAnonymized); err != nil {
			return err
		}
		for _, c := range []struct {
			into  *int64
			query string
		}{
			{&res.AccessTokens, `SELECT count(*) FROM access_tokens WHERE user_id = ?`},
			{&res.CLIAccounts, `SELECT count(*) FROM cli_accounts WHERE user_id = ?`},
			{&res.PushSubscriptions, `SELECT count(*) FROM push_subscriptions WHERE user_id = ?`},
		} {
			if err := tx.QueryRowContext(ctx, c.query, userID).Scan(c.into); err != nil {
				return err
			}
		}
		if res.Conversations, err = conversations.DeleteOwned(ctx, tx, userID); err != nil {
			return err
		}
		if res.MentionsAnonymized, err = boards.AnonymizeMentions(ctx, tx, userID, "Deleted user"); err != nil {
			return err
		}
		// The assignee is about to become none: say it was a deleted account.
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET assignee_deleted = 1 WHERE assignee_id = ?`, userID); err != nil {
			return err
		}
		// What the effects of apps did for them stays (the schema sets their user to none), but an
		// entry that hasn't run can't: it would act as them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_effects WHERE user_id = ? AND state IN ('pending', 'running')`, userID); err != nil {
			return err
		}
		// Their private spaces go with everything in them (boards, apps, records, photos' rows, the
		// search index's rows), by the schema's cascade; their files went before.
		spaces, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE owner_id = ?`, userID)
		if err != nil {
			return err
		}
		if res.PrivateSpaces, err = spaces.RowsAffected(); err != nil {
			return err
		}
		// Invitations to their address carry it.
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE email = ?`, acct.email); err != nil {
			return err
		}
		// The providers file the agent runtime reads holds their provider accounts, which go with
		// the account: its rewriting is recorded in the transaction that makes it necessary, as a
		// step of this account alone, so another purge's acknowledgement of the file can't clear it.
		if err := s.record(ctx, tx, userID, cleanProviders, providersTarget(userID)); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM purge_cleanups WHERE user_id = ?`, userID).
			Scan(&res.CleanupsPending); err != nil {
			return err
		}
		for _, ws := range workspaces {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: ws, Action: audit.AccountPurged,
				TargetType: audit.TargetUser, Target: userID, TargetCreated: acct.created,
				Details: audit.Details{
					"private_spaces": res.PrivateSpaces, "conversations": res.Conversations,
					"access_grants": res.AccessTokens, "cli_accounts": res.CLIAccounts, "push_subscriptions": res.PushSubscriptions,
					"documents_anonymized": res.DocumentsAnonymized, "mentions_anonymized": res.MentionsAnonymized,
					"uploads_removed": res.UploadsRemoved, "cleanups_pending": res.CleanupsPending,
				}}); err != nil {
				return err
			}
		}
		if _, err := audit.Erase(ctx, tx, userID, acct.created, acct.email); err != nil {
			return err
		}
		// The account, and with it, by the schema, its memberships and every row that is theirs
		// alone; the rows of what they made for others lose them (ON DELETE SET NULL).
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID); err != nil {
			return err
		}
		return nil
	})
	return err
}

// write runs fn in a transaction that takes the write lock when it begins, rolled back on error.
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.o.DB.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
