package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/currency"
	"taskto.ai/taskto/internal/paylink"
)

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

func (s *Service) WriteEvent(ctx context.Context, w paylink.EventWrite) (paylink.EventResult, error) {
	var spaceID, workspaceID int64
	err := s.db.QueryRowContext(ctx, `SELECT a.space_id, sp.workspace_id FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`, w.AppID).
		Scan(&spaceID, &workspaceID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && workspaceID != w.WorkspaceID {
		return paylink.EventResult{}, eventFailure(ErrNotFound)
	}
	if err != nil {
		return paylink.EventResult{}, err
	}

	var binding Binding
	var noHandler, unmet bool
	write := SystemWrite{
		Actor: SystemActor{Kind: SystemKindIntegration, ID: w.IntegrationID}, SpaceID: spaceID, AppID: w.AppID, RecordID: w.RecordID,
		Action: w.Event,
		Guard: func(ctx context.Context, tx *sql.Tx) error {
			b, err := eventBinding(ctx, tx, w.AppID, w.Slot, w.IntegrationID)
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
			return systemDecision{skip: true}, nil
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

func eventBinding(ctx context.Context, q queryer, appID int64, slot string, integrationID int64) (Binding, error) {
	b, err := liveBinding(ctx, q, appID, slot)
	if !errors.Is(err, ErrIntegrationUnapproved) {
		return b, err
	}
	var live sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT live_release FROM apps WHERE id = ?`, appID).Scan(&live); err != nil || !live.Valid {
		return Binding{}, err
	}
	rel, rerr := loadRelease(ctx, q, appID, live.Int64, true)
	if rerr != nil {
		return Binding{}, rerr
	}
	if _, declared := rel.Definition.Integrations[slot]; declared {
		return Binding{}, err
	}
	var found []Binding
	for _, other := range slices.Sorted(maps.Keys(rel.Definition.Integrations)) {
		ob, oerr := liveBinding(ctx, q, appID, other)
		if oerr == nil && ob.IntegrationID == integrationID && ob.Kind == KindPayments {
			found = append(found, ob)
		} else if oerr != nil && !errors.Is(oerr, ErrIntegrationUnapproved) {
			return Binding{}, oerr
		}
	}
	if len(found) != 1 {
		return Binding{}, err
	}
	return found[0], nil
}

type settleError struct{ err error }

func (e *settleError) Error() string { return e.err.Error() }
func (e *settleError) Unwrap() error { return e.err }

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
		return &paylink.Error{Code: apierr.CodeIntegrationEventRecordRefused}
	}
	return err
}

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
	return nil, false, nil
}
