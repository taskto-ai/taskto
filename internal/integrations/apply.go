package integrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/paylink"
)

const refundWait = 72 * time.Hour

const (
	refundRetryMin = 30 * time.Second
	refundRetryMax = 30 * time.Minute
)

func (s *Service) UseRecords(w paylink.EventWriter) {
	s.SetApplier(&eventApplier{svc: s, records: w})
}

type eventApplier struct {
	svc     *Service
	records paylink.EventWriter
}

type transition struct {
	from []string
	to   string
}

var transitions = map[string]transition{
	EventPaymentSucceeded: {from: []string{"creating", "open", "failed", "expired"}, to: "paid"},
	EventPaymentExpired:   {from: []string{"creating", "open"}, to: "expired"},
	EventPaymentFailed:    {from: []string{"creating", "open"}, to: "failed"},
	EventPaymentRefunded:  {from: []string{"paid"}, to: "refunded"},
}

var errStateMoved = errors.New("the ref's state moved")

const maxApplyReads = 3

func (a *eventApplier) ApplyEvent(ctx context.Context, ev StoredEvent) error {
	tr, ok := transitions[ev.Kind]
	if !ok {
		return Ignore(apierr.CodeIntegrationEventRefUnknown)
	}
	for range maxApplyReads {
		r, err := a.svc.refForEvent(ctx, ev)
		if err != nil {
			return err
		}
		if err := a.classify(ctx, ev, r, tr); err != nil {
			return err
		}
		res, err := a.records.WriteEvent(ctx, paylink.EventWrite{
			WorkspaceID: ev.WorkspaceID, IntegrationID: ev.IntegrationID, AppID: r.appID, RecordID: r.recordID, Slot: r.slot,
			Event: ev.Kind, AmountMinor: ev.AmountMinor, Currency: ev.Currency, OccurredAt: ev.OccurredAt, PaymentID: ev.PaymentID,
			Settle: func(ctx context.Context, tx *sql.Tx) error { return a.svc.settleRef(ctx, tx, r, ev, tr) },
			Done: func(ctx context.Context, tx *sql.Tx, res paylink.EventResult) error {
				return a.svc.applied(ctx, tx, ev, res)
			},
		})
		if errors.Is(err, errStateMoved) {
			continue
		}
		if err != nil {
			if errors.Is(err, errLeaseLost) {
				return err
			}
			var pe *paylink.Error
			if errors.As(err, &pe) {
				return &EventError{Code: pe.Code, Params: pe.Params, Transient: pe.Transient}
			}
			return err
		}
		switch {
		case res.NoHandler:
			return Ignore(apierr.CodeIntegrationEventNoHandler)
		case res.ConditionUnmet:
			return Ignore(apierr.CodeIntegrationEventConditionUnmet)
		}
		return nil
	}
	return &EventError{Code: apierr.CodeIntegrationEventGaveUp, Transient: true}
}

var errLeaseLost = errors.New("the event is another claim's")

func outcomeOf(res paylink.EventResult) (string, apierr.Code) {
	switch {
	case res.NoHandler:
		return EventIgnored, apierr.CodeIntegrationEventNoHandler
	case res.ConditionUnmet:
		return EventIgnored, apierr.CodeIntegrationEventConditionUnmet
	}
	return EventApplied, ""
}

func (s *Service) applied(ctx context.Context, tx *sql.Tx, ev StoredEvent, res paylink.EventResult) error {
	state, code := outcomeOf(res)
	at := s.store.stamp(s.now())
	ok, err := s.store.endEvent(ctx, tx, ev.ID, ev.Lease, ev.Kind, state, code, nil, at)
	if err != nil {
		return err
	}
	if !ok {
		return errLeaseLost
	}
	if ev.Kind != EventPaymentSucceeded || ev.PaymentID == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE integration_events
		SET state = 'pending', next_attempt_at = ?,
			attempts = CASE WHEN state = 'failed' THEN 0 ELSE attempts END,
			processed_at = CASE WHEN state = 'failed' THEN NULL ELSE processed_at END
		WHERE integration_id = ? AND kind = ? AND provider_payment_id = ?
			AND (state = 'pending' OR (state = 'failed' AND error_code = ?))`,
		at, ev.IntegrationID, EventPaymentRefunded, ev.PaymentID, string(apierr.CodeIntegrationEventPaymentUnknown))
	return err
}

func (s *Service) refForEvent(ctx context.Context, ev StoredEvent) (ref, error) {
	unknown := Ignore(apierr.CodeIntegrationEventRefUnknown)
	if ev.Kind != EventPaymentRefunded {
		if ev.RefToken == "" {
			return ref{}, unknown
		}
		r, found, err := s.refByToken(ctx, ev.RefToken)
		if err != nil {
			return ref{}, err
		}
		if !found || r.integrationID != ev.IntegrationID {
			return ref{}, unknown
		}
		return r, nil
	}
	if ev.PaymentID == "" {
		return ref{}, unknown
	}
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE integration_id = ? AND payment_id = ?`,
		ev.IntegrationID, ev.PaymentID))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ref{}, err
	}
	var ours bool
	if err := s.store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_events
			WHERE integration_id = ? AND provider_payment_id = ? AND ref_token IS NOT NULL AND kind = ?)
		OR EXISTS (SELECT 1 FROM integration_refs
			WHERE integration_id = ? AND state NOT IN ('paid', 'refunded') AND amount_minor = ? AND currency = ? AND account_mode = ?)`,
		ev.IntegrationID, ev.PaymentID, EventPaymentSucceeded,
		ev.IntegrationID, ev.AmountMinor, ev.Currency, modeOfEvent(ev.Livemode)).Scan(&ours); err != nil {
		return ref{}, err
	}
	if !ours {
		return ref{}, unknown
	}
	return ref{}, s.refundWaits(ev)
}

func (s *Service) refundWaits(ev StoredEvent) error {
	waited := s.now().Sub(ev.ReceivedAt)
	if waited >= refundWait {
		return &EventError{Code: apierr.CodeIntegrationEventPaymentUnknown}
	}
	return waitEvent(apierr.CodeIntegrationEventPaymentUnknown, min(max(waited/2, refundRetryMin), refundRetryMax, refundWait-waited))
}

func (a *eventApplier) classify(ctx context.Context, ev StoredEvent, r ref, tr transition) error {
	if r.accountMode != modeOfEvent(ev.Livemode) {
		return Ignore(apierr.CodeIntegrationEventModeMismatch)
	}
	if !slices.Contains(tr.from, r.state) {
		return a.svc.settledAlready(ctx, ev, r, tr)
	}
	if r.objectID == "" && ev.Kind != EventPaymentSucceeded && ev.Kind != EventPaymentRefunded {
		return Ignore(apierr.CodeIntegrationEventLate)
	}
	if ev.Kind != EventPaymentSucceeded && ev.Kind != EventPaymentRefunded && ev.ObjectID != "" && ev.ObjectID != r.objectID {
		return Ignore(apierr.CodeIntegrationEventLate)
	}
	if ev.Kind == EventPaymentRefunded {
		if ev.Currency != r.currency || ev.AmountMinor > r.amountMinor {
			return &EventError{Code: apierr.CodeIntegrationAmountMismatch}
		}
		if ev.AmountMinor < r.amountMinor {
			return Ignore(apierr.CodeIntegrationEventPartialRefund)
		}
		return nil
	}
	if ev.AmountMinor != r.amountMinor || ev.Currency != r.currency {
		return &EventError{Code: apierr.CodeIntegrationAmountMismatch}
	}
	return nil
}

func modeOfEvent(live bool) string {
	if live {
		return ModeLive
	}
	return ModeTest
}

func (s *Service) settledAlready(ctx context.Context, ev StoredEvent, r ref, tr transition) error {
	switch {
	case ev.Kind == EventPaymentSucceeded && (r.state == "paid" || r.state == "refunded"):
		if samePayment(r, ev) {
			return Ignore(apierr.CodeIntegrationEventDuplicate)
		}
		return s.secondPayment(ctx, ev, r)
	case r.state == tr.to:
		return Ignore(apierr.CodeIntegrationEventDuplicate)
	case ev.Kind == EventPaymentRefunded:
		return s.refundWaits(ev)
	}
	return Ignore(apierr.CodeIntegrationEventLate)
}

func (s *Service) secondPayment(ctx context.Context, ev StoredEvent, r ref) error {
	code := apierr.CodeIntegrationEventSecondPayment
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		at := s.store.stamp(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO integration_ref_duplicates (ref_id, payment_key, object_id, payment_id, amount_minor, currency, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (ref_id, payment_key) DO NOTHING`,
			r.id, paymentKey(ev), nullString(ev.ObjectID), nullString(ev.PaymentID), ev.AmountMinor, ev.Currency, at)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			code = apierr.CodeIntegrationEventDuplicate
		}
		ok, err := s.store.endEvent(ctx, tx, ev.ID, ev.Lease, ev.Kind, EventIgnored, code, nil, at)
		if err == nil && !ok {
			err = errLeaseLost
		}
		return err
	})
	if err != nil {
		return err
	}
	return Ignore(code)
}

func samePayment(r ref, ev StoredEvent) bool {
	if r.paymentID != "" && ev.PaymentID != "" {
		return r.paymentID == ev.PaymentID
	}
	return r.paidObjectID != "" && ev.ObjectID != "" && r.paidObjectID == ev.ObjectID
}

func paymentKey(ev StoredEvent) string {
	switch {
	case ev.PaymentID != "":
		return "payment:" + ev.PaymentID
	case ev.ObjectID != "":
		return "object:" + ev.ObjectID
	}
	return "event:" + ev.ProviderEvent
}

func (s *Service) settleRef(ctx context.Context, tx *sql.Tx, r ref, ev StoredEvent, tr transition) error {
	at := s.store.stamp(s.store.now())
	args := []any{tr.to, at}
	set := `state = ?, settled_at = coalesce(settled_at, ?)`
	if tr.to == "paid" {
		set += `, paid_object_id = ?, payment_id = ?`
		args = append(args, nullString(ev.ObjectID), nullString(ev.PaymentID))
		set += `, provider_object_id = CASE WHEN provider_object_id IS NULL AND ? != '' AND NOT EXISTS (
				SELECT 1 FROM integration_refs o WHERE o.integration_id = ? AND o.provider_object_id = ?)
			THEN ? ELSE provider_object_id END`
		args = append(args, ev.ObjectID, ev.IntegrationID, ev.ObjectID, ev.ObjectID)
	}
	q := `UPDATE integration_refs SET ` + set + ` WHERE id = ? AND integration_id = ? AND state IN (`
	args = append(args, r.id, ev.IntegrationID)
	for i, from := range tr.from {
		if i > 0 {
			q += ", "
		}
		q += "?"
		args = append(args, from)
	}
	res, err := tx.ExecContext(ctx, q+")", args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return errStateMoved
	}
	if tr.to == "paid" {
		return s.settleAttemptOfEvent(ctx, tx, r, ev)
	}
	return nil
}

func (s *Service) settleAttemptOfEvent(ctx context.Context, tx *sql.Tx, r ref, ev StoredEvent) error {
	_, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'settled', object_id = coalesce(object_id, ?)
		WHERE ref_id = ? AND token = ? AND outcome IS NULL`, nullString(ev.ObjectID), r.id, ev.RefToken)
	return err
}

func needsAdmin(state string, code apierr.Code, kind string) bool {
	switch state {
	case EventFailed:
		return true
	case EventIgnored:
		return code == apierr.CodeIntegrationEventSecondPayment ||
			code == apierr.CodeIntegrationEventConditionUnmet && kind == EventPaymentSucceeded
	}
	return false
}

func (s *Store) notifyEventAdmins(ctx context.Context, tx *sql.Tx, event, lease int64) error {
	var workspace, integration int64
	err := tx.QueryRowContext(ctx, `SELECT i.workspace_id, e.integration_id
		FROM integration_events e JOIN integrations i ON i.id = e.integration_id WHERE e.id = ?`, event).
		Scan(&workspace, &integration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM memberships WHERE workspace_id = ? AND role = 'admin'`, workspace)
	if err != nil {
		return err
	}
	var admins []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		admins = append(admins, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range admins {
		if err := notifications.Add(ctx, tx, notifications.Notice{
			UserID: id, WorkspaceID: workspace, Kind: notifications.KindIntegrationEvent,
			Ref:       notifications.Ref{Integration: integration, Event: event},
			DedupeKey: fmt.Sprintf("event:%d:%d", event, lease),
		}); err != nil {
			return err
		}
	}
	return nil
}
