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

// Applying a verified event to the record it is about (docs/integrations.md#applying-events).
//
// The worker (events.go) hands each stored event to this step. It is the only code that reads what a
// provider said and changes what an app holds, so it is strict about what it believes:
//
//   - The event is mapped to a record through a token the server issued, and nothing else: the token
//     of any of a ref's keys (refByToken), since a payment echoes the token of the key whose object was
//     paid, which is a later key's after an earlier one expired. The provider's metadata, descriptions
//     and ids name no record, app or space here: the ref does, and
//     the ref belongs to one integration, which must be the event's, and to one mode of the provider's
//     account (test or live), which must be the mode the event is of.
//   - A refund carries no token of ours. It is mapped through the payment the ref learned from the
//     verified event that paid it (integration_refs.payment_id), in the event's own integration. A
//     provider tells events in no set order, so a full refund can come before the payment it undoes:
//     one that no paid ref has, and that a ref of the integration not paid yet could be (the same
//     amount, currency and mode), waits for that payment for refundWait, on the worker's schedule and
//     using up none of its attempts, and the payment's apply wakes it. After that it fails
//     (integration_event_payment_unknown), and the admins are told.
//   - The money is checked exactly against what the ref asked for: another amount or currency is not
//     applied (integration_amount_mismatch), whatever else is right.
//   - The ref's state only moves forward, by a conditional UPDATE ... WHERE state IN (...) that runs in
//     the transaction that writes the record (paylink.EventWrite.Settle), after the app's approval,
//     the integration and the space have been checked as of that moment: the state and the record
//     change together or not at all. An event whose state change finds the ref moved is read again,
//     and is a duplicate or a late event: ignored, with the code that says which.
//   - A payment on a ref that was paid already, by another payment, is a second payment: recorded for
//     the admins (integration_ref_duplicates) and never applied twice. Two payments are the same when
//     both name the same payment, or, when either names none, the same object (samePayment).
//   - A failure of the apply-time checks, a record that is gone or one that doesn't fit leaves the ref
//     as it was and the event failed with a code, and the admins are told. Nothing about a payment
//     that arrived is dropped silently: it ends as applied, as ignored with a reason, or as failed
//     with a reason an admin sees, and a failed one is retried once what is wrong is fixed.
//
// The step is idempotent: the event store forgets an event when it is purged, and the provider's retry
// of it is stored again, so the same event may arrive a second time long after the first. The second
// finds the ref where the first left it and changes nothing. And an event's own outcome (applied, or
// ignored because the app's rule didn't hold) commits in the transaction that settles its ref and
// writes its record (Done): a worker that stops after that commit leaves an event that is ended, never
// one that a later try would find settled and take for a duplicate.

// refundWait is how long a full refund waits for the payment it undoes to be applied: Stripe retries a
// webhook it couldn't deliver for up to three days in live mode, so a payment told after its refund
// arrives within it.
const refundWait = 72 * time.Hour

// The waits between two tries of a refund that waits: half of the time it has waited, within these.
const (
	refundRetryMin = 30 * time.Second
	refundRetryMax = 30 * time.Minute
)

// UseRecords makes the apps the place events are applied to: it sets the worker's apply step. Until
// it is called, stored events wait.
func (s *Service) UseRecords(w paylink.EventWriter) {
	s.SetApplier(&eventApplier{svc: s, records: w})
}

type eventApplier struct {
	svc     *Service
	records paylink.EventWriter
}

// What a kind of event does to a ref: the states it may move from, and the one it moves to.
type transition struct {
	from []string
	to   string
}

var transitions = map[string]transition{
	// Money is never turned away for the state it finds: a payment may arrive on a ref whose link was
	// being made, whose failure was recorded after the payer had the page, or that expired by our own
	// count.
	EventPaymentSucceeded: {from: []string{"creating", "open", "failed", "expired"}, to: "paid"},
	EventPaymentExpired:   {from: []string{"creating", "open"}, to: "expired"},
	EventPaymentFailed:    {from: []string{"creating", "open"}, to: "failed"},
	EventPaymentRefunded:  {from: []string{"paid"}, to: "refunded"},
}

// errStateMoved is the answer of Settle when the ref was changed by another between reading it and
// the conditional update.
var errStateMoved = errors.New("the ref's state moved")

// maxApplyReads bounds the rereads of a ref whose state keeps moving under the update.
const maxApplyReads = 3

func (a *eventApplier) ApplyEvent(ctx context.Context, ev StoredEvent) error {
	tr, ok := transitions[ev.Kind]
	if !ok {
		return Ignore(apierr.CodeIntegrationEventRefUnknown) // a kind the apply step doesn't know: nothing of it is applied
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
			continue // another worker changed the ref first: read it again and see what that makes the event
		}
		if err != nil {
			if errors.Is(err, errLeaseLost) {
				return err // the event is another claim's: nothing was written, and that claim decides
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

// errLeaseLost is Done's answer when the event's claim is no longer the one that runs the write: the
// write is rolled back, so only the claim that owns the event applies it.
var errLeaseLost = errors.New("the event is another claim's")

// outcomeOf is the end of an event whose write went through: applied, or ignored with the reason.
func outcomeOf(res paylink.EventResult) (string, apierr.Code) {
	switch {
	case res.NoHandler:
		return EventIgnored, apierr.CodeIntegrationEventNoHandler
	case res.ConditionUnmet:
		return EventIgnored, apierr.CodeIntegrationEventConditionUnmet
	}
	return EventApplied, ""
}

// applied ends the event in tx, the transaction that settled its ref and wrote (or left) its record,
// with what the write made of it, and owes the admins a notification if they are told of that end.
// A payment that applied wakes the refunds that wait for it.
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
	// The refunds of this payment that wait for it are due now; one whose wait ended, and that failed
	// for want of it, is tried again as an admin's retry would (its admins were told already).
	_, err = tx.ExecContext(ctx, `UPDATE integration_events
		SET state = 'pending', next_attempt_at = ?,
			attempts = CASE WHEN state = 'failed' THEN 0 ELSE attempts END,
			processed_at = CASE WHEN state = 'failed' THEN NULL ELSE processed_at END
		WHERE integration_id = ? AND kind = ? AND provider_payment_id = ?
			AND (state = 'pending' OR (state = 'failed' AND error_code = ?))`,
		at, ev.IntegrationID, EventPaymentRefunded, ev.PaymentID, string(apierr.CodeIntegrationEventPaymentUnknown))
	return err
}

// refForEvent finds the ref an event is about, in the event's own integration.
func (s *Service) refForEvent(ctx context.Context, ev StoredEvent) (ref, error) {
	unknown := Ignore(apierr.CodeIntegrationEventRefUnknown)
	if ev.Kind != EventPaymentRefunded {
		if ev.RefToken == "" {
			return ref{}, unknown
		}
		// Only a token the server issued finds a ref: one of its keys', never anything else the event says.
		r, found, err := s.refByToken(ctx, ev.RefToken)
		if err != nil {
			return ref{}, err
		}
		// A token of another integration's ref names nothing here, and says nothing of whose it is.
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
	// No ref has been paid by this payment (yet). The payment may be ours all the same, its own event
	// not applied yet: providers tell events in no set order, and a refund can come first. It is when an
	// earlier verified event of this integration told the payment with one of our tokens, or when a ref
	// of the integration that isn't paid yet asks exactly what the refund gives back (a ref is made
	// before its payment, and the payment before its refund, so that ref exists by now). Such a refund
	// waits for its payment; any other is a payment of the provider's account that this integration
	// never made a link for, which changes no record.
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

// refundWaits is the answer for a refund that waits for its payment: tried again later, for refundWait
// from when it arrived, and failed for the admins after that.
func (s *Service) refundWaits(ev StoredEvent) error {
	waited := s.now().Sub(ev.ReceivedAt)
	if waited >= refundWait {
		return &EventError{Code: apierr.CodeIntegrationEventPaymentUnknown}
	}
	return waitEvent(apierr.CodeIntegrationEventPaymentUnknown, min(max(waited/2, refundRetryMin), refundRetryMax, refundWait-waited))
}

// classify says whether the event may be applied to the ref as it is now: nil if it may, and
// otherwise why not, as the worker's answer: ignored (a duplicate, a late event, another mode), failed
// (a payment that isn't what was asked) or a second payment recorded for the admins.
func (a *eventApplier) classify(ctx context.Context, ev StoredEvent, r ref, tr transition) error {
	if r.accountMode != modeOfEvent(ev.Livemode) {
		return Ignore(apierr.CodeIntegrationEventModeMismatch)
	}
	if !slices.Contains(tr.from, r.state) {
		return a.svc.settledAlready(ctx, ev, r, tr)
	}
	// A ref with no object is one whose current key's outcome is unknown (or in flight): the entry that
	// asked for the link still looks for it, and may adopt it. An end that isn't money (expired, failed)
	// would leave a ref that can no longer note the link, so it waits for what the lookup finds; only a
	// payment, which is real money, applies to such a ref.
	if r.objectID == "" && ev.Kind != EventPaymentSucceeded && ev.Kind != EventPaymentRefunded {
		if ev.Kind == EventPaymentExpired {
			// It does say what became of the key whose token it carries, though: that key's object
			// expired, and it is no longer one a payer may use (expiredKeyOfEvent).
			if err := a.svc.expiredKeyOfEvent(ctx, r, ev); err != nil {
				return err
			}
		}
		return Ignore(apierr.CodeIntegrationEventLate)
	}
	// An end that isn't money, told of an object that isn't the ref's link, is an earlier key's (the
	// object of a key that expired, which the ref moved on from): it says nothing of the link the payer
	// has now, so the ref stays as it is.
	if ev.Kind != EventPaymentSucceeded && ev.Kind != EventPaymentRefunded && ev.ObjectID != "" && ev.ObjectID != r.objectID {
		return Ignore(apierr.CodeIntegrationEventLate)
	}
	if ev.Kind == EventPaymentRefunded {
		// What has been refunded so far, of this ref's currency, and not more than was paid.
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

// modeOfEvent is the mode of the provider's account an event of livemode is of.
func modeOfEvent(live bool) string {
	if live {
		return ModeLive
	}
	return ModeTest
}

// settledAlready is the answer for an event whose state change the ref has no use for: the ref is
// where the event says (a duplicate, such as the same event told again after a purge) or past it (a
// late event), or the event is of a payment the ref isn't paid by.
func (s *Service) settledAlready(ctx context.Context, ev StoredEvent, r ref, tr transition) error {
	switch {
	case ev.Kind == EventPaymentSucceeded && (r.state == "paid" || r.state == "refunded"):
		if samePayment(r, ev) {
			return Ignore(apierr.CodeIntegrationEventDuplicate) // the same payment, told again
		}
		return s.secondPayment(ctx, ev, r)
	case r.state == tr.to:
		return Ignore(apierr.CodeIntegrationEventDuplicate)
	case ev.Kind == EventPaymentRefunded:
		// A refund of a ref that was not paid (its payment is not applied yet, or was refused): wait.
		return s.refundWaits(ev)
	case ev.Kind == EventPaymentExpired && ev.ObjectID != "" && ev.ObjectID == r.objectID:
		// The ref's own link expired after the ref was paid on another key's object: the payment stays
		// as it was settled, and the link is kept as one no payer can use (MayBePaid).
		if err := s.noteLinkExpired(ctx, r.id, r.objectID, s.store.stamp(s.now())); err != nil {
			return err
		}
	}
	return Ignore(apierr.CodeIntegrationEventLate)
}

// secondPayment records another payment of a paid ref for the admins, kept for as long as the ref is
// (not only while the event is), once per payment, and ends the event in the same transaction: told
// to the admins when the payment is new, and a duplicate when it was recorded already (the same event
// told again after the purge). A worker that stops between the two leaves neither, so the first
// notification can't be lost to a later try finding the row.
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

// samePayment says whether a paid event tells the payment that paid the ref: the same payment when
// both name one, and otherwise the same object when both name one. Two ids that are both empty say
// nothing, so a payment that can't be told apart from the ref's by either is a second payment: told to
// the admins, never taken for a repeat.
func samePayment(r ref, ev StoredEvent) bool {
	if r.paymentID != "" && ev.PaymentID != "" {
		return r.paymentID == ev.PaymentID
	}
	return r.paidObjectID != "" && ev.ObjectID != "" && r.paidObjectID == ev.ObjectID
}

// paymentKey is what tells a second payment from another: its payment, else its object, else the
// event that told it.
func paymentKey(ev StoredEvent) string {
	switch {
	case ev.PaymentID != "":
		return "payment:" + ev.PaymentID
	case ev.ObjectID != "":
		return "object:" + ev.ObjectID
	}
	return "event:" + ev.ProviderEvent
}

// settleRef moves the ref to the event's state, in tx, only from a state the event may move it from:
// the conditional update that makes a duplicate or a late event change nothing. A paid ref keeps the
// object and the payment the verified event told, which is how a refund finds it and a repeat is
// known.
func (s *Service) settleRef(ctx context.Context, tx *sql.Tx, r ref, ev StoredEvent, tr transition) error {
	at := s.store.stamp(s.store.now())
	args := []any{tr.to, at}
	set := `state = ?, settled_at = coalesce(settled_at, ?)`
	if tr.to == "paid" {
		set += `, paid_object_id = ?, payment_id = ?, paid_link_id = ?`
		args = append(args, nullString(ev.ObjectID), nullString(ev.PaymentID), nullString(ev.LinkID))
		// A payment is the answer to a call whose outcome was unknown: the link was made, and a payer
		// used it. The ref that has no object yet (its call's answer never came, and the entry only
		// looks for it) learns it from the event, if the event names it and no other ref has it. The key
		// the payment came through stops being unknown (settleAttemptOfEvent), and the paid ref is no
		// one's to confirm. The entry that asked for the link is stopped by madeLink when it runs.
		set += `, provider_object_id = CASE WHEN provider_object_id IS NULL AND ? != '' AND NOT EXISTS (
				SELECT 1 FROM integration_refs o WHERE o.integration_id = ? AND o.provider_object_id = ?)
			THEN ? ELSE provider_object_id END`
		args = append(args, ev.ObjectID, ev.IntegrationID, ev.ObjectID, ev.ObjectID)
	}
	// The ref's integration is part of the condition, as it is of the lookup: a token never moves
	// another integration's ref.
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

// expiredKeyOfEvent notes, for a verified expiry of an object that carries the token of a key of a
// ref with no link (the key's call was answered with nothing the server could read, and its outcome
// is unknown), that the key's object expired: the key is accounted for, and left nothing a payer can
// use. Only that key, and only a Checkout Session the server asked for itself (the ref's calls make
// sessions, since it has pages of ours): a session opened from a Stripe Payment Link expires while
// its link stays payable, and says nothing of the link. A key that has an outcome, or has another
// object on record, is left as it is. The ref is left as it is too: its entry, or a newer one of the
// record, moves on from it (nextAttempt, takeEarlier).
func (s *Service) expiredKeyOfEvent(ctx context.Context, r ref, ev StoredEvent) error {
	if ev.ObjectID == "" || ev.LinkID != "" || r.successURL == "" || ev.RefToken == "" {
		return nil
	}
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'expired', object_id = ?
		WHERE ref_id = ? AND token = ? AND outcome IS NULL AND (object_id IS NULL OR object_id = ?)`,
		ev.ObjectID, r.id, ev.RefToken, ev.ObjectID)
	return err
}

// settleAttemptOfEvent gives the key the payment came through (the one whose token the event carried)
// the outcome settled, in the transaction that paid the ref, if it has none: a call under it may have
// made the object nobody had seen, and the payment says it did, so nothing is unknown of it any more.
// A key that has an outcome keeps it. Other keys of the ref are not touched: the ref is paid, and no
// key is taken or asked again for a paid ref (OpenPaymentLink, ConfirmNoPaymentLink).
func (s *Service) settleAttemptOfEvent(ctx context.Context, tx *sql.Tx, r ref, ev StoredEvent) error {
	_, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'settled', object_id = coalesce(object_id, ?)
		WHERE ref_id = ? AND token = ? AND outcome IS NULL`, nullString(ev.ObjectID), r.id, ev.RefToken)
	return err
}

// needsAdmin says whether an event that ended so is one the workspace's admins are told of: every
// failure, and the two things that are money not applied and no failure of the server's: a second
// payment, and a payment the app's own condition left unrecorded.
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

// notifyEventAdmins tells the workspace's admins, in tx, of the end the event's claim (lease) came to:
// one the event owes them a notification of (endEvent decided that). One notification per admin per
// claim: the same failure told again by a retry of the worker adds none, a failure after an admin's
// retry is a new one. A notification holds the ids only: what it says is worked out when it is read
// (notifications.Item), for an admin of the workspace then.
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
