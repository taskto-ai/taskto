package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/currency"
	"taskto.ai/taskto/internal/paylink"
)

func (s *Service) WithPaymentLinks(p paylink.Opener) *Service {
	s.platform.Payments = p
	return s
}

type paymentGrant struct {
	spaceID, workspaceID int64
	binding              Binding
}

func (s *Service) paymentGrantOf(ctx context.Context, q queryer, e effectRow, spaceID, workspaceID int64) (paymentGrant, error) {
	p := e.payload
	b, err := liveBinding(ctx, q, e.appID, p.Slot)
	switch {
	case errors.Is(err, ErrOffline):
		return paymentGrant{}, refuse(apierr.CodeAppEffectAppOffline)
	case errors.Is(err, ErrNotFound):
		return paymentGrant{}, refuse(apierr.CodeAppEffectAppAccess)
	case errors.Is(err, ErrIntegrationUnapproved), errors.Is(err, ErrNotPublished):
		return paymentGrant{}, refuse(apierr.CodeIntegrationUnavailable)
	case err != nil:
		return paymentGrant{}, err
	}
	ap, err := launchable(ctx, q, spaceID, e.appID)
	switch {
	case errors.Is(err, ErrNotFound):
		return paymentGrant{}, refuse(apierr.CodeAppEffectAppAccess)
	case errors.Is(err, ErrOffline):
		return paymentGrant{}, refuse(apierr.CodeAppEffectAppOffline)
	case err != nil:
		return paymentGrant{}, err
	case ap.liveDef == nil:
		return paymentGrant{}, refuse(apierr.CodeIntegrationUnavailable)
	}
	var raw string
	switch err := q.QueryRowContext(ctx, `SELECT data FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, e.recordID, e.appID).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return paymentGrant{}, refuse(apierr.CodeAppEffectRecordGone)
	case err != nil:
		return paymentGrant{}, err
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return paymentGrant{}, err
	}
	asks := func(l ContractLink) bool {
		inCurrency := (l.Currency != "" && l.Currency == p.Currency) || (l.Currency == "" && slices.Contains(l.CurrencyOptions, p.Currency))
		described := (!p.DescribesID || l.DescribesID) && !slices.ContainsFunc(p.Describes, func(f string) bool { return !slices.Contains(l.Describes, f) })
		return l.Action == e.action && l.ActionKind == e.actionKind && l.AmountField == p.AmountField && l.CurrencyField == p.CurrencyField &&
			l.URLField == p.URLField && l.IDField == p.IDField && inCurrency && described && amountWithinBounds(b.Contract, l.AmountField, p.Amount)
	}
	if !slices.ContainsFunc(b.Contract.Links, asks) {
		return paymentGrant{}, refuse(apierr.CodeIntegrationUnavailable)
	}
	if !slices.ContainsFunc(b.Contract.Links, func(l ContractLink) bool {
		return asks(l) && (l.When == nil || ap.liveDef.Collection.holds(*l.When, data))
	}) {
		return paymentGrant{}, refuse(apierr.CodeIntegrationConditionChanged)
	}
	return paymentGrant{spaceID: spaceID, workspaceID: workspaceID, binding: b}, nil
}

func amountWithinBounds(c Contract, field, amount string) bool {
	v, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return true
	}
	for _, f := range c.Fields {
		if f.Name != field {
			continue
		}
		if (f.Min != nil && v < *f.Min) || (f.Max != nil && v > *f.Max) {
			return false
		}
	}
	return true
}

func (s *Service) paymentSnapshot(ctx context.Context, e effectRow) (paymentGrant, error) {
	var g paymentGrant
	err := s.access.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
		var spaceID, workspaceID int64
		if err := tx.QueryRowContext(ctx, `SELECT a.space_id, sp.workspace_id FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`,
			e.appID).Scan(&spaceID, &workspaceID); errors.Is(err, sql.ErrNoRows) {
			return refuse(apierr.CodeAppEffectAppAccess)
		} else if err != nil {
			return err
		}
		if s.hooks.afterPaymentSnapshotRead != nil {
			s.hooks.afterPaymentSnapshotRead()
		}
		if e.payload.SpaceID != spaceID {
			return refuse(apierr.CodeAppEffectSpaceChanged)
		}
		if err := s.authorizeAppOn(ctx, tx, snap, e, spaceID); err != nil {
			return err
		}
		var err error
		g, err = s.paymentGrantOf(ctx, tx, e, spaceID, workspaceID)
		return err
	})
	return g, err
}

func (s *Service) createPaymentLink(ctx context.Context, e effectRow) (map[string]any, error) {
	if s.platform.Payments == nil {
		return nil, errors.New("payment links are not available in this server")
	}
	p := e.payload
	g, err := s.paymentSnapshot(ctx, e)
	if err != nil {
		return nil, err
	}
	spaceID, workspaceID, b := g.spaceID, g.workspaceID, g.binding
	superseded := map[string]any{"workspace_id": workspaceID, "space_id": spaceID, "superseded": true}
	if newer, err := s.paymentLinkSuperseded(ctx, s.db, e); err != nil {
		return nil, err
	} else if newer {
		return superseded, nil
	}
	link, err := s.platform.Payments.OpenPaymentLink(ctx, paylink.Request{
		WorkspaceID: workspaceID, IntegrationID: b.IntegrationID, SpaceID: spaceID, AppID: e.appID, RecordID: e.recordID, EffectID: e.id,
		Slot: p.Slot, Amount: p.Amount, Currency: p.Currency, Description: p.Description,
	})
	if err != nil {
		var pe *paylink.Error
		if errors.As(err, &pe) {
			return nil, &effectError{code: pe.Code, params: pe.Params, transient: pe.Transient, wait: pe.Wait}
		}
		return nil, err
	}
	_, err = s.SystemWrite(ctx, SystemWrite{
		Actor: SystemActor{Kind: SystemKindIntegration, ID: b.IntegrationID}, SpaceID: spaceID, AppID: e.appID, RecordID: e.recordID,
		Action:   "payment_link.created",
		Declared: []string{p.URLField, p.IDField},
		Set:      map[string]any{p.URLField: link.URL, p.IDField: link.ObjectID},
		Guard: func(ctx context.Context, tx *sql.Tx) error {
			if newer, err := s.paymentLinkSuperseded(ctx, tx, e); err != nil {
				return err
			} else if newer {
				return errSuperseded
			}
			if err := s.authorizeAppOn(ctx, tx, s.access.On(tx), e, spaceID); err != nil {
				return err
			}
			now, err := s.paymentGrantOf(ctx, tx, e, spaceID, workspaceID)
			if err != nil {
				return err
			}
			if now.binding.IntegrationID != b.IntegrationID {
				return refuse(apierr.CodeIntegrationChanged)
			}
			return nil
		},
	})
	switch {
	case errors.Is(err, errSuperseded):
		return superseded, nil
	case errors.Is(err, ErrNotFound):
		return nil, refuse(apierr.CodeAppEffectRecordGone)
	case errors.Is(err, ErrOffline), errors.Is(err, ErrReadOnly):
		return nil, refuse(apierr.CodeAppEffectAppOffline)
	case errors.Is(err, ErrNotPublished), errors.Is(err, ErrSystemField):
		return nil, refuse(apierr.CodeIntegrationUnavailable)
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrFull):
		return nil, refuse(apierr.CodeAppEffectInvalid)
	case err != nil:
		return nil, err
	}
	return map[string]any{
		"workspace_id": workspaceID, "space_id": spaceID,
		"amount": currency.Format(link.AmountMinor, link.Currency), "currency": link.Currency,
	}, nil
}

func previewPaymentLink(p payload) (EffectDetail, apierr.Code) {
	d := EffectDetail{Currency: p.Currency}
	if p.Currency == "" || !currency.Known(p.Currency) {
		return d, apierr.CodeIntegrationCurrencyRefused
	}
	minor, err := currency.Minor(p.Amount, p.Currency)
	if err != nil {
		return d, apierr.CodeIntegrationAmountRefused
	}
	d.Amount = currency.Format(minor, p.Currency)
	return d, ""
}

var errSuperseded = errors.New("a newer payment link entry replaced this one")

func (s *Service) paymentLinkSuperseded(ctx context.Context, q queryer, e effectRow) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM app_payment_link_latest
		WHERE app_id = ? AND record_id = ? AND field IN (?, ?) AND effect_id > ?`,
		e.appID, e.recordID, e.payload.URLField, e.payload.IDField, e.id).Scan(&n)
	return n > 0, err
}

func notePaymentLinkEntry(ctx context.Context, tx *sql.Tx, appID, recordID, effectID int64, p payload) error {
	for _, field := range []string{p.URLField, p.IDField} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_payment_link_latest (app_id, record_id, field, effect_id) VALUES (?, ?, ?, ?)
			ON CONFLICT (app_id, record_id, field) DO UPDATE SET effect_id = excluded.effect_id
			WHERE excluded.effect_id > app_payment_link_latest.effect_id`, appID, recordID, field, effectID); err != nil {
			return err
		}
	}
	return nil
}
