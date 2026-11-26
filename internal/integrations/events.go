package integrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// Event states, as the outbox's entries': pending waits for the worker, running is leased to one,
// applied and ignored are done, and failed has given up (or was refused by the apply step) and
// waits for an admin.
const (
	EventPending = "pending"
	EventRunning = "running"
	EventApplied = "applied"
	// EventIgnored is an event that is real and verified but changes nothing: a duplicate or a
	// late event, found out by the ref's state having moved.
	EventIgnored = "ignored"
	EventFailed  = "failed"
)

// Where an event came from.
const (
	SourceWebhook = "webhook"
	SourcePoll    = "poll"
)

// Bounds and timings of the event worker.
const (
	// maxEventAttempts bounds the tries of an event before it fails for good.
	maxEventAttempts = 5
	// eventLease is how long a worker owns the event it took: after it, a worker that died has
	// lost it.
	eventLease = 2 * time.Minute
	// eventTimeout bounds one try.
	eventTimeout = time.Minute
	eventPoll    = 2 * time.Second
	// How long finished events are kept, and how long any event that has stopped moving is.
	keepEventsDone = 30 * 24 * time.Hour
	keepEventsAll  = 90 * 24 * time.Hour
	// eventBackoffBase is the wait after the first failed try, doubled for each next.
	eventBackoffBase = 5 * time.Second
	eventBackoffMax  = 5 * time.Minute
)

// StoredEvent is an event as the worker gets it: what the webhook route kept, and where it came
// from. It holds no body.
type StoredEvent struct {
	ID            int64
	IntegrationID int64
	WorkspaceID   int64
	ProviderEvent string
	Source        string
	Event
	// Attempts counts the tries, this one included.
	Attempts int
	// Lease is the number of the claim this try runs under: only it may give the event its end.
	Lease int64
	// ReceivedAt is when the server took the event in.
	ReceivedAt time.Time
}

// Applier applies one stored event: the next card's step. It returns nil when the event was
// applied, Ignore(code) for one that changes nothing, and a *EventError for one that failed. Any
// other error is a failure that may pass, tried again. It must be safe to run twice for one
// event, since a worker that dies after the change and before noting it is followed by another.
type Applier interface {
	ApplyEvent(ctx context.Context, ev StoredEvent) error
}

// EventError is why an event wasn't applied, as a code to tell admins. Transient is for a
// failure that may pass, which is tried again; the others fail at once and wait for an admin.
type EventError struct {
	Code      apierr.Code
	Params    apierr.Params
	Transient bool
	// ignored marks an outcome that is not a failure (see Ignore).
	ignored bool
	// wait, when set, is an event that can't be applied yet and may be later (see waitEvent).
	wait time.Duration
}

func (e *EventError) Error() string { return string(e.Code) }

// Ignore is the Applier's answer for an event that is valid and changes nothing, with the code
// that says why (a duplicate, an event that came late).
func Ignore(code apierr.Code, params ...apierr.Params) error {
	e := &EventError{Code: code, ignored: true}
	if len(params) > 0 {
		e.Params = params[0]
	}
	return e
}

// waitEvent is the Applier's answer for an event that can't be applied yet and may be once another
// event is: it is tried again after d, and the try counts against none of its attempts. The applier
// bounds how long an event may wait, and ends it with a code the admins are told of after that.
func waitEvent(code apierr.Code, d time.Duration) error {
	return &EventError{Code: code, Transient: true, wait: max(d, time.Second)}
}

// accepted is what a verified delivery became.
type accepted int

const (
	// storedNew: the event is new and waits for the worker.
	storedNew accepted = iota
	// storedAgain: the integration had the event already.
	storedAgain
)

// record stores a verified event for the integration, once: a second delivery of the same event
// id finds the row and changes nothing (ON CONFLICT DO NOTHING), so a replay is answered like the
// first delivery. last_event_at moves for every verified delivery, a replay too: it tells an admin
// the webhook arrives.
func (s *Store) record(ctx context.Context, integration int64, ev Event, source string, body []byte) (accepted, error) {
	return s.recordGuarded(ctx, integration, nil, ev, source, body)
}

// errHookRefused is a delivery that verified but may not be stored any more: the integration was
// disabled, or its mode or webhook secrets changed, after the route looked it up. It is answered
// like every other refusal.
var errHookRefused = errors.New("the integration changed since the delivery was verified")

// hookGuard is what the route saw of the integration when it verified a delivery: the write
// checks it again inside its own transaction, so a delivery that was in flight when an admin
// disabled the integration or rotated its secret stores nothing. A poll's guard (poll.go) has no
// revision: it checks the integration and its mode only, since it used no webhook secret.
type hookGuard struct {
	mode     string
	revision string
}

// check reads the integration in tx and says errHookRefused unless it is still enabled, of the
// same mode, with the webhook secrets the delivery was verified against.
func (g *hookGuard) check(ctx context.Context, s *Store, tx *sql.Tx, integration int64) error {
	if g == nil {
		return nil
	}
	var disabled bool
	var mode string
	err := tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL, mode FROM integrations WHERE id = ?`, integration).Scan(&disabled, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return errHookRefused
	}
	if err != nil {
		return err
	}
	if disabled || mode != g.mode {
		return errHookRefused
	}
	if g.revision == "" {
		return nil
	}
	rev, err := s.credentialRevision(ctx, tx, integration)
	if err != nil {
		return err
	}
	if rev != g.revision {
		return errHookRefused
	}
	return nil
}

// recordGuarded is record for a delivery that was verified against the state guard describes (nil
// for events that didn't come in through the route).
func (s *Store) recordGuarded(ctx context.Context, integration int64, guard *hookGuard, ev Event, source string, body []byte) (accepted, error) {
	sum := sha256.Sum256(body)
	at := s.now()
	result := storedNew
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := guard.check(ctx, s, tx, integration); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO integration_events
			(integration_id, provider_event_id, source, kind, ref_token, object_id, provider_payment_id, amount_minor, currency, livemode,
			 body_sha256, occurred_at, received_at, state, next_attempt_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
			ON CONFLICT (integration_id, provider_event_id) DO NOTHING`,
			integration, ev.ID, source, ev.Kind, nullString(ev.RefToken), nullString(ev.ObjectID), nullString(ev.PaymentID),
			ev.AmountMinor, ev.Currency, ev.Livemode, hex.EncodeToString(sum[:]), s.stamp(ev.OccurredAt), s.stamp(at), s.stamp(at))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			result = storedAgain
		}
		// last_event_at tells an admin that the provider's webhook arrives: a poll's event says nothing of it.
		if source == SourcePoll {
			return nil
		}
		return s.touchEvent(ctx, tx, integration, at)
	})
	return result, err
}

// touchEvent notes that a verified event arrived for the integration, with no event to store (a
// type the server doesn't act on).
func (s *Store) touchEvent(ctx context.Context, tx *sql.Tx, integration int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE integrations SET last_event_at = ? WHERE id = ?`, s.stamp(at), integration)
	return err
}

func (s *Store) noteEvent(ctx context.Context, integration int64, guard *hookGuard) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := guard.check(ctx, s, tx, integration); err != nil {
			return err
		}
		return s.touchEvent(ctx, tx, integration, s.now())
	})
}

// credentialRevision is a digest of the integration's sealed webhook secrets, current and
// previous: it changes whenever either is set, replaced or dropped, and says nothing of them.
func (s *Store) credentialRevision(ctx context.Context, q querier, integration int64) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, sealed, coalesce(expires_at, '') FROM integration_secrets
		WHERE integration_id = ? AND name IN (?, ?) ORDER BY name`, integration, SecretWebhookSecret, SecretWebhookSecretPrevious)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for rows.Next() {
		var name, expires string
		var sealed []byte
		if err := rows.Scan(&name, &sealed, &expires); err != nil {
			_ = rows.Close()
			return "", err
		}
		for _, part := range [][]byte{[]byte(name), sealed, []byte(expires)} {
			var n [8]byte
			binary.BigEndian.PutUint64(n[:], uint64(len(part)))
			h.Write(n[:])
			h.Write(part)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// tx runs fn in a write transaction with no actor behind it: the webhook and the worker act for
// nobody.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
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
	return tx.Commit()
}

// hookTarget is what the webhook route needs to know of the integration behind a public id.
type hookTarget struct {
	id        int64
	workspace int64
	provider  string
	mode      string
	disabled  bool
}

// byPublicID finds the integration with the public id, disabled or not: the route does the same
// work for a disabled one as for an enabled one, and refuses it afterwards.
func (s *Store) byPublicID(ctx context.Context, publicID string) (hookTarget, error) {
	var t hookTarget
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, provider, mode, disabled_at IS NOT NULL FROM integrations
		WHERE public_id = ?`, publicID).Scan(&t.id, &t.workspace, &t.provider, &t.mode, &t.disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return hookTarget{}, errNotFound
	}
	return t, err
}

// SetApplier sets the step that applies events. Until there is one the worker leaves events
// pending, untouched and uncounted, and still purges the old ones.
func (s *Service) SetApplier(a Applier) { s.applier.Store(&a) }

func (s *Service) currentApplier() Applier {
	if p := s.applier.Load(); p != nil {
		return *p
	}
	return nil
}

// StartEvents starts the worker that applies stored events, and returns the function that stops it
// and waits for it. It runs in the server process, which the data directory has one of (package
// lockfile). A webhook that stored an event wakes it; it also looks every few seconds, for events
// whose lease ran out and ones waiting to be retried.
func (s *Service) StartEvents(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poll := time.NewTicker(eventPoll)
		defer poll.Stop()
		tidy := time.NewTicker(time.Hour)
		defer tidy.Stop()
		for {
			s.drainEvents(ctx)
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			case <-s.wake:
			case <-tidy.C:
				if err := s.purgeEvents(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("integrations: purging old events failed", "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// poke wakes the worker, if it runs in this process.
func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drainEvents does the events that are due, one after another, and then sends the admins'
// notifications that are owed.
func (s *Service) drainEvents(ctx context.Context) {
	defer s.sendEventNotices(ctx)
	for done := 1; ctx.Err() == nil; done++ {
		// A queue that stays busy never keeps the admins waiting: the notifications owed get a turn
		// between events too.
		if done%noticeTurn == 0 {
			s.sendEventNotices(ctx)
		}
		a := s.currentApplier()
		if a == nil {
			return
		}
		ok, err := s.runEvent(ctx, a)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("integrations: event worker", "err", err)
			}
			return
		}
		if !ok {
			return
		}
	}
}

// purgeEvents removes the events nobody needs any more: finished ones after keepEventsDone, and
// whatever has stopped moving (a failed one that nobody retried) after keepEventsAll. An event
// that waits or runs is never removed: it is the worker's. A purged event is forgotten, so the
// provider's retry of it, signed afresh, is stored again: whoever applies events must be safe
// against an event it has already seen (docs/integrations.md#events). A recorded delivery replayed
// as it was is still refused, since its signature is older than the tolerance.
func (s *Service) purgeEvents(ctx context.Context) error {
	t := s.now()
	_, err := s.store.db.ExecContext(ctx, `DELETE FROM integration_events
		WHERE ((state IN ('applied', 'ignored') AND received_at < ?) OR (state = 'failed' AND received_at < ?))
			AND NOT EXISTS (SELECT 1 FROM integration_event_notices n WHERE n.event_id = integration_events.id)`,
		s.store.stamp(t.Add(-keepEventsDone)), s.store.stamp(t.Add(-keepEventsAll)))
	return err
}

// claimEvent takes the next event that is due: one that waits, or one whose worker's lease has run
// out. An event whose lease ran out after its last permitted try isn't tried again: it has failed.
func (s *Service) claimEvent(ctx context.Context) (ev StoredEvent, lease int64, found bool, err error) {
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		t := s.now()
		at := s.store.stamp(t)
		for {
			var id int64
			var attempts int
			err := tx.QueryRowContext(ctx, `SELECT id, attempts FROM integration_events
				WHERE (state = 'pending' AND next_attempt_at <= ?) OR (state = 'running' AND lease_until <= ?)
				ORDER BY id LIMIT 1`, at, at).Scan(&id, &attempts)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if attempts >= maxEventAttempts {
				// Its worker died (or kept being stopped) on the last try: the bound holds. The admins are
				// owed a notification, which sendEventNotices writes: one that can't be written never holds
				// this event, or the ones after it, back.
				if _, err := tx.ExecContext(ctx, `UPDATE integration_events
					SET state = 'failed', lease_until = NULL, error_code = ?, error_params = NULL, processed_at = ? WHERE id = ?`,
					string(apierr.CodeIntegrationEventGaveUp), at, id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO integration_event_notices (event_id, lease, due_at)
					SELECT id, lease, ? FROM integration_events WHERE id = ? ON CONFLICT DO NOTHING`, at, id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integration_events
				SET state = 'running', attempts = attempts + 1, lease = lease + 1, lease_until = ? WHERE id = ?`,
				s.store.stamp(t.Add(eventLease)), id); err != nil {
				return err
			}
			var occurred, received string
			err = tx.QueryRowContext(ctx, `SELECT e.id, e.integration_id, i.workspace_id, e.provider_event_id, e.source, e.kind,
					coalesce(e.ref_token, ''), coalesce(e.object_id, ''), coalesce(e.provider_payment_id, ''),
					e.amount_minor, e.currency, e.livemode, e.occurred_at, e.received_at, e.attempts, e.lease
				FROM integration_events e JOIN integrations i ON i.id = e.integration_id WHERE e.id = ?`, id).
				Scan(&ev.ID, &ev.IntegrationID, &ev.WorkspaceID, &ev.ProviderEvent, &ev.Source, &ev.Kind,
					&ev.RefToken, &ev.ObjectID, &ev.PaymentID, &ev.AmountMinor, &ev.Currency, &ev.Livemode, &occurred, &received, &ev.Attempts, &lease)
			if err != nil {
				return err
			}
			ev.Event.ID, ev.Lease = ev.ProviderEvent, lease
			if ev.OccurredAt, err = time.Parse(timeFormat, occurred); err != nil {
				return err
			}
			if ev.ReceivedAt, err = time.Parse(timeFormat, received); err != nil {
				return err
			}
			found = true
			return nil
		}
	})
	return ev, lease, found, err
}

// runEvent does the next event that is due, if there is one.
func (s *Service) runEvent(ctx context.Context, a Applier) (bool, error) {
	ev, lease, found, err := s.claimEvent(ctx)
	if err != nil || !found {
		return false, err
	}
	try, cancel := context.WithTimeout(ctx, eventTimeout)
	defer cancel()
	err = a.ApplyEvent(try, ev)
	if ctx.Err() != nil {
		return false, ctx.Err() // stopping: the lease runs out and the event is taken again
	}
	return true, s.settleEvent(ctx, ev, lease, err)
}

// settleEvent notes what became of a try, if its worker still owns the event (the lease is the
// claim's number): applied, ignored, waiting for the next try, or failed for good.
func (s *Service) settleEvent(ctx context.Context, ev StoredEvent, lease int64, cause error) error {
	t := s.now()
	at := s.store.stamp(t)
	var ee *EventError
	switch {
	case cause == nil:
		return s.finishEvent(ctx, ev, lease, EventApplied, "", nil, at)
	case errors.As(cause, &ee) && ee.ignored:
		return s.finishEvent(ctx, ev, lease, EventIgnored, ee.Code, ee.Params, at)
	case ee != nil && ee.wait > 0:
		// Not yet: waits, and gives back the attempt its claim counted.
		_, err := s.store.db.ExecContext(ctx, `UPDATE integration_events
			SET state = 'pending', attempts = max(attempts - 1, 0), next_attempt_at = ?, lease_until = NULL, error_code = ?, error_params = ?
			WHERE id = ? AND state = 'running' AND lease = ?`,
			s.store.stamp(t.Add(ee.wait)), string(ee.Code), paramsJSON(ee.Params), ev.ID, lease)
		return err
	case !errors.As(cause, &ee):
		// Not one of the reasons the apply step was written to give: the database, a timeout. The
		// error may quote anything, so it goes to the log by id only, and the row keeps a code.
		slog.Warn("integrations: event failed", "event", ev.ID, "integration", ev.IntegrationID, "attempt", ev.Attempts)
		ee = &EventError{Code: apierr.CodeIntegrationEventGaveUp, Transient: true}
	}
	params := paramsJSON(ee.Params)
	if ee.Transient && ev.Attempts < maxEventAttempts {
		wait := min(eventBackoffBase<<(ev.Attempts-1), eventBackoffMax)
		_, err := s.store.db.ExecContext(ctx, `UPDATE integration_events
			SET state = 'pending', next_attempt_at = ?, lease_until = NULL, error_code = ?, error_params = ?
			WHERE id = ? AND state = 'running' AND lease = ?`,
			s.store.stamp(t.Add(wait)), string(ee.Code), params, ev.ID, lease)
		return err
	}
	return s.finishEvent(ctx, ev, lease, EventFailed, ee.Code, ee.Params, at)
}

// finishEvent gives the event its end, if the claim still owns it. An event the apply step already
// ended, in the transaction that wrote its record (eventApplier's Done), isn't running any more and
// keeps the end it has.
func (s *Service) finishEvent(ctx context.Context, ev StoredEvent, lease int64, state string, code apierr.Code, params apierr.Params, at string) error {
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := s.store.endEvent(ctx, tx, ev.ID, lease, ev.Kind, state, code, params, at)
		return err
	})
}

// endEvent gives the event its end state, if the claim (lease) still owns it, and says whether it
// did. When the end is one the admins are told of (needsAdmin), the same transaction records that a
// notification is owed for that claim (integration_event_notices): the end and the debt can't be
// stored apart, a debt of an earlier claim is kept beside it, and the notification itself is written by
// sendEventNotices, so one that can't be written never holds the event, or the events after it, back.
func (s *Store) endEvent(ctx context.Context, tx *sql.Tx, id, lease int64, kind, state string, code apierr.Code, params apierr.Params, at string) (bool, error) {
	var c any
	if code != "" {
		c = string(code)
	}
	res, err := tx.ExecContext(ctx, `UPDATE integration_events
		SET state = ?, error_code = ?, error_params = ?, lease_until = NULL, processed_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, state, c, paramsJSON(params), at, id, lease)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if needsAdmin(state, code, kind) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO integration_event_notices (event_id, lease, due_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, id, lease, at); err != nil {
			return false, err
		}
	}
	return true, nil
}

// noticeRetry is the wait before a notification that couldn't be written is tried again. It is
// tried until it is written: it is how the admins learn of money that wasn't applied.
const noticeRetry = time.Minute

// noticeTurn is how many events the worker does before the notifications owed get a turn, while the
// queue stays busy.
const noticeTurn = 10

// sendEventNotices writes the admins' notifications that events owe, each in a transaction of its
// own with the debt's settling. One that fails is logged and tried again after noticeRetry, and the
// others go on: no notification holds back another, or an event.
func (s *Service) sendEventNotices(ctx context.Context) {
	for ctx.Err() == nil {
		var id, lease int64
		err := s.store.db.QueryRowContext(ctx, `SELECT event_id, lease FROM integration_event_notices
			WHERE due_at <= ? ORDER BY due_at, event_id, lease LIMIT 1`, s.store.stamp(s.now())).Scan(&id, &lease)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("integrations: reading the notifications events owe failed", "err", err)
			}
			return
		}
		err = s.store.tx(ctx, func(tx *sql.Tx) error {
			if err := s.store.notifyEventAdmins(ctx, tx, id, lease); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM integration_event_notices WHERE event_id = ? AND lease = ?`, id, lease)
			return err
		})
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("integrations: the admins couldn't be notified of an event; tried again later", "event", id)
		if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_event_notices SET due_at = ? WHERE event_id = ? AND lease = ?`,
			s.store.stamp(s.now().Add(noticeRetry)), id, lease); err != nil {
			return
		}
	}
}

func paramsJSON(p apierr.Params) any {
	if len(p) == 0 {
		return nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return string(raw)
}
