package cliaccounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/conversations"
)

type ModelSetter func(ctx context.Context, agentConversation int64, cli cliworker.CLI) error

func (s *Service) SetModelSetter(f ModelSetter) { s.setModel = f }

const maxSwitches = 8

const limitedFor = time.Hour

func limitedUntil(limits []cliworker.Limit, limitsAt *string, now time.Time) (bool, time.Time) {
	var read time.Time
	if limitsAt != nil {
		read, _ = time.Parse(time.RFC3339Nano, *limitsAt)
	}
	limited, until := false, time.Time{}
	for _, l := range limits {
		reached := l.Status == "rejected" || (l.UsedPercent != nil && *l.UsedPercent >= 100)
		if !reached {
			continue
		}
		switch {
		case l.ResetsAt != nil && l.ResetsAt.After(now):
			limited = true
			if l.ResetsAt.After(until) {
				until = *l.ResetsAt
			}
		case l.ResetsAt == nil && !read.IsZero() && now.Sub(read) < limitedFor:
			limited = true
		}
	}
	return limited, until
}

func parseLimits(raw sql.NullString) []cliworker.Limit {
	var out []cliworker.Limit
	if raw.Valid {
		_ = json.Unmarshal([]byte(raw.String), &out)
	}
	return out
}

func nullString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func (c choice) limited(now time.Time) (bool, time.Time) {
	if c.shared != nil {
		return limitedUntil(c.shared.Limits, c.shared.LimitsAt, now)
	}
	return limitedUntil(parseLimits(c.row.limits), nullString(c.row.limitsAt), now)
}

func (c choice) key() string {
	if c.shared != nil {
		return "s" + strconv.FormatInt(c.shared.ID, 10)
	}
	return "a" + strconv.FormatInt(c.row.id, 10)
}

func (c choice) cli() cliworker.CLI {
	if c.shared != nil {
		return c.shared.CLI
	}
	return c.row.ref.cli
}

func (c choice) title() string {
	if c.shared != nil {
		return c.shared.Name + " (" + c.shared.Label + ", this workspace's)"
	}
	return accountTitle(c.row)
}

func accountTitle(r row) string {
	switch {
	case r.label != "":
		return r.ref.cli.Name() + " (" + r.label + ")"
	case r.ref.number() > 1:
		return r.ref.cli.Name() + " (account " + strconv.Itoa(r.ref.number()) + ")"
	}
	return r.ref.cli.Name()
}

func (s *Service) switchingOn(ctx context.Context, user int64) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT coalesce((SELECT enabled FROM cli_switching WHERE user_id = ?), 0)`, user).Scan(&on)
	return on, err
}

type queueEntry struct {
	account, shared int64
}

func (s *Service) queue(ctx context.Context, user int64) ([]queueEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(account_id, 0), coalesce(shared_account_id, 0) FROM cli_switch_queue
		WHERE user_id = ? ORDER BY position`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueEntry
	for rows.Next() {
		var e queueEntry
		if err := rows.Scan(&e.account, &e.shared); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) byQueue(ctx context.Context, user int64, own []row, shared []sharedRow) error {
	q, err := s.queue(ctx, user)
	if err != nil || len(q) == 0 {
		return err
	}
	ownAt, sharedAt := map[int64]int{}, map[int64]int{}
	for i, e := range q {
		if e.account != 0 {
			ownAt[e.account] = i + 1
		} else {
			sharedAt[e.shared] = i + 1
		}
	}
	at := func(m map[int64]int, id int64) int {
		if p := m[id]; p > 0 {
			return p
		}
		return len(q) + 1
	}
	sort.SliceStable(own, func(i, j int) bool { return at(ownAt, own[i].id) < at(ownAt, own[j].id) })
	sort.SliceStable(shared, func(i, j int) bool { return at(sharedAt, shared[i].ID) < at(sharedAt, shared[j].ID) })
	return nil
}

func (s *Service) candidates(ctx context.Context, p conversations.Principal) ([]choice, error) {
	q, err := s.queue(ctx, p.User)
	if err != nil || len(q) == 0 {
		return nil, err
	}
	mode, err := s.modeOf(ctx, p.Workspace, p.User)
	if err != nil {
		return nil, err
	}
	unusable, _, err := s.sharedUnusable(ctx)
	if err != nil {
		return nil, err
	}
	var out []choice
	for _, e := range q {
		switch {
		case e.account != 0 && mode.usesOwn():
			r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts
				WHERE id = ? AND user_id = ? AND slot IS NOT NULL`, e.account, p.User))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if r.state == Connected && s.worker.Available(r.ref.cli) {
				out = append(out, choice{row: r})
			}
		case e.shared != 0 && mode.usesShared() && unusable == "":
			a, ok, err := s.sharedAccount(ctx, p.Workspace, e.shared)
			if err != nil {
				return nil, err
			}
			if !ok || !s.worker.Available(a.CLI) {
				continue
			}
			r, err := s.usageRow(ctx, p.User, a)
			if err != nil {
				return nil, err
			}
			out = append(out, choice{row: r, shared: &a})
		}
	}
	return out, nil
}

func (s *Service) startChoice(ctx context.Context, p conversations.Principal, cli cliworker.CLI, c choice) (choice, string, error) {
	cands, err := s.candidates(ctx, p)
	if err != nil || len(cands) == 0 {
		return c, "", err
	}
	now := time.Now()
	last, err := s.lastAccount(ctx, p.Conversation)
	if err != nil {
		return c, "", err
	}
	for _, k := range cands {
		if last != 0 && k.row.id == last && k.cli() == cli && k.key() != c.key() {
			if limited, _ := k.limited(now); !limited {
				return k, "", nil
			}
		}
	}
	limited, until := c.limited(now)
	if !limited {
		return c, "", nil
	}
	pickFrom := func(sameCLI bool) (choice, bool) {
		for _, k := range cands {
			if k.key() == c.key() || (sameCLI && k.cli() != cli) {
				continue
			}
			if l, _ := k.limited(now); !l {
				return k, true
			}
		}
		return choice{}, false
	}
	next, ok := pickFrom(true)
	if !ok {
		next, ok = pickFrom(false)
	}
	if !ok {
		return c, "", nil
	}
	return next, fmt.Sprintf("%s is at its usage limit%s: this turn runs on %s", c.title(), untilText(until), next.title()), nil
}

func (s *Service) nextChoice(ctx context.Context, p conversations.Principal, tried map[string]bool) (choice, bool, error) {
	cands, err := s.candidates(ctx, p)
	if err != nil {
		return choice{}, false, err
	}
	now := time.Now()
	for _, k := range cands {
		if tried[k.key()] {
			continue
		}
		if l, _ := k.limited(now); !l {
			return k, true, nil
		}
	}
	return choice{}, false, nil
}

func untilText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " until " + t.UTC().Format("2006-01-02 15:04 UTC")
}

type QueueAccount struct {
	Kind          string        `json:"kind"`
	ID            string        `json:"id"`
	CLI           cliworker.CLI `json:"cli"`
	Title         string        `json:"title"`
	Label         string        `json:"label,omitempty"`
	Number        int           `json:"number,omitempty"`
	Workspace     int64         `json:"workspace,omitempty"`
	WorkspaceName string        `json:"workspace_name,omitempty"`
	Limited       bool          `json:"limited"`
	LimitedUntil  string        `json:"limited_until,omitempty"`
}

type switchingState struct {
	Enabled   bool           `json:"enabled"`
	Queue     []QueueAccount `json:"queue"`
	Available []QueueAccount `json:"available"`
}

func (s *Service) SwitchingRoutes(mux *http.ServeMux, a interface {
	RequireUser(http.Handler) http.Handler
},
) {
	mux.Handle("GET /api/me/cli-switching", a.RequireUser(http.HandlerFunc(s.handleSwitching)))
	mux.Handle("PUT /api/me/cli-switching", a.RequireUser(http.HandlerFunc(s.handleSetSwitching)))
}

type queueable struct {
	list   []QueueAccount
	own    map[string]int64
	byRow  map[int64]QueueAccount
	shared map[int64]QueueAccount
}

func (s *Service) available(ctx context.Context, user int64) (queueable, error) {
	now := time.Now()
	out := queueable{list: []QueueAccount{}, own: map[string]int64{}, byRow: map[int64]QueueAccount{}, shared: map[int64]QueueAccount{}}
	own, err := s.own(ctx, user, "")
	if err != nil {
		return out, err
	}
	for _, r := range own {
		q := QueueAccount{Kind: "own", ID: r.ref.handle(), CLI: r.ref.cli, Title: accountTitle(r), Label: r.label, Number: r.ref.number()}
		q.Limited, q.LimitedUntil = limitFields(limitedUntil(parseLimits(r.limits), nullString(r.limitsAt), now))
		out.list = append(out.list, q)
		out.own[q.ID], out.byRow[r.id] = r.id, q
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.workspace_id, a.cli, a.label, a.limits, a.limits_at, w.name
		FROM cli_shared_accounts a
		JOIN memberships m ON m.workspace_id = a.workspace_id AND m.user_id = ?
		JOIN workspaces w ON w.id = a.workspace_id ORDER BY a.cli, w.name, a.id`, user)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ws int64
		var cli, label, wsName string
		var limits, limitsAt sql.NullString
		if err := rows.Scan(&id, &ws, &cli, &label, &limits, &limitsAt, &wsName); err != nil {
			return out, err
		}
		c := cliworker.CLI(cli)
		q := QueueAccount{
			Kind: "shared", ID: strconv.FormatInt(id, 10), CLI: c, Workspace: ws, WorkspaceName: wsName,
			Title: c.Name() + " (" + label + ", " + wsName + ")", Label: label,
		}
		q.Limited, q.LimitedUntil = limitFields(limitedUntil(parseLimits(limits), nullString(limitsAt), now))
		out.list = append(out.list, q)
		out.shared[id] = q
	}
	return out, rows.Err()
}

func limitFields(limited bool, until time.Time) (bool, string) {
	if !limited || until.IsZero() {
		return limited, ""
	}
	return true, until.UTC().Format(time.RFC3339)
}

func (s *Service) handleSwitching(w http.ResponseWriter, r *http.Request) {
	ctx, user := r.Context(), caller(r)
	on, err := s.switchingOn(ctx, user)
	if err != nil {
		serverError(w, r, err)
		return
	}
	avail, err := s.available(ctx, user)
	if err != nil {
		serverError(w, r, err)
		return
	}
	q, err := s.queue(ctx, user)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := switchingState{Enabled: on, Queue: []QueueAccount{}, Available: avail.list}
	for _, e := range q {
		if a, ok := avail.byRow[e.account]; ok && e.account != 0 {
			out.Queue = append(out.Queue, a)
		} else if a, ok := avail.shared[e.shared]; ok && e.shared != 0 {
			out.Queue = append(out.Queue, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

const maxQueue = 32

func (s *Service) handleSetSwitching(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
		Queue   *[]struct {
			Account string `json:"account"`
			Shared  int64  `json:"shared"`
		} `json:"queue"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, user := r.Context(), caller(r)
	var entries []queueEntry
	if req.Queue != nil {
		if len(*req.Queue) > maxQueue {
			writeError(w, http.StatusBadRequest, apierr.CodeQueueTooLong, fmt.Sprintf("queue: at most %d accounts", maxQueue), apierr.Params{"max": maxQueue})
			return
		}
		avail, err := s.available(ctx, user)
		if err != nil {
			serverError(w, r, err)
			return
		}
		seen := map[queueEntry]bool{}
		for _, q := range *req.Queue {
			var e queueEntry
			switch {
			case q.Account != "" && q.Shared == 0:
				e.account = avail.own[q.Account]
			case q.Account == "" && q.Shared > 0:
				if _, ok := avail.shared[q.Shared]; ok {
					e.shared = q.Shared
				}
			default:
				writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `queue: each entry is {"account": "<handle>"} or {"shared": <id>}`)
				return
			}
			if e == (queueEntry{}) {
				writeError(w, http.StatusNotFound, apierr.CodeQueueAccountNotFound, "queue: an account that isn't one of yours, or of your workspaces")
				return
			}
			if seen[e] {
				writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "queue: an account appears twice")
				return
			}
			seen[e] = true
			entries = append(entries, e)
		}
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if req.Enabled != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO cli_switching (user_id, enabled) VALUES (?, ?)
				ON CONFLICT (user_id) DO UPDATE SET enabled = excluded.enabled`, user, *req.Enabled); err != nil {
				return err
			}
		}
		if req.Queue == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cli_switch_queue WHERE user_id = ?`, user); err != nil {
			return err
		}
		for i, e := range entries {
			var account, shared any
			if e.account != 0 {
				account = e.account
			} else {
				shared = e.shared
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO cli_switch_queue (user_id, position, account_id, shared_account_id)
				VALUES (?, ?, ?, ?)`, user, i+1, account, shared); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.handleSwitching(w, r)
}
