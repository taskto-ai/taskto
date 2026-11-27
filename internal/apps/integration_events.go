package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/currency"
	"taskto.ai/taskto/internal/paylink"
)

// Applying a verified event of an integration to a record (docs/integrations.md#applying-events).
//
// The integrations' event worker has already mapped the event to a record, through the reference the
// server issued, and checked its money. What it asks the apps is to write what the app declared for
// the event, and nothing else. Its whole authority is the live release's approved contract, read in
// the transaction that writes (so an approval revoked a moment ago, a release published or the app
// taken offline is seen):
//
//   - the app is online, its live release declares the slot, that release's contract is approved,
//     and the integration it was bound to is switched on and allowed in the app's space
//     (liveBinding), and it is the integration the reference was made under;
//   - the fields written are the ones the contract's handler for the event lists, with its literals
//     and the sources it names, never anything the event says as text (EventWrite holds none);
//   - the handler's only_if rule, which the contract holds too, is checked against the record as it
//     is then: when it doesn't hold nothing is written, and the reference is settled all the same,
//     since the money did arrive;
//   - the record still has to fit the collection (Collection.Fits), and a write that wouldn't is
//     refused with nothing changed;
//   - the history's actor is the integration (SystemWrite), and no effect is queued: effects run as
//     the user who acted, and there is none.
//
// Settle, the integrations' step that moves the reference to its new state, runs in that same
// transaction after the checks and before the write, so the state and the record change together
// or neither does. Done, the integrations' step that records the event's outcome, runs last in it,
// so the outcome commits with them too.

// EventBinding says which integration the slot of the app is bound to when an event may be written
// to the app now: Binding's check, with the refusals that mean "not now" (the app gone, offline or not
// published, a contract that isn't approved, an integration that is off or not allowed in the space)
// answered as ok false (paylink.EventTargetChecker).
func (s *Service) EventBinding(ctx context.Context, appID int64, slot string) (int64, bool, error) {
	b, err := s.Binding(ctx, appID, slot)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrOffline), errors.Is(err, ErrNotPublished), errors.Is(err, ErrIntegrationUnapproved):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return b.IntegrationID, true, nil
}

// WriteEvent writes the event to the record. Its failures are *paylink.Error, except those of
// w.Settle and w.Done, which are returned as they are.
func (s *Service) WriteEvent(ctx context.Context, w paylink.EventWrite) (paylink.EventResult, error) {
	var spaceID, workspaceID int64
	err := s.db.QueryRowContext(ctx, `SELECT a.space_id, sp.workspace_id FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`, w.AppID).
		Scan(&spaceID, &workspaceID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && workspaceID != w.WorkspaceID {
		// An app that is gone, or in another workspace than the integration's: nothing of it is touched.
		return paylink.EventResult{}, eventFailure(ErrNotFound)
	}
	if err != nil {
		return paylink.EventResult{}, err
	}

	var binding Binding
	var noHandler, unmet bool
	write := SystemWrite{
		Actor: SystemActor{Kind: SystemKindIntegration, ID: w.IntegrationID}, SpaceID: spaceID, AppID: w.AppID, RecordID: w.RecordID,
		Action: w.Event, // the normalized event is the history's action
		Guard: func(ctx context.Context, tx *sql.Tx) error {
			b, err := liveBinding(ctx, tx, w.AppID, w.Slot)
			if err != nil {
				return err
			}
			if b.IntegrationID != w.IntegrationID {
				return &paylink.Error{Code: apierr.CodeIntegrationEventIntegrationChanged}
			}
			binding = b
			if w.Settle != nil {
				if err := w.Settle(ctx, tx); err != nil {
					return &settleError{err}
				}
			}
			return nil
		},
	}
	decide := func(def Definition, fields map[string]any) (systemDecision, error) {
		i := slices.IndexFunc(binding.Contract.Handlers, func(h ContractHandler) bool { return h.Event == w.Event })
		if i < 0 {
			noHandler = true
			return systemDecision{skip: true}, nil
		}
		for _, r := range binding.Contract.OnlyIf {
			if r.Event == w.Event && !def.Collection.holds(r.Rule, fields) {
				unmet = true
				return systemDecision{skip: true}, nil
			}
		}
		d := systemDecision{set: map[string]any{}}
		for _, set := range binding.Contract.Handlers[i].Sets {
			d.declared = append(d.declared, set.Field)
			f, ok := def.Collection.field(set.Field)
			if !ok {
				return systemDecision{}, &systemFieldError{field: set.Field, why: "isn't in the app any more"}
			}
			v, ok, err := eventValue(f, set, w)
			if err != nil {
				return systemDecision{}, err
			}
			if ok {
				d.set[set.Field] = v
			}
		}
		if len(d.set) == 0 {
			return systemDecision{skip: true}, nil // every source it names is one the event doesn't have
		}
		return d, nil
	}

	var res SystemResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		var err error
		if res, err = s.systemWriteTx(ctx, tx, write, decide); err != nil {
			return err
		}
		if w.Done != nil {
			if err := w.Done(ctx, tx, paylink.EventResult{Written: res.Changed, NoHandler: noHandler, ConditionUnmet: unmet}); err != nil {
				return &settleError{err}
			}
		}
		return nil
	})
	var se *settleError
	switch {
	case errors.As(err, &se):
		return paylink.EventResult{}, se.err
	case err != nil:
		return paylink.EventResult{}, eventFailure(err)
	}
	return paylink.EventResult{Written: res.Changed, NoHandler: noHandler, ConditionUnmet: unmet}, nil
}

// settleError carries the integrations' own failure of Settle through the write unchanged.
type settleError struct{ err error }

func (e *settleError) Error() string { return e.err.Error() }
func (e *settleError) Unwrap() error { return e.err }

// eventFailure turns what a write of an event refused with into the code the admins are told.
func eventFailure(err error) error {
	var pe *paylink.Error
	switch {
	case errors.As(err, &pe):
		return pe
	case errors.Is(err, ErrNotFound):
		return &paylink.Error{Code: apierr.CodeIntegrationEventRecordGone}
	case errors.Is(err, ErrOffline), errors.Is(err, ErrReadOnly), errors.Is(err, ErrNotPublished):
		return &paylink.Error{Code: apierr.CodeIntegrationEventAppOffline}
	case errors.Is(err, ErrIntegrationUnapproved):
		return &paylink.Error{Code: apierr.CodeIntegrationEventUnavailable}
	case errors.Is(err, ErrSystemField), errors.Is(err, ErrInvalid), errors.Is(err, ErrFull):
		// The record, as the event would leave it, doesn't fit the app, or the contract names what a
		// system write may not set.
		return &paylink.Error{Code: apierr.CodeIntegrationEventRecordRefused}
	}
	return err
}

// eventValue is what the handler's set puts in the field, for the event. ok is false for a source
// the event doesn't have: it is left out, as the rest of the event is still true.
func eventValue(f Field, set ContractSet, w paylink.EventWrite) (value any, ok bool, err error) {
	if set.From == "" {
		var v any
		if err := json.Unmarshal(set.Value, &v); err != nil {
			return nil, false, err
		}
		return v, true, nil
	}
	switch set.From {
	case SourceOccurredAt, SourcePaidAt:
		at := w.OccurredAt.UTC()
		if f.Type == TypeDate {
			return at.Format(time.DateOnly), true, nil
		}
		return at.Format(time.RFC3339), true, nil
	case SourceAmount:
		text := currency.Format(w.AmountMinor, w.Currency)
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, false, err
		}
		return n, true, nil
	case SourceCurrency:
		return w.Currency, true, nil
	case SourcePaymentID:
		return w.PaymentID, w.PaymentID != "", nil
	}
	// receipt_url: Stripe's checkout events don't carry one, and the adapter keeps no text a
	// payer's side could have written.
	return nil, false, nil
}
