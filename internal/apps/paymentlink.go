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
	g := paymentGrant{spaceID: spaceID, workspaceID: workspaceID, binding: b}
	if !slices.ContainsFunc(b.Contract.Links, func(l ContractLink) bool {
		return asks(l) && (l.When == nil || ap.liveDef.Collection.holds(*l.When, data))
	}) {
		return g, refuse(apierr.CodeIntegrationConditionChanged)
	}
	if amount, code := p.moneyEffect().moneyOf(ap.liveDef.Collection, data); amount != p.Amount || code != p.Currency {
		return paymentGrant{}, refuse(apierr.CodeIntegrationAmountChanged)
	}
	return g, nil
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
	lookupOnly := false
	if isCode(err, apierr.CodeIntegrationConditionChanged) {
		res, resolve, uerr := s.paymentConditionUnmet(ctx, e, g)
		if !resolve {
			return res, uerr
		}
		err, lookupOnly = nil, true
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
		Slot: p.Slot, URLField: p.URLField, AmountField: p.AmountField, CurrencyField: p.CurrencyField, Amount: p.Amount, Currency: p.Currency, Description: p.Description,
		LookupOnly: lookupOnly,
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
		return nil, refuse(apierr.CodeAppEffectInvalid)
	case err != nil:
		return nil, err
	}
	return map[string]any{
		"workspace_id": workspaceID, "space_id": spaceID,
		"amount": currency.Format(link.AmountMinor, link.Currency), "currency": link.Currency,
	}, nil
}

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

func isCode(err error, code apierr.Code) bool {
	var ee *effectError
	return errors.As(err, &ee) && ee.code == code
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

func (p payload) moneyEffect() Effect {
	e := Effect{Kind: EffectCreatePaymentLink, AmountField: p.AmountField, CurrencyField: p.CurrencyField}
	if p.CurrencyField == "" {
		e.Currency = p.Currency
	}
	return e
}

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

func paymentMoneyKept(ctx context.Context, tx *sql.Tx, payments paylink.Opener, appID int64, live *Definition, next Definition) error {
	if live == nil {
		return nil
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

func sameValue(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}
