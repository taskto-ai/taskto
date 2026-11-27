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

const (
	EventPending = "pending"
	EventRunning = "running"
	EventApplied = "applied"
	EventIgnored = "ignored"
	EventFailed  = "failed"
)

const (
	SourceWebhook = "webhook"
	SourcePoll    = "poll"
)

const (
	maxEventAttempts = 5
	eventLease       = 2 * time.Minute
	eventTimeout     = time.Minute
	eventPoll        = 2 * time.Second
	keepEventsDone   = 30 * 24 * time.Hour
	keepEventsAll    = 90 * 24 * time.Hour
	eventBackoffBase = 5 * time.Second
	eventBackoffMax  = 5 * time.Minute
)

type StoredEvent struct {
	ID            int64
	IntegrationID int64
	WorkspaceID   int64
	ProviderEvent string
	Source        string
	Event
	Attempts   int
	Lease      int64
	ReceivedAt time.Time
}

type Applier interface {
	ApplyEvent(ctx context.Context, ev StoredEvent) error
}

type EventError struct {
	Code      apierr.Code
	Params    apierr.Params
	Transient bool
	ignored   bool
	wait      time.Duration
}

func (e *EventError) Error() string { return string(e.Code) }

func Ignore(code apierr.Code, params ...apierr.Params) error {
	e := &EventError{Code: code, ignored: true}
	if len(params) > 0 {
		e.Params = params[0]
	}
	return e
}

func waitEvent(code apierr.Code, d time.Duration) error {
	return &EventError{Code: code, Transient: true, wait: max(d, time.Second)}
}

type accepted int

const (
	storedNew accepted = iota
	storedAgain
)

func (s *Store) record(ctx context.Context, integration int64, ev Event, source string, body []byte) (accepted, error) {
	return s.recordGuarded(ctx, integration, nil, ev, source, body)
}

var errHookRefused = errors.New("the integration changed since the delivery was verified")

type hookGuard struct {
	mode     string
	revision string
}

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

func (s *Store) recordGuarded(ctx context.Context, integration int64, guard *hookGuard, ev Event, source string, body []byte) (accepted, error) {
	sum := sha256.Sum256(body)
	at := s.now()
	result := storedNew
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := guard.check(ctx, s, tx, integration); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO integration_events
			(integration_id, provider_event_id, source, kind, ref_token, object_id, provider_payment_id, link_id, amount_minor, currency, livemode,
			 body_sha256, occurred_at, received_at, state, next_attempt_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
			ON CONFLICT (integration_id, provider_event_id) DO NOTHING`,
			integration, ev.ID, source, ev.Kind, nullString(ev.RefToken), nullString(ev.ObjectID), nullString(ev.PaymentID), nullString(ev.LinkID),
			ev.AmountMinor, ev.Currency, ev.Livemode, hex.EncodeToString(sum[:]), s.stamp(ev.OccurredAt), s.stamp(at), s.stamp(at))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			result = storedAgain
		}
		if source == SourcePoll {
			return nil
		}
		return s.touchEvent(ctx, tx, integration, at)
	})
	return result, err
}

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

type hookTarget struct {
	id                                   int64
	workspace                            int64
	provider                             string
	mode                                 string
	disabled                             bool
	registeredURL, previousRegisteredURL string
}

func (s *Store) byPublicID(ctx context.Context, publicID string) (hookTarget, error) {
	var t hookTarget
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, provider, mode, disabled_at IS NOT NULL, coalesce(webhook_registered_url, ''),
		coalesce(webhook_previous_registered_url, '') FROM integrations WHERE public_id = ?`, publicID).Scan(&t.id, &t.workspace, &t.provider,
		&t.mode, &t.disabled, &t.registeredURL, &t.previousRegisteredURL)
	if errors.Is(err, sql.ErrNoRows) {
		return hookTarget{}, errNotFound
	}
	return t, err
}

func (s *Service) SetApplier(a Applier) { s.applier.Store(&a) }

func (s *Service) currentApplier() Applier {
	if p := s.applier.Load(); p != nil {
		return *p
	}
	return nil
}

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

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) drainEvents(ctx context.Context) {
	defer s.sendEventNotices(ctx)
	for done := 1; ctx.Err() == nil; done++ {
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

func (s *Service) purgeEvents(ctx context.Context) error {
	t := s.now()
	_, err := s.store.db.ExecContext(ctx, `DELETE FROM integration_events
		WHERE ((state IN ('applied', 'ignored') AND received_at < ?) OR (state = 'failed' AND received_at < ?))
			AND NOT EXISTS (SELECT 1 FROM integration_event_notices n WHERE n.event_id = integration_events.id)`,
		s.store.stamp(t.Add(-keepEventsDone)), s.store.stamp(t.Add(-keepEventsAll)))
	return err
}

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
					coalesce(e.ref_token, ''), coalesce(e.object_id, ''), coalesce(e.provider_payment_id, ''), coalesce(e.link_id, ''),
					e.amount_minor, e.currency, e.livemode, e.occurred_at, e.received_at, e.attempts, e.lease
				FROM integration_events e JOIN integrations i ON i.id = e.integration_id WHERE e.id = ?`, id).
				Scan(&ev.ID, &ev.IntegrationID, &ev.WorkspaceID, &ev.ProviderEvent, &ev.Source, &ev.Kind,
					&ev.RefToken, &ev.ObjectID, &ev.PaymentID, &ev.LinkID, &ev.AmountMinor, &ev.Currency, &ev.Livemode, &occurred, &received, &ev.Attempts, &lease)
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

func (s *Service) runEvent(ctx context.Context, a Applier) (bool, error) {
	ev, lease, found, err := s.claimEvent(ctx)
	if err != nil || !found {
		return false, err
	}
	try, cancel := context.WithTimeout(ctx, eventTimeout)
	defer cancel()
	err = a.ApplyEvent(try, ev)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return true, s.settleEvent(ctx, ev, lease, err)
}

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
		_, err := s.store.db.ExecContext(ctx, `UPDATE integration_events
			SET state = 'pending', attempts = max(attempts - 1, 0), next_attempt_at = ?, lease_until = NULL, error_code = ?, error_params = ?
			WHERE id = ? AND state = 'running' AND lease = ?`,
			s.store.stamp(t.Add(ee.wait)), string(ee.Code), paramsJSON(ee.Params), ev.ID, lease)
		return err
	case !errors.As(cause, &ee):
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

func (s *Service) finishEvent(ctx context.Context, ev StoredEvent, lease int64, state string, code apierr.Code, params apierr.Params, at string) error {
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := s.store.endEvent(ctx, tx, ev.ID, lease, ev.Kind, state, code, params, at)
		return err
	})
}

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

const noticeRetry = time.Minute

const noticeTurn = 10

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
