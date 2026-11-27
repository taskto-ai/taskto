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

const Kind schedule.Kind = "account.purge"

const jobKey = "purge"

type Apps interface {
	FinishMoves(ctx context.Context, space int64) error
	PurgeUploadsNoting(ctx context.Context, owner int64, removed func(ctx context.Context, tx *sql.Tx) error) (int, error)
}

type Knowledge interface {
	AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(n int) error) (int, error)
	RemoveSpace(sp access.Space) error
	ForgetSpace(sp access.Space)
}

type CLIWorkers interface {
	Forget(ctx context.Context, user int64) error
	WorkerKind() string
}

type Browsers interface {
	Forget(ctx context.Context, user int64) error
}

type Options struct {
	DB        *sql.DB
	DataDir   string
	Scheduler *schedule.Service
	Knowledge Knowledge
	Apps      Apps
	Agent     func(ctx context.Context) (conversations.Purger, error)
	CLI       CLIWorkers
	Browsers  Browsers
	Clock     func() time.Time
}

type Service struct {
	o Options

	mu        sync.Mutex
	apps      Apps
	agent     func(ctx context.Context) (conversations.Purger, error)
	providers func(ctx context.Context) error

	beforeSchedule func(user int64)

	stepTimeout time.Duration
}

func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Service{o: o, agent: o.Agent, apps: o.Apps}
}

func (s *Service) SetAgent(agent func(ctx context.Context) (conversations.Purger, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agent = agent
}

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

func (s *Service) Attach() error {
	return s.o.Scheduler.RegisterServer(Kind, s.handle)
}

func (s *Service) handle(ctx context.Context, j schedule.Job) error {
	return s.Purge(ctx, j.UserID)
}

func (s *Service) SchedulePurge(ctx context.Context, tx *sql.Tx, userID, workspaceID int64, at time.Time) error {
	_, err := s.o.Scheduler.Reschedule(ctx, tx, schedule.Job{Kind: Kind, UserID: userID, WorkspaceID: workspaceID, DedupeKey: jobKey, RunAt: at})
	return err
}

func (s *Service) CancelPurge(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := s.o.Scheduler.Cancel(ctx, tx, userID, Kind, jobKey)
	return err
}

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
				return nil
			}
			ws, err := s.workspaceOf(ctx, tx, user)
			if err != nil || ws == 0 {
				return err
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

const stampFormat = "2006-01-02T15:04:05.000Z"

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

type Result struct {
	PrivateSpaces       int64
	Conversations       int64
	AccessTokens        int64
	CLIAccounts         int64
	PushSubscriptions   int64
	DocumentsAnonymized int64
	MentionsAnonymized  int64
	UploadsRemoved      int64
	CleanupsPending     int64
}

func (s *Service) Purge(ctx context.Context, userID int64) error {
	acct, ok, err := s.begin(ctx, userID)
	if err != nil || !ok {
		return err
	}
	res := Result{}

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

	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, o := range owned {
			if err := s.record(ctx, tx, userID, cleanAgentConversation, strconv.FormatInt(o.AgentID, 10)); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, userID, cleanBrowser, strconv.FormatInt(userID, 10)); err != nil {
			return err
		}
		return s.recordCLIHomes(ctx, tx, userID)
	})
	if err != nil {
		return err
	}

	s.mu.Lock()
	apps := s.apps
	s.mu.Unlock()
	if apps == nil {
		return errors.New("the apps service isn't set yet: the purge waits for it, so what the apps keep isn't skipped")
	}
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
	if _, err := apps.PurgeUploadsNoting(ctx, userID, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE account_deletions SET uploads_removed = uploads_removed + 1 WHERE user_id = ?`, userID)
		return err
	}); err != nil {
		return err
	}

	if _, err := s.runCleanups(ctx, userID); err != nil {
		return err
	}

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
	var anonErrs []error
	for _, sp := range shared {
		if err := ctx.Err(); err != nil {
			return err
		}
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
	s.forgetGone(ctx, private)
	if finishErr != nil {
		return finishErr
	}
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

func (s *Service) finish(ctx context.Context, userID int64, private []access.Space, acct account, res *Result) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
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
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET assignee_deleted = 1 WHERE assignee_id = ?`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_effects WHERE user_id = ? AND state IN ('pending', 'running')`, userID); err != nil {
			return err
		}
		spaces, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE owner_id = ?`, userID)
		if err != nil {
			return err
		}
		if res.PrivateSpaces, err = spaces.RowsAffected(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE email = ?`, acct.email); err != nil {
			return err
		}
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID); err != nil {
			return err
		}
		return nil
	})
	return err
}

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
