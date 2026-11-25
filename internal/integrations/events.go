package integrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	sum := sha256.Sum256(body)
	at := s.now()
	result := storedNew
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO integration_events
			(integration_id, provider_event_id, source, kind, ref_token, object_id, provider_payment_id, amount_minor, currency,
			 body_sha256, occurred_at, received_at, state, next_attempt_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
			ON CONFLICT (integration_id, provider_event_id) DO NOTHING`,
			integration, ev.ID, source, ev.Kind, nullString(ev.RefToken), nullString(ev.ObjectID), nullString(ev.PaymentID),
			ev.AmountMinor, ev.Currency, hex.EncodeToString(sum[:]), s.stamp(ev.OccurredAt), s.stamp(at), s.stamp(at))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			result = storedAgain
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

func (s *Store) noteEvent(ctx context.Context, integration int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return s.touchEvent(ctx, tx, integration, s.now()) })
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
}

// byPublicID finds the enabled integration with the public id. A missing one and a disabled one
// are the same answer, errNotFound.
func (s *Store) byPublicID(ctx context.Context, publicID string) (hookTarget, error) {
	var t hookTarget
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, provider, mode FROM integrations
		WHERE public_id = ? AND disabled_at IS NULL`, publicID).Scan(&t.id, &t.workspace, &t.provider, &t.mode)
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

// drainEvents does the events that are due, one after another.
func (s *Service) drainEvents(ctx context.Context) {
	for ctx.Err() == nil {
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
// that waits or runs is never removed: it is the worker's. A replay of a purged event can't be
// accepted again, since a signature older than the tolerance is refused first.
func (s *Service) purgeEvents(ctx context.Context) error {
	t := s.now()
	_, err := s.store.db.ExecContext(ctx, `DELETE FROM integration_events
		WHERE (state IN ('applied', 'ignored') AND received_at < ?) OR (state = 'failed' AND received_at < ?)`,
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
				// Its worker died (or kept being stopped) on the last try: the bound holds.
				if _, err := tx.ExecContext(ctx, `UPDATE integration_events
					SET state = 'failed', lease_until = NULL, error_code = ?, error_params = NULL, processed_at = ? WHERE id = ?`,
					string(apierr.CodeIntegrationEventGaveUp), at, id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integration_events
				SET state = 'running', attempts = attempts + 1, lease = lease + 1, lease_until = ? WHERE id = ?`,
				s.store.stamp(t.Add(eventLease)), id); err != nil {
				return err
			}
			var occurred string
			err = tx.QueryRowContext(ctx, `SELECT e.id, e.integration_id, i.workspace_id, e.provider_event_id, e.source, e.kind,
					coalesce(e.ref_token, ''), coalesce(e.object_id, ''), coalesce(e.provider_payment_id, ''),
					e.amount_minor, e.currency, e.occurred_at, e.attempts, e.lease
				FROM integration_events e JOIN integrations i ON i.id = e.integration_id WHERE e.id = ?`, id).
				Scan(&ev.ID, &ev.IntegrationID, &ev.WorkspaceID, &ev.ProviderEvent, &ev.Source, &ev.Kind,
					&ev.RefToken, &ev.ObjectID, &ev.PaymentID, &ev.AmountMinor, &ev.Currency, &occurred, &ev.Attempts, &lease)
			if err != nil {
				return err
			}
			ev.Event.ID = ev.ProviderEvent
			if ev.OccurredAt, err = time.Parse(timeFormat, occurred); err != nil {
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
		return s.finishEvent(ctx, ev.ID, lease, EventApplied, "", nil, at)
	case errors.As(cause, &ee) && ee.ignored:
		return s.finishEvent(ctx, ev.ID, lease, EventIgnored, ee.Code, ee.Params, at)
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
	return s.finishEvent(ctx, ev.ID, lease, EventFailed, ee.Code, ee.Params, at)
}

func (s *Service) finishEvent(ctx context.Context, id, lease int64, state string, code apierr.Code, params apierr.Params, at string) error {
	var c any
	if code != "" {
		c = string(code)
	}
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_events
		SET state = ?, error_code = ?, error_params = ?, lease_until = NULL, processed_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, state, c, paramsJSON(params), at, id, lease)
	return err
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
