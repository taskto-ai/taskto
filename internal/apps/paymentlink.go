package apps

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/currency"
	"taskto.ai/taskto/internal/paylink"
)

// The create_payment_link effect (docs/integrations.md).
//
// When its entry runs, as the user who acted, the worker checks what the action and the effect
// need of that user (authorizeApp), then that the app's live release still has an approved
// contract for the slot that asks for exactly this link, and that the integration it was approved
// with is switched on and allowed in the app's space (Binding): an admin who revokes the approval,
// switches the integration off or takes the space away stops the entry at once. Then it asks the
// integration's side (paylink.Opener) for the link, which is the one that checks the admin's limits
// (the ceiling, the currencies), keeps the reference that makes the call happen once, and talks to
// the provider with the integration's key, which never comes here. Last it writes the link and its
// id to the two fields the effect names through the system writer (system.go), limited to the
// fields of the approved contract and with no user behind it, so a record is never changed by more
// than the contract an admin read.
//
// The provider's answer and the record's change are two steps, not one transaction: the entry is
// done again after a crash between them, the integration finds the link it already made (it keeps
// its address with the reference), and writing it to the record again sets what is there already,
// which is no change and no new revision.
//
// The newest entry wins. An entry runs once its predecessors have, but one that is tried again
// later (after a failure, a crash or a retry by its user) could write its link after a newer entry
// asked for another amount of the same record and wrote its own: it would put a link for an
// earlier amount over the right one. So an entry is superseded when a newer entry exists for the
// same record and the same two fields, in any state: it asks the provider for nothing and writes
// nothing, and ends done (nothing is left for it to do). The check is made before the call and
// again, in the write's own transaction, just before the link is written (SystemWrite.Guard).

// WithPaymentLinks sets what makes payment links, and returns the service.
func (s *Service) WithPaymentLinks(p paylink.Opener) *Service {
	s.platform.Payments = p
	return s
}

// createPaymentLink does the entry of a create_payment_link effect.
func (s *Service) createPaymentLink(ctx context.Context, e effectRow) (map[string]any, error) {
	if s.platform.Payments == nil {
		return nil, errors.New("payment links are not available in this server")
	}
	p := e.payload
	if err := s.authorizeApp(ctx, e); err != nil {
		return nil, err
	}
	b, err := s.Binding(ctx, e.appID, p.Slot)
	switch {
	case errors.Is(err, ErrOffline):
		return nil, refuse(apierr.CodeAppEffectAppOffline)
	case errors.Is(err, ErrNotFound):
		return nil, refuse(apierr.CodeAppEffectAppAccess)
	case errors.Is(err, ErrIntegrationUnapproved), errors.Is(err, ErrNotPublished):
		return nil, refuse(apierr.CodeIntegrationUnavailable)
	case err != nil:
		return nil, err
	}
	// The live release must still ask for this very link: the approved contract holds the action,
	// the amount and where the link goes, and an entry queued by an older release that asked for
	// anything else has no authority now.
	// What the description sends out is part of what an admin approved: the entry's description was
	// filled when it was queued, from the fields its release named, and the live contract must still
	// name each of them (and the id, if the text holds it). Wording alone is not in the contract.
	asked := slices.ContainsFunc(b.Contract.Links, func(l ContractLink) bool {
		inCurrency := (l.Currency != "" && l.Currency == p.Currency) || (l.Currency == "" && slices.Contains(l.CurrencyOptions, p.Currency))
		described := (!p.DescribesID || l.DescribesID) && !slices.ContainsFunc(p.Describes, func(f string) bool { return !slices.Contains(l.Describes, f) })
		return l.Action == e.action && l.AmountField == p.AmountField && l.URLField == p.URLField && l.IDField == p.IDField && inCurrency && described
	})
	if !asked {
		return nil, refuse(apierr.CodeIntegrationUnavailable)
	}
	var spaceID, workspaceID int64
	if err := s.db.QueryRowContext(ctx, `SELECT a.space_id, sp.workspace_id FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`,
		e.appID).Scan(&spaceID, &workspaceID); err != nil {
		return nil, err
	}
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
			return nil, &effectError{code: pe.Code, params: pe.Params, transient: pe.Transient}
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
		// The record, as the link would leave it, doesn't fit the app (a field's limit, the app's size).
		return nil, refuse(apierr.CodeAppEffectInvalid)
	case err != nil:
		return nil, err
	}
	return map[string]any{
		"workspace_id": workspaceID, "space_id": spaceID,
		"amount": currency.Format(link.AmountMinor, link.Currency), "currency": link.Currency,
	}, nil
}

// previewPaymentLink is what a preview says of a payment link effect: the amount and the currency
// the sample record would ask for, and the code it would fail with if the amount can't be asked in
// that currency (the exact rules of the money, which need no integration). It never calls anyone,
// opens no key and reads no integration: the admin's ceiling and list of currencies are checked
// when the entry runs, and a draft has no integration yet.
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

// errSuperseded is the Guard's refusal of a write whose entry a newer one has replaced.
var errSuperseded = errors.New("a newer payment link entry replaced this one")

// paymentLinkSuperseded is whether a newer entry (a higher number, since numbers are given in the
// order entries are queued and never again) asks for a payment link of the same record into the
// same two fields, in any state: that entry's link is the record's, and this one's must never
// replace it.
func (s *Service) paymentLinkSuperseded(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, e effectRow) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM app_effects
		WHERE app_id = ? AND record_id = ? AND kind = ? AND id > ?
		  AND json_extract(payload, '$.url_field') = ? AND json_extract(payload, '$.id_field') = ?`,
		e.appID, e.recordID, EffectCreatePaymentLink, e.id, e.payload.URLField, e.payload.IDField).Scan(&n)
	return n > 0, err
}
