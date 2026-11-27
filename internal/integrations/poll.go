package integrations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/paylink"
)

const (
	pollTick           = 5 * time.Second
	pollFirst          = time.Minute
	pollMax            = 30 * time.Minute
	pollFirstHooked    = 30 * time.Minute
	pollMaxHooked      = 6 * time.Hour
	maxPollAge         = 30 * 24 * time.Hour
	pollCallsPerMinute = 20
	pollRateWait       = time.Minute
	pollHold           = 5 * time.Minute
	pollLease          = 2 * time.Minute
	pollTimeout        = 30 * time.Second
	maxPollShift       = 20
)

func (s *Service) StartPolling(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(pollTick)
		defer tick.Stop()
		for {
			s.pollPass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func pollWait(reads int, hooked bool) time.Duration {
	first, most := pollFirst, pollMax
	if hooked {
		first, most = pollFirstHooked, pollMaxHooked
	}
	return min(first<<min(max(reads, 0), maxPollShift), most)
}

const pollDue = `r.state = 'open' AND r.provider_object_id IS NOT NULL AND i.disabled_at IS NULL AND r.account_mode = i.mode
	AND r.created_at > ? AND (r.next_poll_at <= ? OR (r.next_poll_at IS NULL AND r.created_at <= ?))`

func (s *Service) pollDueArgs(now time.Time) []any {
	return []any{s.store.stamp(now.Add(-maxPollAge)), s.store.stamp(now), s.store.stamp(now.Add(-pollFirst))}
}

func (s *Service) pollPass(ctx context.Context) {
	if s.currentApplier() == nil {
		return
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT DISTINCT i.id, i.workspace_id FROM integration_refs r
		JOIN integrations i ON i.id = r.integration_id WHERE `+pollDue+` ORDER BY i.id`, s.pollDueArgs(s.now())...)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("integrations: polling couldn't list the integrations that are due", "err", err)
		}
		return
	}
	type target struct{ id, workspace int64 }
	var due []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.workspace); err != nil {
			_ = rows.Close()
			return
		}
		due = append(due, t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return
	}
	for _, t := range due {
		if ctx.Err() != nil {
			return
		}
		s.pollIntegration(ctx, t.workspace, t.id)
	}
}

func (s *Service) pollIntegration(ctx context.Context, workspace, id int64) {
	if s.pollBudget(id, s.now()) == 0 {
		return
	}
	rec, err := s.store.getSnapshot(ctx, workspace, id)
	if err != nil {
		if !errors.Is(err, errNotFound) && ctx.Err() == nil {
			slog.Warn("integrations: polling couldn't read an integration", "integration", id, "err", err)
		}
		return
	}
	poller, ok := Lookup(rec.Provider).(PaymentPoller)
	if !ok {
		s.holdDue(ctx, id, pollHold)
		return
	}
	creds, err := s.store.credentials(ctx, workspace, id)
	if err != nil || creds.APIKey == "" {
		s.holdDue(ctx, id, pollHold)
		return
	}
	client := s.Client(Lookup(rec.Provider), rec.Settings, s.clientOpts...)
	hooked := s.publicURL != "" && creds.WebhookSecret != ""

	refs, err := s.dueRefs(ctx, id, s.pollBudget(id, s.now()))
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("integrations: polling couldn't list an integration's refs", "integration", id, "err", err)
		}
		return
	}
	for _, r := range refs {
		if ctx.Err() != nil {
			return
		}
		if r.accountMode != creds.Mode || !s.pollMayRead(ctx, workspace, rec.ID, r) {
			s.deferRef(ctx, r.id, pollHold)
			continue
		}
		if !s.takePollCall(id, s.now()) {
			return
		}
		if !s.claimPoll(ctx, r) {
			continue
		}
		s.pollRef(ctx, rec, creds, client, poller, r, hooked)
	}
}

func (s *Service) dueRefs(ctx context.Context, integration int64, limit int) ([]ref, error) {
	args := append(s.pollDueArgs(s.now()), integration, limit)
	rows, err := s.store.db.QueryContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id IN (
			SELECT r.id FROM integration_refs r JOIN integrations i ON i.id = r.integration_id WHERE `+pollDue+` AND r.integration_id = ?)
		ORDER BY coalesce(next_poll_at, created_at), id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []ref
	for rows.Next() {
		r, err := scanRef(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) pollMayRead(ctx context.Context, workspace, integration int64, r ref) bool {
	var space int64
	if err := s.store.db.QueryRowContext(ctx, `SELECT space_id FROM apps WHERE id = ?`, r.appID).Scan(&space); err != nil {
		return false
	}
	if ok, err := s.store.usable(ctx, workspace, integration, space); err != nil || !ok {
		return false
	}
	ap, _ := s.currentApplier().(*eventApplier)
	if ap == nil {
		return false
	}
	checker, ok := ap.records.(paylink.EventTargetChecker)
	if !ok {
		return true
	}
	bound, ok, err := checker.EventBinding(ctx, r.appID, r.slot)
	return err == nil && ok && bound == r.integrationID
}

func (s *Service) claimPoll(ctx context.Context, r ref) bool {
	now := s.now()
	res, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ?
		WHERE id = ? AND state = 'open' AND (next_poll_at <= ? OR (next_poll_at IS NULL AND created_at <= ?))`,
		s.store.stamp(now.Add(pollLease)), r.id, s.store.stamp(now), s.store.stamp(now.Add(-pollFirst)))
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1
}

func (s *Service) pollRef(ctx context.Context, rec record, creds Credentials, client *Client, poller PaymentPoller, r ref, hooked bool) {
	rctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	obs, err := poller.ReadPayment(rctx, creds, client, r.successURL == "", r.objectID)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.pollFailed(ctx, rec, r, hooked, err)
		return
	}
	switch {
	case len(obs.Payments) > 0:
		for _, p := range obs.Payments {
			link := ""
			if r.successURL == "" && p.ObjectID != r.objectID {
				link = r.objectID
			}
			s.pollTell(ctx, rec, r, Event{ID: "poll:" + p.ObjectID + ":paid", Kind: EventPaymentSucceeded, ObjectID: p.ObjectID, LinkID: link,
				PaymentID: p.PaymentID, AmountMinor: p.AmountMinor, Currency: p.Currency, Livemode: p.Livemode, OccurredAt: p.PaidAt})
		}
	case obs.Expired:
		s.pollTell(ctx, rec, r, Event{ID: "poll:" + r.objectID + ":expired", Kind: EventPaymentExpired, ObjectID: r.objectID,
			AmountMinor: r.amountMinor, Currency: r.currency, Livemode: obs.Livemode})
	}
	s.settlePoll(ctx, r, hooked)
}

func (s *Service) pollTell(ctx context.Context, rec record, r ref, ev Event) {
	ev.RefToken = s.pollToken(ctx, r)
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = s.now()
	}
	got, err := s.store.recordGuarded(ctx, rec.ID, &hookGuard{mode: rec.Mode}, ev, SourcePoll, []byte(ev.ID))
	switch {
	case errors.Is(err, errHookRefused):
		return
	case err != nil:
		if ctx.Err() == nil {
			slog.Warn("integrations: polling couldn't store what it found", "integration", rec.ID, "err", err)
		}
		return
	}
	if got == storedNew {
		slog.Info("integrations: polling found a change at the provider", "integration", rec.ID, "kind", ev.Kind)
		s.poke()
	}
}

func (s *Service) pollToken(ctx context.Context, r ref) string {
	var token string
	err := s.store.db.QueryRowContext(ctx, `SELECT token FROM integration_ref_attempts WHERE ref_id = ? AND object_id = ?
		ORDER BY seq DESC LIMIT 1`, r.id, r.objectID).Scan(&token)
	if err != nil {
		return r.token
	}
	return token
}

func (s *Service) pollFailed(ctx context.Context, rec record, r ref, hooked bool, err error) {
	ce, isCall := IsCall(err)
	if !isCall {
		s.settlePoll(ctx, r, hooked)
		return
	}
	logFailure(rec, ce)
	if ce.Status == http.StatusTooManyRequests {
		wait := min(max(ce.RetryAfter, pollRateWait), maxRetryAfter)
		s.blockPolls(rec.ID, s.now().Add(wait))
		s.deferRef(ctx, r.id, wait)
		return
	}
	s.settlePoll(ctx, r, hooked)
}

func (s *Service) settlePoll(ctx context.Context, r ref, hooked bool) {
	next := s.now().Add(pollWait(r.polls, hooked))
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ?, polls = polls + 1
		WHERE id = ? AND state = 'open'`, s.store.stamp(next), r.id); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't note a read", "ref", r.id, "err", err)
	}
}

func (s *Service) deferRef(ctx context.Context, id int64, d time.Duration) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ? WHERE id = ? AND state = 'open'`,
		s.store.stamp(s.now().Add(d)), id); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't defer a ref", "ref", id, "err", err)
	}
}

func (s *Service) holdDue(ctx context.Context, integration int64, d time.Duration) {
	now := s.now()
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ? WHERE id IN (
			SELECT r.id FROM integration_refs r JOIN integrations i ON i.id = r.integration_id WHERE `+pollDue+` AND r.integration_id = ?)`,
		append([]any{s.store.stamp(now.Add(d))}, append(s.pollDueArgs(now), integration)...)...); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't hold an integration's refs", "integration", integration, "err", err)
	}
}

type pollLimits struct {
	reads   map[int64][]time.Time
	blocked map[int64]time.Time
}

func (s *Service) pollWindow(id int64, now time.Time) []time.Time {
	if s.polls.reads == nil {
		s.polls.reads, s.polls.blocked = map[int64][]time.Time{}, map[int64]time.Time{}
	}
	var keep []time.Time
	for _, t := range s.polls.reads[id] {
		if now.Sub(t) < time.Minute && !t.After(now) {
			keep = append(keep, t)
		}
	}
	s.polls.reads[id] = keep
	return keep
}

func (s *Service) pollBudget(id int64, now time.Time) int {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	w := s.pollWindow(id, now)
	if until, ok := s.polls.blocked[id]; ok {
		if now.Before(until) {
			return 0
		}
		delete(s.polls.blocked, id)
	}
	return max(pollCallsPerMinute-len(w), 0)
}

func (s *Service) takePollCall(id int64, now time.Time) bool {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	w := s.pollWindow(id, now)
	if until, ok := s.polls.blocked[id]; ok && now.Before(until) {
		return false
	}
	if len(w) >= pollCallsPerMinute {
		return false
	}
	s.polls.reads[id] = append(w, now)
	return true
}

func (s *Service) blockPolls(id int64, until time.Time) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	s.pollWindow(id, s.now())
	if until.After(s.polls.blocked[id]) {
		s.polls.blocked[id] = until
	}
}
