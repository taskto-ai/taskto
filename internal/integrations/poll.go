package integrations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/paylink"
)

// Reconciliation polling of open payments (docs/integrations.md#reconciliation-polling).
//
// A server with no public address can't receive a provider's webhooks, and one that has an address
// can miss one. Its worker reads, from time to time, the objects the server made for the refs that
// are still open, and feeds what it finds through the very path a webhook's event takes: a synthetic
// event ("poll:<object>:<state>") is stored in integration_events like any other, once per integration
// (the unique event id), and the event worker's apply step (apply.go) moves the ref and writes the
// record. The ref's conditional state change is the real guard, so a payment that the webhook, the
// poll or both tell is applied exactly once. Nothing here changes a ref or a record itself.
//
// What it will and won't do:
//
//   - It reads only (ReadPayment: retrieve a session, list a link's sessions) and never makes, changes
//     or deletes anything at the provider. It never reads with a key of another mode than the ref's:
//     a ref belongs to the mode of the key it was made with, and an integration whose key changed mode
//     leaves such refs alone (nothing is read for them).
//   - It reads a ref's object with the key of the ref's own integration, in that integration's
//     workspace, and for nothing else: the ref is found by its integration, never by what the provider
//     says.
//   - It is polite. Each ref waits longer after every read (pollFirst, doubling to pollMax; much
//     longer where webhooks arrive too), an integration is read at most pollCallsPerMinute times a
//     minute, and a 429 stops all of its reads for the wait Stripe asked for (Retry-After, at least
//     pollRateWait).
//   - It stops by itself. A ref that is paid, expired, failed or refunded is not open and is never
//     read; one older than maxPollAge isn't read any more either (a Payment Link never expires on
//     its own, so a record whose payer never paid stops costing calls after that time).
//   - It respects what a person switched off: an integration that is disabled is not read for, and a
//     ref whose app is offline, whose approval was revoked or whose integration is no longer allowed
//     in the app's space is only looked at again later (pollHold), with nothing sent to the provider.
//     The apply step checks all of it again when it applies the event.
//   - It runs on a worker of its own (StartPolling), in the server process, whether or not the
//     server has a public address.
const (
	// pollTick is how often the worker looks for refs that are due.
	pollTick = 5 * time.Second
	// pollFirst is how long after its creation a ref is read for the first time, and the first wait
	// where no webhook arrives; the wait doubles with each read up to pollMax.
	pollFirst = time.Minute
	pollMax   = 30 * time.Minute
	// Where the provider's webhooks arrive as well (the server has a public address and the integration
	// a webhook secret) the poll is a safety net, and waits longer.
	pollFirstHooked = 30 * time.Minute
	pollMaxHooked   = 6 * time.Hour
	// maxPollAge is how long after its creation a ref is still read.
	maxPollAge = 30 * 24 * time.Hour
	// pollCallsPerMinute is the most reads an integration makes in a minute, whatever the number of its
	// open refs. Stripe's own limit is far above it (https://docs.stripe.com/rate-limits).
	pollCallsPerMinute = 20
	// pollRateWait is how long an integration waits after a 429 that named no time.
	pollRateWait = time.Minute
	// pollHold is how long a ref waits that can't be read now (its app or integration is off, or the
	// provider can't read it), counted against nothing.
	pollHold = 5 * time.Minute
	// pollLease is how long a ref taken by a pass is not taken again if the pass never settled it.
	pollLease = 2 * time.Minute
	// pollTimeout bounds one read.
	pollTimeout = 30 * time.Second
	// maxPollShift keeps the doubling from overflowing.
	maxPollShift = 20
)

// StartPolling starts the worker that reads open payments at their providers, and returns the function
// that stops it and waits for it. It runs in the server process, which the data directory has one of
// (package lockfile). Without an apply step (UseRecords) nothing is read: what it found could not be
// applied.
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

// pollWait is how long a ref waits after a read, given how many reads were made before it.
func pollWait(reads int, hooked bool) time.Duration {
	first, most := pollFirst, pollMax
	if hooked {
		first, most = pollFirstHooked, pollMaxHooked
	}
	return min(first<<min(max(reads, 0), maxPollShift), most)
}

// pollDue is the condition of a ref that is read now: open with an object, of the mode its integration's
// key has, in an integration that is on, not older than maxPollAge, and due (its own time has come, or
// it was never read and is pollFirst old). The arguments are pollDueArgs'.
const pollDue = `r.state = 'open' AND r.provider_object_id IS NOT NULL AND i.disabled_at IS NULL AND r.account_mode = i.mode
	AND r.created_at > ? AND (r.next_poll_at <= ? OR (r.next_poll_at IS NULL AND r.created_at <= ?))`

func (s *Service) pollDueArgs(now time.Time) []any {
	return []any{s.store.stamp(now.Add(-maxPollAge)), s.store.stamp(now), s.store.stamp(now.Add(-pollFirst))}
}

// pollPass reads the refs that are due, integration by integration.
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

// pollIntegration reads the due refs of one integration, within its budget of reads.
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
		s.holdDue(ctx, id, pollHold) // no key, or one the server's key can't open
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
		// A ref reads with its own mode's key only: the integration's key is another mode now.
		if r.accountMode != creds.Mode || !s.pollMayRead(ctx, workspace, rec.ID, r) {
			s.deferRef(ctx, r.id, pollHold)
			continue
		}
		if !s.takePollCall(id, s.now()) {
			return // out of reads for now, or the provider asked for a wait: the rest stay due
		}
		if !s.claimPoll(ctx, r) {
			continue
		}
		s.pollRef(ctx, rec, creds, client, poller, r, hooked)
	}
}

// dueRefs lists the integration's refs that are due, the longest waiting first, at most limit.
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

// pollMayRead says whether the ref's app may take events now: the integration is on and allowed in the
// app's space, and the app is online with its approved contract bound to this very integration. It
// only decides whether to ask the provider; the apply step checks again. A check that fails to run
// says no, so nothing is read on a guess.
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

// claimPoll takes the ref for a read: its next time moves out by pollLease, so no other pass reads it
// meanwhile, and a pass that never settled it finds it due again. It says false when the ref isn't due
// any more.
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

// pollRef reads the ref's object and tells what it found.
func (s *Service) pollRef(ctx context.Context, rec record, creds Credentials, client *Client, poller PaymentPoller, r ref, hooked bool) {
	rctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	obs, err := poller.ReadPayment(rctx, creds, client, r.successURL == "", r.objectID)
	if ctx.Err() != nil {
		return // stopping: the claim runs out and the ref is read again
	}
	if err != nil {
		s.pollFailed(ctx, rec, r, hooked, err)
		return
	}
	switch {
	case len(obs.Payments) > 0:
		// A paid object is told even when it ended another way too: money is never dropped.
		for _, p := range obs.Payments {
			// A hosted link is paid through a session of its own: the event names the session, and the
			// link it came from, as a webhook's event of it does (Event.LinkID).
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

// pollTell stores what a read found as an event of the integration, with the ref's token for the key
// whose object was read, and wakes the event worker. A second poll of the same state, or the webhook's
// event for it, is another row or the same one: the apply step is the one that makes it once.
//
// A payment is dated as a webhook's event of it is, by the provider's own time of the payment
// (ObservedPayment.PaidAt, in ev.OccurredAt): one made at 23:59:30 and found at 00:00:10 is dated
// 23:59:30, the day it was made. Only an event the provider says no time of (an expiry, or a payment
// of a provider that says none) is dated by when the poll found it.
func (s *Service) pollTell(ctx context.Context, rec record, r ref, ev Event) {
	ev.RefToken = s.pollToken(ctx, r)
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = s.now()
	}
	got, err := s.store.recordGuarded(ctx, rec.ID, &hookGuard{mode: rec.Mode}, ev, SourcePoll, []byte(ev.ID))
	switch {
	case errors.Is(err, errHookRefused):
		return // switched off since it was read
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

// pollToken is the token of the key whose object the ref holds: the one the apply step finds the ref
// by, and the key it settles. A ref's first key's token is the ref's own.
func (s *Service) pollToken(ctx context.Context, r ref) string {
	var token string
	err := s.store.db.QueryRowContext(ctx, `SELECT token FROM integration_ref_attempts WHERE ref_id = ? AND object_id = ?
		ORDER BY seq DESC LIMIT 1`, r.id, r.objectID).Scan(&token)
	if err != nil {
		return r.token
	}
	return token
}

// pollFailed notes a read that failed: a 429 holds the whole integration for the wait it asked, and
// counts as no read of the ref; anything else is a read that found nothing and the ref waits longer.
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

// settlePoll notes a read of the ref, and when it is read next.
func (s *Service) settlePoll(ctx context.Context, r ref, hooked bool) {
	next := s.now().Add(pollWait(r.polls, hooked))
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ?, polls = polls + 1
		WHERE id = ? AND state = 'open'`, s.store.stamp(next), r.id); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't note a read", "ref", r.id, "err", err)
	}
}

// deferRef moves the ref's next read out by d, counting it as no read.
func (s *Service) deferRef(ctx context.Context, id int64, d time.Duration) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ? WHERE id = ? AND state = 'open'`,
		s.store.stamp(s.now().Add(d)), id); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't defer a ref", "ref", id, "err", err)
	}
}

// holdDue defers every ref of the integration that is due, for a reason that is the integration's.
func (s *Service) holdDue(ctx context.Context, integration int64, d time.Duration) {
	now := s.now()
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET next_poll_at = ? WHERE id IN (
			SELECT r.id FROM integration_refs r JOIN integrations i ON i.id = r.integration_id WHERE `+pollDue+` AND r.integration_id = ?)`,
		append([]any{s.store.stamp(now.Add(d))}, append(s.pollDueArgs(now), integration)...)...); err != nil && ctx.Err() == nil {
		slog.Warn("integrations: polling couldn't hold an integration's refs", "integration", integration, "err", err)
	}
}

// ----- the integration's budget of reads -----

// pollLimits is what the polling remembers of each integration's reads, in memory: the times of the
// reads of the last minute, and until when a 429 holds them. After a restart the budget is whole
// again, which a restart is rare enough for.
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

// pollBudget is how many reads the integration may still make now.
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

// takePollCall spends one read of the integration's budget, and says false when there is none.
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

// blockPolls holds every read of the integration until the time.
func (s *Service) blockPolls(id int64, until time.Time) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	s.pollWindow(id, s.now())
	if until.After(s.polls.blocked[id]) {
		s.polls.blocked[id] = until
	}
}
