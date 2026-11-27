package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
	// matter for an admin: the record no longer meets the action's condition, and nothing was made.
	if !slices.ContainsFunc(b.Contract.Links, func(l ContractLink) bool {
		return asks(l) && (l.When == nil || ap.liveDef.Collection.holds(*l.When, data))
	}) {
		return paymentGrant{}, refuse(apierr.CodeIntegrationConditionChanged)
	}
	// The money the entry asks is the record's still. It can't change while an entry waits to run
	// (paymentMoneyLocked), but one that failed and is retried after the record's amount or currency
	// changed would ask for a link at the old money, which the invoice no longer says.
	if amount, code := p.moneyEffect().moneyOf(ap.liveDef.Collection, data); amount != p.Amount || code != p.Currency {
		return paymentGrant{}, refuse(apierr.CodeIntegrationAmountChanged)
	}
	return paymentGrant{spaceID: spaceID, workspaceID: workspaceID, binding: b}, nil
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
		Slot: p.Slot, URLField: p.URLField, AmountField: p.AmountField, CurrencyField: p.CurrencyField, Amount: p.Amount, Currency: p.Currency, Description: p.Description,
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

// moneyEffect is the effect as far as its money goes: where the entry's amount and currency were read.
func (p payload) moneyEffect() Effect {
	e := Effect{Kind: EffectCreatePaymentLink, AmountField: p.AmountField, CurrencyField: p.CurrencyField}
	if p.CurrencyField == "" {
		e.Currency = p.Currency
	}
	return e
}

// paymentMoneyLocked refuses a change of the record's money (the amount or the currency a payment
// link effect of the app reads) while a payment link of the record may still be paid: an entry that
// waits to run asks the money it was queued with, and a link that is open, still being made or whose
// outcome is unknown asks the money it was made for. A payer would pay the old amount for an invoice
// that says another, and the payment would settle it. Once the link is confirmed expired (or paid on
// that very link), the money may change, and the next link asks the new one. Asked of the change's
// own transaction, under the write lock, so no entry or link begins between the check and the change.
// Every write of a record asks it: a person's, an action's, the agent's and the system's (an event's
// handler, system.go).
func paymentMoneyLocked(ctx context.Context, tx *sql.Tx, payments paylink.Opener, appID, recordID int64, def Definition, before, after map[string]any) error {
	locked, err := lockedMoney(ctx, tx, payments, appID, recordID, def)
	if err != nil {
		return err
	}
	for _, f := range locked[recordID] {
		e := Effect{Kind: EffectCreatePaymentLink, AmountField: f.Amount, CurrencyField: f.Currency}
		wasAmount, wasCode := e.moneyOf(def.Collection, before)
		nowAmount, nowCode := e.moneyOf(def.Collection, after)
		changed := ""
		// What the field holds is compared too, not only the money read from it: a release that changed
		// the field's type (an acknowledged change) reads no amount from it at all, and a change of its
		// value is a change of the link's money all the same.
		if wasAmount != nowAmount || !sameValue(before[f.Amount], after[f.Amount]) {
			changed = f.Amount
		} else if f.Currency != "" && (wasCode != nowCode || !sameValue(before[f.Currency], after[f.Currency])) {
			changed = f.Currency
		}
		if changed != "" {
			return invalidAs(apierr.CodeAppPaymentLinkOpen, apierr.Params{"field": changed},
				"%s can't change while the record's payment link may still be paid: change it once the link has expired", changed)
		}
	}
	return nil
}

// paymentMoneyKept refuses a release (published, or rolled back to) that would read the money of a
// record whose payment link may still be paid otherwise than the live release does: a field retyped,
// a choice of currencies without the one the link asks, a bound that changes what is read. The link
// asks what the live release read, and the payment of it settles the invoice the new one describes.
// The fields are those the links and the entries that wait were read from, and those the live
// release's effects read. A release may read money from other fields too: a new link asks those.
func paymentMoneyKept(ctx context.Context, tx *sql.Tx, payments paylink.Opener, appID int64, live *Definition, next Definition) error {
	if live == nil {
		return nil // nothing was ever live: no link was asked of it
	}
	locked, err := lockedMoney(ctx, tx, payments, appID, 0, *live)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(locked)) {
		var raw string
		switch err := tx.QueryRowContext(ctx, `SELECT data FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, id, appID).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return err
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return err
		}
		for _, f := range locked[id] {
			e := Effect{Kind: EffectCreatePaymentLink, AmountField: f.Amount, CurrencyField: f.Currency}
			wasAmount, wasCode := e.moneyOf(live.Collection, data)
			nowAmount, nowCode := e.moneyOf(next.Collection, data)
			field := ""
			if wasAmount != nowAmount {
				field = f.Amount
			} else if wasCode != nowCode {
				field = f.Currency
			}
			if field != "" {
				return apierr.New(http.StatusUnprocessableEntity, apierr.CodeAppPaymentLinkOpen,
					fmt.Sprintf("%s can't be read otherwise while a record's payment link may still be paid: publish this once the link has expired", field),
					apierr.Params{"field": field})
			}
		}
	}
	return nil
}

// lockedMoney is, for each record of the app (recordID 0: every one) whose money is locked, the fields
// it is locked in: those of the entries that wait to run, of the links that may still be paid, and of
// the payment link effects of def. A link that doesn't say which fields it was read from (made before
// links kept them, its entry cleaned up since) locks every field any release of the app read money
// from, and def's.
func lockedMoney(ctx context.Context, tx *sql.Tx, payments paylink.Opener, appID, recordID int64, def Definition) (map[int64][]paylink.MoneyFields, error) {
	locked := map[int64][]paylink.MoneyFields{}
	rows, err := tx.QueryContext(ctx, `SELECT record_id, coalesce(json_extract(payload, '$.amount_field'), ''), coalesce(json_extract(payload, '$.currency_field'), '')
		FROM app_effects WHERE app_id = ? AND (? = 0 OR record_id = ?) AND kind = ? AND state IN ('pending', 'running')`,
		appID, recordID, recordID, EffectCreatePaymentLink)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var f paylink.MoneyFields
		if err := rows.Scan(&id, &f.Amount, &f.Currency); err != nil {
			_ = rows.Close()
			return nil, err
		}
		locked[id] = append(locked[id], f)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if payments != nil {
		linked, err := payments.PayableMoney(ctx, tx, appID, recordID)
		if err != nil {
			return nil, err
		}
		for id, fields := range linked {
			locked[id] = append(locked[id], fields...)
		}
	}
	if len(locked) == 0 {
		return locked, nil
	}
	own := moneyFieldsOf(def)
	var every []paylink.MoneyFields
	for id, fields := range locked {
		out := slices.Clone(own)
		for _, f := range fields {
			if f.Amount != "" {
				out = append(out, f)
				continue
			}
			if every == nil {
				if every, err = releasesMoneyFields(ctx, tx, appID); err != nil {
					return nil, err
				}
			}
			out = append(out, every...)
		}
		slices.SortFunc(out, func(a, b paylink.MoneyFields) int {
			return strings.Compare(a.Amount+"\x00"+a.Currency, b.Amount+"\x00"+b.Currency)
		})
		locked[id] = slices.Compact(out)
	}
	return locked, nil
}

// moneyFieldsOf are the fields the payment link effects of def read their money from.
func moneyFieldsOf(def Definition) []paylink.MoneyFields {
	var out []paylink.MoneyFields
	for _, name := range slices.Sorted(maps.Keys(def.Actions)) {
		for _, e := range def.Actions[name].Effects {
			if e.Kind == EffectCreatePaymentLink {
				out = append(out, paylink.MoneyFields{Amount: e.AmountField, Currency: e.CurrencyField})
			}
		}
	}
	return out
}

// releasesMoneyFields are the fields the payment link effects of every release of the app read their
// money from: what a link that doesn't say its own may have been read from (never nil, so it is read
// once per check).
func releasesMoneyFields(ctx context.Context, tx *sql.Tx, appID int64) ([]paylink.MoneyFields, error) {
	rows, err := tx.QueryContext(ctx, `SELECT definition FROM app_releases WHERE app_id = ? ORDER BY number`, appID)
	if err != nil {
		return nil, err
	}
	out := []paylink.MoneyFields{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var def Definition
		if err := json.Unmarshal([]byte(raw), &def); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, moneyFieldsOf(def)...)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// sameValue says whether two values of a record's field are the same as JSON: 120 read from the
// database and 120 from a request are, whatever their Go types.
func sameValue(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}
