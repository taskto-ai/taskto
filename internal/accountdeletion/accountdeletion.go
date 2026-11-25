// Package accountdeletion purges a deleted account (docs/api.md#deleting-an-account).
//
// A person deletes their own account, or an admin deletes a member's (package auth), and the
// account is switched off at once and kept for a grace period, which an operator can cancel. When
// it ends, the scheduler runs the purge here, as the server and for no one's access: the
// scheduler's kind is registered with RegisterServer, so the account being disabled doesn't hold
// the job.
//
// # What the purge deletes: what the person owned alone
//
//   - their private spaces (in every workspace): the documents, with the unversioned files and
//     the large files' bytes, the personal boards, and the private apps with their records and
//     photos, from disk and from the database, and the search index's rows with them;
//   - their conversations, and the agent runtime's copies (retired: Pi Durable keeps no way to
//     delete a conversation, see package conversations);
//   - their CLI logins and shared-account mappings, with what the CLI worker kept for them
//     (sessions, working directories and the browser's profile);
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
// A comment's mention keeps its place but loses the label the writer typed.
//
// # Safety
//
// The purge is idempotent: each step finds what is left and finishes it, so a handler that was
// stopped or timed out runs again to the end. Once it has begun (account_deletions.purge_started_at)
// the deletion can't be cancelled. The account itself goes last, in one transaction with the audit
// events, so nothing that names the person survives a purge that completed.
package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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

// Knowledge is what the purge needs of the knowledge base: *knowledge.Store.
type Knowledge interface {
	AnonymizeAuthors(ctx context.Context, sp access.Space, emails []string) (int, error)
	RemoveSpace(sp access.Space) error
}

// CLIWorkers is what the purge needs of the CLI accounts: *cliaccounts.Service.
type CLIWorkers interface {
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
	// Agent returns the agent runtime's client for retiring conversations, or an error if it isn't
	// ready (the purge is tried again). Nil when the agent runtime is off: there are no runtime
	// conversations to retire.
	Agent func(ctx context.Context) (conversations.Sidecar, error)
	// CLI forgets the CLI worker's files of the person. Nil when CLI workers are off.
	CLI CLIWorkers
	// Clock is the time; time.Now by default.
	Clock func() time.Time
}

// Service schedules and runs the purges of deleted accounts.
type Service struct {
	o Options

	mu    sync.Mutex
	agent func(ctx context.Context) (conversations.Sidecar, error)
}

// New returns the service. Attach it to the scheduler before the scheduler starts, and give it to
// the auth service (auth.Service.SetDeletionScheduler).
func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Service{o: o, agent: o.Agent}
}

// SetAgent sets the agent runtime's client (Options.Agent), for a server that starts the agent
// runtime after the scheduler has the purge's handler. Call it before the server serves.
func (s *Service) SetAgent(agent func(ctx context.Context) (conversations.Sidecar, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agent = agent
}

func (s *Service) runtime(ctx context.Context) (conversations.Sidecar, error) {
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
		SELECT d.user_id, d.purge_at,
			coalesce((SELECT m.workspace_id FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
				WHERE m.user_id = d.user_id ORDER BY w.is_setup DESC, w.id LIMIT 1),
				(SELECT id FROM workspaces ORDER BY is_setup DESC, id LIMIT 1))
		FROM account_deletions d
		WHERE NOT EXISTS (SELECT 1 FROM scheduled_jobs j WHERE j.user_id = d.user_id AND j.kind = ? AND j.dedupe_key = ?
			AND j.state IN ('pending', 'running'))`, string(Kind), jobKey)
	if err != nil {
		return err
	}
	type missing struct {
		user, workspace int64
		at              string
	}
	var todo []missing
	for rows.Next() {
		var m missing
		var ws sql.NullInt64
		if err := rows.Scan(&m.user, &m.at, &ws); err != nil {
			_ = rows.Close()
			return err
		}
		m.workspace = ws.Int64
		todo = append(todo, m)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range todo {
		if m.workspace == 0 {
			continue // no workspace exists: nothing to file the job under
		}
		at, err := time.Parse("2006-01-02T15:04:05.000Z", m.at)
		if err != nil {
			return err
		}
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := s.SchedulePurge(ctx, tx, m.user, m.workspace, at); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slog.Info("account deletion: scheduled the purge of an account that had none", "user_id", m.user)
	}
	return nil
}

// Result is what a purge deleted, in numbers (the audit event's details).
type Result struct {
	PrivateSpaces       int64
	Conversations       int64
	AccessTokens        int64
	CLIAccounts         int64
	PushSubscriptions   int64
	DocumentsAnonymized int64
	MentionsAnonymized  int64
	BySelf              bool
}

// Purge deletes everything userID owned alone, and the account. It does nothing for an account
// that is gone or isn't waiting to be deleted (a cancelled deletion).
func (s *Service) Purge(ctx context.Context, userID int64) error {
	acct, ok, err := s.begin(ctx, userID)
	if err != nil || !ok {
		return err
	}
	res := Result{BySelf: acct.bySelf}

	// What is to go, read once the purge has begun: nothing new can be made by an account that
	// is switched off, and what the steps below delete is found again by a retry.
	private, err := s.spaces(ctx, `SELECT id, workspace_id FROM spaces WHERE owner_id = ? ORDER BY id`, userID)
	if err != nil {
		return err
	}
	shared, err := s.spaces(ctx, `SELECT id, workspace_id FROM spaces WHERE kind = 'shared' ORDER BY id`)
	if err != nil {
		return err
	}
	owned, err := conversations.OwnedBy(ctx, s.o.DB, userID)
	if err != nil {
		return err
	}

	// The agent runtime's copies first: a retry that finds the rows gone could no longer say
	// which of its conversations were theirs.
	if len(owned) > 0 {
		rt, err := s.runtime(ctx)
		if err != nil {
			return fmt.Errorf("the agent runtime isn't ready to retire the account's conversations: %w", err)
		}
		if rt != nil {
			if _, err := conversations.RetireOwned(ctx, rt, owned); err != nil {
				return err
			}
		}
	}
	if s.o.CLI != nil {
		if err := s.o.CLI.Forget(ctx, userID); err != nil {
			return err
		}
	}
	// Files before rows: a purge that stops between the two still finds the spaces to finish.
	for _, sp := range private {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.o.Knowledge.RemoveSpace(sp); err != nil {
			return err
		}
	}
	// Their name and address in what they wrote in the shared spaces, of every workspace they have
	// been in: the history of a space they left holds them too.
	for _, sp := range shared {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := s.o.Knowledge.AnonymizeAuthors(ctx, sp, []string{acct.email})
		if err != nil {
			return fmt.Errorf("replacing the account's name in space %d's history: %w", sp.ID, err)
		}
		res.DocumentsAnonymized += int64(n)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.finish(ctx, userID, acct, &res)
}

type account struct {
	email, name, created string
	bySelf               bool
}

// begin marks the purge as begun and reads who it is for. ok is false when there is nothing to
// do: the account is gone, or its deletion was cancelled.
func (s *Service) begin(ctx context.Context, userID int64) (acct account, ok bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT u.email, u.name, u.created_at, d.by_self
			FROM account_deletions d JOIN users u ON u.id = d.user_id WHERE d.user_id = ?`, userID).
			Scan(&acct.email, &acct.name, &acct.created, &acct.bySelf)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		_, err = tx.ExecContext(ctx, `UPDATE account_deletions SET purge_started_at = coalesce(purge_started_at, ?) WHERE user_id = ?`,
			s.o.Clock().UTC().Format("2006-01-02T15:04:05.000Z"), userID)
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
		if err := rows.Scan(&sp.ID, &sp.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// finish deletes the rows, and the account last, with the audit events, in one transaction.
func (s *Service) finish(ctx context.Context, userID int64, acct account, res *Result) error {
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
		for _, ws := range workspaces {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: ws, Action: audit.AccountPurged,
				TargetType: audit.TargetUser, Target: userID, TargetCreated: acct.created,
				Details: audit.Details{
					"by_self": res.BySelf, "private_spaces": res.PrivateSpaces, "conversations": res.Conversations,
					"access_grants": res.AccessTokens, "cli_accounts": res.CLIAccounts, "push_subscriptions": res.PushSubscriptions,
					"documents_anonymized": res.DocumentsAnonymized, "mentions_anonymized": res.MentionsAnonymized,
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
	if err != nil {
		return err
	}
	slog.Info("account deletion: purged an account", "user_id", userID, "private_spaces", res.PrivateSpaces,
		"conversations", res.Conversations, "documents_anonymized", res.DocumentsAnonymized)
	return nil
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
