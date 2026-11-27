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

// paymentGrant is what the live release approves for an entry, as of one moment: where the app is,
// and the approved binding of the slot that asks for exactly this link.
type paymentGrant struct {
	spaceID, workspaceID int64
	binding              Binding
}

// paymentGrantOf reads, through q (a snapshot, or the transaction a write holds the lock in), what the
// live release approves for the entry: the app's space and workspace, the binding of its slot (the
// app is online, the release declares the slot and its contract is approved, the integration is on
// and allowed in the space), and that the contract has a link that asks for exactly what the entry
// asks, as of the live record.
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
	// The live release's collection and record, from the same read as the binding: what the contract's
	// rule and bounds are held against.
	ap, err := launchable(ctx, q, spaceID, e.appID)
	switch {
	case errors.Is(err, ErrNotFound):
		return paymentGrant{}, refuse(apierr.CodeAppEffectAppAccess) // moved to another space
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
	// The live release must still ask for this very link: the approved contract holds the action and
	// what it does (who may trigger it), the amount, its bounds, the currency and where it is read from,
	// the rule the record must meet, and where the link goes, and an entry queued by an older release
	// that asked for anything else has no authority now.
	// What the description sends out is part of what an admin approved: the entry's description was
	// filled when it was queued, from the fields its release named, and the live contract must still
	// name each of them (and the id, if the text holds it). Wording alone is not in the contract.
	asks := func(l ContractLink) bool {
		inCurrency := (l.Currency != "" && l.Currency == p.Currency) || (l.Currency == "" && slices.Contains(l.CurrencyOptions, p.Currency))
		described := (!p.DescribesID || l.DescribesID) && !slices.ContainsFunc(p.Describes, func(f string) bool { return !slices.Contains(l.Describes, f) })
		return l.Action == e.action && l.ActionKind == e.actionKind && l.AmountField == p.AmountField && l.CurrencyField == p.CurrencyField &&
			l.URLField == p.URLField && l.IDField == p.IDField && inCurrency && described && amountWithinBounds(b.Contract, l.AmountField, p.Amount)
	}
	if !slices.ContainsFunc(b.Contract.Links, asks) {
		return paymentGrant{}, refuse(apierr.CodeIntegrationUnavailable)
	}
	// The link is still approved as it was asked; what has changed is the record, which an ordinary
	// edit may have made fail the rule (`when`) of every link that asks for exactly this. That is no
	// matter for an admin: the record no longer meets the action's condition. The grant is returned with
	// the refusal: whether anything was made by then is the caller's to say (paymentConditionUnmet).
	g := paymentGrant{spaceID: spaceID, workspaceID: workspaceID, binding: b}
	if !slices.ContainsFunc(b.Contract.Links, func(l ContractLink) bool {
		return asks(l) && (l.When == nil || ap.liveDef.Collection.holds(*l.When, data))
	}) {
		return g, refuse(apierr.CodeIntegrationConditionChanged)
	}
	return g, nil
}

// amountWithinBounds reports whether the amount (decimal text) is within the bounds the live
// contract states for the amount's field: an amount of an older release, above a maximum the admin
// approved since, is not what was approved. An amount that isn't a number is the integration's to
// refuse, and a field with no bound has none.
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

// paymentSnapshot is the authorization of an entry before the provider is asked, as of one moment:
// the app's place, who may do what, and what the live release approves are all read in one read
// transaction, access included (through a checker over that transaction), so no check mixes the
// state of one instant with another's. The app must still be in the space the entry was queued in: a
// move clears an app's approvals, and what an admin approves again in the new space is not what the
// entry asked of the old one.
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

// createPaymentLink does the entry of a create_payment_link effect.
func (s *Service) createPaymentLink(ctx context.Context, e effectRow) (map[string]any, error) {
	if s.platform.Payments == nil {
		return nil, errors.New("payment links are not available in this server")
	}
	p := e.payload
	g, err := s.paymentSnapshot(ctx, e)
	if isCode(err, apierr.CodeIntegrationConditionChanged) {
		res, resolve, uerr := s.paymentConditionUnmet(ctx, e, g)
		if !resolve {
			return res, uerr
		}
		// The entry's reference may hold an object nobody has seen: the entry stays on its key's lookup
		// (the integration's side looks for it, and waits, and fails for an admin), and the write's own
		// check of the condition then says the link was made and kept.
		err = nil
	}
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
		// In the write's own transaction, under the lock the change is made with: the provider took
		// time, and an admin, the user's rights or the app may have changed in it. Nothing is written
		// unless the entry is still the newest, the user still may, and the live release still approves
		// this link with the integration that made it (as createCard checks, in its own).
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
			// And of the mode the integration's key has now: an admin who replaced the key while the
			// call was out leaves a link of the old mode, which is not this record's to hold.
			if err := s.platform.Payments.LinkCurrent(ctx, tx, b.IntegrationID, link.ObjectID); err != nil {
				var pe *paylink.Error
				if errors.As(err, &pe) {
					return &effectError{code: pe.Code, params: pe.Params, transient: pe.Transient}
				}
				return err
			}
			return nil
		},
	})
	switch {
	case isCode(err, apierr.CodeIntegrationConditionChanged):
		// The record stopped meeting the condition while the provider made the link: it exists, and
		// stays with the entry's reference (a payment on it is still recorded); a retry once the record
		// qualifies again writes it. Never "nothing was created".
		return nil, refuse(apierr.CodeIntegrationConditionChangedLinkKept)
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

// paymentConditionUnmet is the entry's end when the record doesn't meet the action's condition before
// the provider is asked (g is the grant as of then, all else approved). Whether anything was made is
// read from the integration's side, never assumed:
//
//   - The entry's own link is on the record already: an entry that wrote it and stopped before it was
//     noted done (a `when` rule may read the link's own field, "no link yet", which the write made
//     false). It is done, with nothing asked of anyone.
//   - The entry made a link the record doesn't hold (the record stopped meeting the condition while the
//     provider made it): it is kept with the entry, and the entry says so.
//   - The entry's reference may hold an object nobody has seen (a call whose outcome is unknown): the
//     entry goes on as if the condition held (resolve), so it keeps looking for that object, which the
//     write then keeps with the reference (integration_condition_changed_link_kept).
//   - Otherwise nothing was made: integration_condition_changed.
func (s *Service) paymentConditionUnmet(ctx context.Context, e effectRow, g paymentGrant) (res map[string]any, resolve bool, err error) {
	link, pending, err := s.platform.Payments.EntryLink(ctx, s.db, e.id)
	if err != nil {
		return nil, false, err
	}
	if link.ObjectID == "" {
		if pending {
			return nil, true, nil
		}
		return nil, false, refuse(apierr.CodeIntegrationConditionChanged)
	}
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT data FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, e.recordID, e.appID).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, refuse(apierr.CodeAppEffectRecordGone)
	case err != nil:
		return nil, false, err
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, false, err
	}
	if url, _ := data[e.payload.URLField].(string); url == link.URL {
		if id, _ := data[e.payload.IDField].(string); id == link.ObjectID {
			return map[string]any{
				"workspace_id": g.workspaceID, "space_id": g.spaceID,
				"amount": currency.Format(link.AmountMinor, link.Currency), "currency": link.Currency,
			}, false, nil
		}
	}
	return nil, false, refuse(apierr.CodeIntegrationConditionChangedLinkKept)
}

// isCode says whether err is an entry's refusal with code.
func isCode(err error, code apierr.Code) bool {
	var ee *effectError
	return errors.As(err, &ee) && ee.code == code
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
// order entries are queued and never again) asks for a payment link of the same record into either
// of the same two fields, in any state: that entry's link is the record's, and this one's must never
// replace it. It is read from the table that keeps the newest entry of each field (the entries
// themselves are cleaned up, a done one after 30 days, and an older one tried again later must
// still find it has been replaced).
func (s *Service) paymentLinkSuperseded(ctx context.Context, q queryer, e effectRow) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM app_payment_link_latest
		WHERE app_id = ? AND record_id = ? AND field IN (?, ?) AND effect_id > ?`,
		e.appID, e.recordID, e.payload.URLField, e.payload.IDField, e.id).Scan(&n)
	return n > 0, err
}

// notePaymentLinkEntry records that the entry is the newest to write each of its two fields of the
// record, in the transaction that queues it. Only forward: an entry never replaces a newer one's mark.
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
