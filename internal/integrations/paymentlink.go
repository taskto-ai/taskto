package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/currency"
	"taskto.ai/taskto/internal/paylink"
)

const (
	PaymentDonePath      = "/payment/done"
	PaymentCancelledPath = "/payment/cancelled"
)

func unavailable() error {
	return &paylink.Error{Code: apierr.CodeIntegrationUnavailable}
}

func (s *Service) OpenPaymentLink(ctx context.Context, req paylink.Request) (paylink.Link, error) {
	rec, err := s.store.getSnapshot(ctx, req.WorkspaceID, req.IntegrationID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return paylink.Link{}, unavailable()
		}
		return paylink.Link{}, err
	}
	usable, err := s.store.usable(ctx, req.WorkspaceID, req.IntegrationID, req.SpaceID)
	if err != nil {
		return paylink.Link{}, err
	}
	provider, _ := Lookup(rec.Provider).(PaymentProvider)
	if !usable || provider == nil || !providerDoes(rec.Provider, KindPayments) {
		return paylink.Link{}, unavailable()
	}

	ref, found, err := s.refOf(ctx, req.EffectID)
	if err != nil {
		return paylink.Link{}, err
	}
	if found && (ref.appID != req.AppID || ref.recordID != req.RecordID) {
		return paylink.Link{}, unavailable()
	}
	if found && (ref.objectID != "" || paidWithoutLink(ref)) {
		return s.savedLink(ctx, rec, provider, ref, req)
	}
	if found && ref.state == "expired" {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	if found && ref.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if !found && req.LookupOnly {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationConditionChanged}
	}
	var pending *attempt
	if found {
		if a, ok, err := s.attemptOf(ctx, ref); err != nil {
			return paylink.Link{}, err
		} else if ok && a.unknown() {
			pending = &a
		}
	}

	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	minor, err := s.checkMoney(rec, req.Amount, cur)
	if err != nil {
		if pending == nil {
			return paylink.Link{}, err
		}
		minor, cur = ref.amountMinor, ref.currency
	}
	creds, err := s.store.credentials(ctx, req.WorkspaceID, req.IntegrationID)
	if err != nil || creds.APIKey == "" {
		var unreadable unreadableError
		if err == nil || errors.As(err, &unreadable) {
			return paylink.Link{}, unavailable()
		}
		return paylink.Link{}, err
	}

	client := s.Client(Lookup(rec.Provider), rec.Settings, s.clientOpts...)
	account, err := provider.AccountID(ctx, creds, client)
	if err != nil {
		return paylink.Link{}, s.failed(ctx, rec, pending, err)
	}
	success, cancel := s.pageURLs()
	mode := Lookup(rec.Provider).Mode(rec.Settings, creds.APIKey.Reveal())
	who := whose{account: account, mode: mode}
	for i := 0; ; i++ {
		var checked []int64
		if !found {
			var link paylink.Link
			var reuse bool
			if link, reuse, checked, err = s.earlierLink(ctx, provider, creds, client, rec, req, minor, cur, who); err != nil {
				return paylink.Link{}, err
			} else if reuse {
				return link, nil
			}
		}
		if s.beforeBeginRef != nil {
			s.beforeBeginRef()
		}
		ref, err = s.beginRef(ctx, req, minor, cur, who, success, cancel, checked)
		if errors.Is(err, errRecheck) && i < maxKeysPerTry {
			continue
		}
		if err != nil {
			if errors.Is(err, errRecheck) {
				return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
			}
			return paylink.Link{}, err
		}
		break
	}
	if ref.objectID != "" {
		return madeLink(ref, req)
	}

	since, err := time.Parse(timeFormat, ref.createdAt)
	if err != nil {
		return paylink.Link{}, err
	}
	t := try{s: s, provider: provider, creds: creds, client: client, rec: rec, req: req, since: since, hosted: ref.successURL == ""}
	for range maxKeysPerTry {
		a, err := s.currentAttempt(ctx, ref)
		if errors.Is(err, errRefMoved) {
			if ref, err = s.refByID(ctx, ref.id); err != nil {
				return paylink.Link{}, err
			}
			if ref.objectID != "" {
				return madeLink(ref, req)
			}
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
		}
		if err != nil {
			return paylink.Link{}, err
		}
		switch a.outcome {
		case attemptSettled:
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
		case attemptExpired, attemptAbandoned:
			if ref, err = s.nextAttempt(ctx, ref, a); err != nil {
				return paylink.Link{}, err
			}
			if ref.objectID != "" {
				return madeLink(ref, req)
			}
			if ref.keySeq == a.seq {
				slog.Warn("integrations: a payment link's key can't move on; an admin decides", "ref", ref.id)
				return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
			}
			continue
		case attemptMade:
			return s.refHolds(ctx, ref, req)
		}

		if a.unknown() {
			res, link, err := t.resolve(ctx, &ref, a)
			switch {
			case err != nil:
				return paylink.Link{}, err
			case res == resolvedOpen:
				return link, nil
			case res == resolvedExpired:
				continue
			}
			if !s.keyKept(a, provider) {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
		}

		if req.LookupOnly {
			if a.unknown() {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationConditionChanged}
		}
		refusedBy := func(err error) error {
			if a.unknown() {
				return s.stillUnknown(ctx, a)
			}
			return err
		}
		if ok, refused, err := s.writeAhead(ctx, ref, a, req, who); err != nil {
			return paylink.Link{}, err
		} else if refused != nil {
			return paylink.Link{}, refusedBy(refused)
		} else if !ok {
			if ref, err = s.refByID(ctx, ref.id); err != nil {
				return paylink.Link{}, err
			}
			if ref.objectID != "" {
				return madeLink(ref, req)
			}
			continue
		}
		if s.afterWriteAhead != nil {
			if err := s.afterWriteAhead(); err != nil {
				return paylink.Link{}, err
			}
		}
		if refused := s.limitsAllow(ctx, s.store.db, req); refused != nil {
			if a, err = s.noteNotExecuted(context.WithoutCancel(ctx), a); err != nil {
				return paylink.Link{}, err
			}
			if errors.As(refused, new(*paylink.Error)) {
				return paylink.Link{}, refusedBy(refused)
			}
			return paylink.Link{}, refused
		}
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if a.unknown() {
			start := time.Now()
			left := s.keyLeft(a, provider)
			if left <= 0 {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
			if s.beforeCall != nil {
				s.beforeCall()
			}
			callCtx, cancel = context.WithDeadline(ctx, start.Add(left))
		} else if s.beforeCall != nil {
			s.beforeCall()
		}
		made, err := provider.CreatePaymentLink(callCtx, creds, client, ref.newLink(a, req.Description))
		cancel()
		if err != nil {
			ce, ok := IsCall(err)
			if !ok {
				return paylink.Link{}, err
			}
			logFailure(rec, ce)
			if ce.NotExecuted {
				if a, err = s.noteNotExecuted(ctx, a); err != nil {
					return paylink.Link{}, err
				}
			} else if a, err = s.attemptByKey(ctx, a.refID, a.seq); err != nil {
				return paylink.Link{}, err
			}
			if a.unknown() {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
		}
		if s.afterCall != nil {
			if err := s.afterCall(); err != nil {
				return paylink.Link{}, err
			}
		}
		if err := s.noteObject(ctx, a, made); err != nil {
			return paylink.Link{}, err
		}
		res, link, err := t.settle(ctx, &ref, a, made)
		switch {
		case err != nil:
			return paylink.Link{}, err
		case res == resolvedOpen:
			return link, nil
		}
	}
	return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

type try struct {
	s        *Service
	provider PaymentProvider
	creds    Credentials
	client   *Client
	rec      record
	req      paylink.Request
	since    time.Time
	hosted   bool
}

type resolution int

const (
	unresolved resolution = iota
	resolvedOpen
	resolvedExpired
)

func (t try) resolve(ctx context.Context, r *ref, a attempt) (resolution, paylink.Link, error) {
	if a.objectID != "" && a.objectVerified {
		return t.settle(ctx, r, a, Link{ObjectID: a.objectID, URL: a.url})
	}
	seen := a.objectID != ""
	found, ok, expired, err := t.provider.FindPaymentLink(ctx, t.creds, t.client, r.newLink(a, t.req.Description), t.since)
	if err != nil {
		if found.ObjectID != "" {
			if err := t.s.noteSeen(ctx, a, found.ObjectID); err != nil {
				return unresolved, paylink.Link{}, err
			}
			seen = true
		}
		ce, isCall := IsCall(err)
		if isCall {
			switch ce.Kind {
			case CallSettled:
				logFailure(t.rec, ce)
				return unresolved, paylink.Link{}, t.s.settleAttempt(ctx, a)
			case CallRefused:
				if seen {
					if none, nerr := nothingPayable(ctx, t.provider, t.creds, t.client, r.newLink(a, t.req.Description), t.since); nerr == nil && none {
						return t.s.expireEnded(ctx, a)
					}
				}
				logFailure(t.rec, ce)
				return unresolved, paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderRefused}
			case CallUnreachable, CallLookupIncomplete:
				if !seen {
					logFailure(t.rec, ce)
					return unresolved, paylink.Link{}, nil
				}
			}
		}
		return unresolved, paylink.Link{}, t.s.readFailed(ctx, t.rec, a, err)
	}
	switch {
	case ok && a.objectID != "" && found.ObjectID != a.objectID:
		slog.Warn("integrations: a lookup found a second object of a key's token; an admin decides", "ref", r.id)
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	case ok:
		slog.Info("integrations: a payment link made by an earlier call was found", "integration", r.integrationID, "provider", t.rec.Provider)
		if err := t.s.noteObject(ctx, a, found); err != nil {
			return unresolved, paylink.Link{}, err
		}
		return t.settle(ctx, r, a, found)
	case expired:
		if a.objectID != "" && found.ObjectID != a.objectID {
			return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
		}
		if ok, err := t.s.markExpiredOf(ctx, a, found.ObjectID); err != nil {
			return unresolved, paylink.Link{}, err
		} else if !ok {
			return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
		}
		return resolvedExpired, paylink.Link{}, nil
	}
	if seen {
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	return unresolved, paylink.Link{}, nil
}

func (t try) settle(ctx context.Context, r *ref, a attempt, l Link) (resolution, paylink.Link, error) {
	if t.s.beforeSettle != nil {
		t.s.beforeSettle(l.ObjectID)
	}
	state, err := t.provider.LinkState(ctx, t.creds, t.client, t.hosted, l.ObjectID)
	if err != nil {
		now, rerr := t.s.attemptByKey(ctx, a.refID, a.seq)
		if rerr != nil {
			return unresolved, paylink.Link{}, rerr
		}
		return unresolved, paylink.Link{}, t.s.readFailed(ctx, t.rec, now, err)
	}
	switch state {
	case LinkOpen:
		link, err := t.s.noteLink(ctx, *r, a, t.req, l)
		if err != nil {
			return unresolved, paylink.Link{}, err
		}
		return resolvedOpen, link, nil
	case LinkSettled:
		return unresolved, paylink.Link{}, t.s.settleAttempt(ctx, a)
	}
	if !t.noOtherPayable(ctx, r, a, l.ObjectID) {
		slog.Warn("integrations: an object of a key expired beside another that may be paid; the entry waits", "ref", r.id)
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	if ok, err := t.s.markExpiredOf(ctx, a, l.ObjectID); err != nil {
		return unresolved, paylink.Link{}, err
	} else if !ok {
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	return resolvedExpired, paylink.Link{}, nil
}

func (t try) noOtherPayable(ctx context.Context, r *ref, a attempt, expired string) bool {
	none, err := nothingPayable(ctx, t.provider, t.creds, t.client, r.newLink(a, t.req.Description), t.since)
	if err == nil && none {
		return true
	}
	slog.Info("integrations: another object of an expired key's token may be paid", "ref", r.id, "expired", expired != "")
	return false
}

func nothingPayable(ctx context.Context, provider PaymentProvider, creds Credentials, client *Client, l NewLink, since time.Time) (bool, error) {
	if checker, ok := provider.(payableChecker); ok {
		return checker.NothingPayable(ctx, creds, client, l, since)
	}
	_, found, _, err := provider.FindPaymentLink(ctx, creds, client, l, since)
	return !found && err == nil, err
}

func (s *Service) expireEnded(ctx context.Context, a attempt) (resolution, paylink.Link, error) {
	now, err := s.attemptByKey(ctx, a.refID, a.seq)
	if err != nil {
		return unresolved, paylink.Link{}, err
	}
	if ok, err := s.markExpiredOf(ctx, now, now.objectID); err != nil {
		return unresolved, paylink.Link{}, err
	} else if !ok {
		return unresolved, paylink.Link{}, s.stillUnknown(ctx, now)
	}
	return resolvedExpired, paylink.Link{}, nil
}

func (s *Service) markExpiredOf(ctx context.Context, a attempt, objectID string) (bool, error) {
	res, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'expired'
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND (object_id IS NULL OR object_id = ?)`, a.refID, a.seq, objectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Service) settleAttempt(ctx context.Context, a attempt) error {
	if err := s.markAttempt(ctx, a, attemptSettled); err != nil {
		return err
	}
	return &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
}

func (s *Service) failed(ctx context.Context, rec record, pending *attempt, err error) error {
	if pending != nil {
		return s.readFailed(ctx, rec, *pending, err)
	}
	ce, ok := IsCall(err)
	if !ok {
		return err
	}
	logFailure(rec, ce)
	switch ce.Kind {
	case CallCredentials:
		return &paylink.Error{Code: apierr.CodeIntegrationCredentialsRefused}
	case CallRefused:
		return &paylink.Error{Code: apierr.CodeIntegrationProviderRefused}
	}
	return &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

func (s *Service) readFailed(ctx context.Context, rec record, a attempt, err error) error {
	if ce, ok := IsCall(err); ok {
		logFailure(rec, ce)
	} else {
		slog.Warn("integrations: a read for a payment link failed", "integration", rec.ID, "provider", rec.Provider)
	}
	return s.stillUnknown(ctx, a)
}

func logFailure(rec record, ce *CallError) {
	slog.Warn("integrations: a call for a payment link failed", "integration", rec.ID, "provider", rec.Provider,
		"kind", int(ce.Kind), "status", ce.Status, "not_executed", ce.NotExecuted, "outcome_unknown", ce.OutcomeUnknown)
}

func (s *Service) stillUnknown(ctx context.Context, a attempt) error {
	now := s.store.now()
	since := now
	if t, err := time.Parse(timeFormat, a.unknownSince); err == nil {
		since = t
	} else if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET unknown_since = ?
		WHERE ref_id = ? AND seq = ? AND unknown_since IS NULL AND sends > clean_sends`, s.store.stamp(now), a.refID, a.seq); err != nil {
		return err
	}
	elapsed := now.Sub(since)
	if elapsed >= outcomeWait {
		slog.Warn("integrations: a payment link's outcome is still unknown; an admin decides", "ref", a.refID)
		return &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
	}
	wait := min(max(elapsed/2, lookupWaitMin), lookupWaitMax, outcomeWait-elapsed)
	return &paylink.Error{Code: apierr.CodeIntegrationLinkLookupPending, Wait: max(wait, time.Second)}
}

func (s *Service) keyKept(a attempt, p PaymentProvider) bool { return s.keyLeft(a, p) > 0 }

func (s *Service) keyLeft(a attempt, p PaymentProvider) time.Duration {
	first, err := time.Parse(timeFormat, a.firstSentAt)
	if err != nil || p.KeyKept() <= 0 {
		return 0
	}
	elapsed := s.store.now().Sub(first)
	if elapsed < 0 || elapsed >= p.KeyKept() {
		return 0
	}
	return p.KeyKept() - elapsed
}

func madeLink(r ref, req paylink.Request) (paylink.Link, error) {
	if r.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if r.url == "" {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	return r.link(), nil
}

func (s *Service) savedLink(ctx context.Context, rec record, provider PaymentProvider, r ref, req paylink.Request) (paylink.Link, error) {
	if r.integrationID != req.IntegrationID || r.url == "" {
		return madeLink(r, req)
	}
	if r.state == "paid" || r.state == "refunded" || r.state == "expired" {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	creds, err := s.store.credentials(ctx, req.WorkspaceID, req.IntegrationID)
	if err != nil || creds.APIKey == "" {
		var unreadable unreadableError
		if err == nil || errors.As(err, &unreadable) {
			return paylink.Link{}, unavailable()
		}
		return paylink.Link{}, err
	}
	if mode := Lookup(rec.Provider).Mode(rec.Settings, creds.APIKey.Reveal()); r.accountMode != "" && mode != r.accountMode {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
	}
	client := s.Client(Lookup(rec.Provider), rec.Settings, s.clientOpts...)
	readFailed := func(err error) error {
		ce, ok := IsCall(err)
		if !ok {
			return err
		}
		logFailure(rec, ce)
		return &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
	}
	if r.accountID != "" {
		account, err := provider.AccountID(ctx, creds, client)
		if err != nil {
			return paylink.Link{}, readFailed(err)
		}
		if account != r.accountID {
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
		}
	}
	state, err := provider.LinkState(ctx, creds, client, r.successURL == "", r.objectID)
	if err != nil {
		return paylink.Link{}, readFailed(err)
	}
	if state != LinkOpen {
		slog.Warn("integrations: a saved payment link can no longer take a payment; an admin reconciles it", "ref", r.id)
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	return madeLink(r, req)
}

func paidWithoutLink(r ref) bool {
	return r.url == "" && (r.state == "paid" || r.state == "refunded")
}

func (s *Service) refHolds(ctx context.Context, r ref, req paylink.Request) (paylink.Link, error) {
	now, err := s.refByID(ctx, r.id)
	if err != nil {
		return paylink.Link{}, err
	}
	if now.objectID == "" {
		return paylink.Link{}, unavailable()
	}
	return madeLink(now, req)
}

const (
	outcomeWait   = 6 * time.Hour
	lookupWaitMin = 30 * time.Second
	lookupWaitMax = 30 * time.Minute
	maxKeysPerTry = 3
)

const stripeKeyKept = 23 * time.Hour

func providerDoes(id, kind string) bool {
	p := Lookup(id)
	if p == nil {
		return false
	}
	for _, k := range p.Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func (s *Service) pageURLs() (success, cancel string) {
	base := strings.TrimRight(s.publicURL, "/")
	if base == "" {
		return "", ""
	}
	return base + PaymentDonePath, base + PaymentCancelledPath
}

func (s *Service) limitsAllow(ctx context.Context, q querier, req paylink.Request) error {
	rec, err := s.store.get(ctx, q, req.WorkspaceID, req.IntegrationID)
	if errors.Is(err, errNotFound) {
		return unavailable()
	} else if err != nil {
		return err
	}
	usable, err := s.store.usableIn(ctx, q, req.WorkspaceID, req.IntegrationID, req.SpaceID)
	if err != nil {
		return err
	}
	if !usable {
		return unavailable()
	}
	_, err = s.checkMoney(rec, req.Amount, strings.ToUpper(strings.TrimSpace(req.Currency)))
	return err
}

func (s *Service) checkMoney(rec record, amount, cur string) (int64, error) {
	refused := func(code apierr.Code, params apierr.Params) error {
		return &paylink.Error{Code: code, Params: params}
	}
	if !currency.Known(cur) || !containsString(rec.Currencies, cur) {
		return 0, refused(apierr.CodeIntegrationCurrencyRefused, apierr.Params{"currency": cur})
	}
	minor, err := currency.Minor(amount, cur)
	switch {
	case errors.Is(err, currency.ErrTooLarge):
		return 0, refused(apierr.CodeIntegrationAmountAboveMax, ceilingParams(rec.MaxAmountMinor, cur))
	case err != nil:
		return 0, refused(apierr.CodeIntegrationAmountRefused, nil)
	}
	if minor > rec.MaxAmountMinor {
		return 0, refused(apierr.CodeIntegrationAmountAboveMax, ceilingParams(rec.MaxAmountMinor, cur))
	}
	return minor, nil
}

func ceilingParams(maxMinor int64, cur string) apierr.Params {
	return apierr.Params{"max": currency.Format(maxMinor, cur), "currency": cur}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type ref struct {
	id, integrationID, appID, recordID            int64
	token, slot                                   string
	objectID, url                                 string
	amountMinor                                   int64
	currency                                      string
	keySeq                                        int
	state                                         string
	createdAt                                     string
	accountID, accountMode, successURL, cancelURL string
	paidObjectID, paymentID                       string
	effectID                                      int64
	paidLinkID                                    string
	linkExpiredAt                                 string
	amountField, currencyField                    string
	polls                                         int
}

func (r ref) newLink(a attempt, description string) NewLink {
	return NewLink{Token: a.token, KeySeq: a.seq, AmountMinor: r.amountMinor, Currency: r.currency, Description: description,
		SuccessURL: r.successURL, CancelURL: r.cancelURL}
}

type whose struct{ account, mode string }

func (r ref) link() paylink.Link {
	return paylink.Link{URL: r.url, ObjectID: r.objectID, AmountMinor: r.amountMinor, Currency: r.currency}
}

const refColumns = `id, token, integration_id, app_id, record_id, slot, coalesce(provider_object_id, ''), coalesce(url, ''),
	amount_minor, currency, key_seq, state, created_at, account_id, account_mode, success_url, cancel_url,
	coalesce(paid_object_id, ''), coalesce(payment_id, ''), coalesce(effect_id, 0), coalesce(paid_link_id, ''), coalesce(link_expired_at, ''),
	amount_field, currency_field, polls`

func scanRef(row interface{ Scan(...any) error }) (ref, error) {
	var r ref
	err := row.Scan(&r.id, &r.token, &r.integrationID, &r.appID, &r.recordID, &r.slot, &r.objectID, &r.url,
		&r.amountMinor, &r.currency, &r.keySeq, &r.state, &r.createdAt, &r.accountID, &r.accountMode, &r.successURL, &r.cancelURL,
		&r.paidObjectID, &r.paymentID, &r.effectID, &r.paidLinkID, &r.linkExpiredAt, &r.amountField, &r.currencyField, &r.polls)
	return r, err
}

func (s *Service) refOf(ctx context.Context, effectID int64) (ref, bool, error) {
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, effectID))
	if errors.Is(err, sql.ErrNoRows) {
		return ref{}, false, nil
	}
	return r, err == nil, err
}

func (s *Service) refByToken(ctx context.Context, token string) (ref, bool, error) {
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs
		WHERE token = ? OR id = (SELECT ref_id FROM integration_ref_attempts WHERE token = ?)`, token, token))
	if errors.Is(err, sql.ErrNoRows) {
		return ref{}, false, nil
	}
	return r, err == nil, err
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Service) beginRef(ctx context.Context, req paylink.Request, minor int64, cur string, who whose, successURL, cancelURL string, checked []int64) (ref, error) {
	token, err := newToken()
	if err != nil {
		return ref{}, err
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ref{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, req.EffectID))
	if errors.Is(err, sql.ErrNoRows) {
		var took bool
		if out, took, err = s.takeEarlier(ctx, tx, req, checked); err != nil {
			return ref{}, err
		}
		if !took {
			at := s.store.stamp(s.store.now())
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency, created_at,
					account_id, account_mode, success_url, cancel_url, amount_field, currency_field)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				token, req.IntegrationID, req.AppID, req.RecordID, req.Slot, req.EffectID, minor, cur, at,
				who.account, who.mode, successURL, cancelURL, req.AmountField, req.CurrencyField); err != nil {
				return ref{}, err
			}
			out, err = scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, req.EffectID))
		}
	}
	if err != nil {
		return ref{}, err
	}
	if out.objectID == "" && (out.accountID != who.account || out.accountMode != who.mode) {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
	}
	if out.objectID == "" && out.integrationID != req.IntegrationID {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if out.objectID == "" && (out.amountMinor != minor || out.currency != cur || !sameMoneyFields(out, req)) {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
	}
	return out, tx.Commit()
}

func (s *Service) refByID(ctx context.Context, id int64) (ref, error) {
	return scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, id))
}

const (
	attemptMade      = "made"
	attemptExpired   = "expired"
	attemptSettled   = "settled"
	attemptAbandoned = "abandoned"
)

type attempt struct {
	refID                     int64
	seq                       int
	token                     string
	sends, cleanSends         int
	firstSentAt, unknownSince string
	outcome                   string
	objectID, url             string
	objectVerified            bool
}

func (a attempt) unknown() bool { return a.outcome == "" && a.sends > a.cleanSends }

const attemptColumns = `ref_id, seq, token, sends, clean_sends, coalesce(first_sent_at, ''), coalesce(unknown_since, ''),
	coalesce(outcome, ''), coalesce(object_id, ''), coalesce(url, ''), object_verified`

func scanAttempt(row interface{ Scan(...any) error }) (attempt, error) {
	var a attempt
	err := row.Scan(&a.refID, &a.seq, &a.token, &a.sends, &a.cleanSends, &a.firstSentAt, &a.unknownSince, &a.outcome, &a.objectID, &a.url, &a.objectVerified)
	return a, err
}

func (s *Service) attemptByKey(ctx context.Context, refID int64, seq int) (attempt, error) {
	return scanAttempt(s.store.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM integration_ref_attempts WHERE ref_id = ? AND seq = ?`, refID, seq))
}

func (s *Service) attemptOf(ctx context.Context, r ref) (attempt, bool, error) {
	a, err := s.attemptByKey(ctx, r.id, r.keySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, nil
	}
	return a, err == nil, err
}

func (s *Service) currentAttempt(ctx context.Context, r ref) (attempt, error) {
	token := r.token
	if r.keySeq > 0 {
		var err error
		if token, err = newToken(); err != nil {
			return attempt{}, err
		}
	}
	if _, err := s.store.db.ExecContext(ctx, `INSERT INTO integration_ref_attempts (ref_id, seq, token, created_at)
		SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM integration_refs WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL
			AND state IN ('creating', 'failed'))
		ON CONFLICT (ref_id, seq) DO NOTHING`, r.id, r.keySeq, token, s.store.stamp(s.store.now()), r.id, r.keySeq); err != nil {
		return attempt{}, err
	}
	a, err := s.attemptByKey(ctx, r.id, r.keySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, errRefMoved
	}
	return a, err
}

var errRefMoved = errors.New("the payment link's reference moved on")

func (s *Service) writeAhead(ctx context.Context, r ref, a attempt, req paylink.Request, who whose) (ok bool, refused, err error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if lerr := s.limitsAllow(ctx, tx, req); lerr != nil {
		if errors.As(lerr, new(*paylink.Error)) {
			return false, lerr, nil
		}
		return false, nil, lerr
	}
	rec, err := s.store.get(ctx, tx, req.WorkspaceID, req.IntegrationID)
	if err != nil {
		return false, nil, err
	}
	if rec.Mode != who.mode {
		return false, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}, nil
	}
	at := s.store.stamp(s.store.now())
	res, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET sends = sends + 1,
			first_sent_at = coalesce(first_sent_at, ?), unknown_since = coalesce(unknown_since, ?)
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL
			AND EXISTS (SELECT 1 FROM integration_refs WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL
				AND state IN ('creating', 'failed'))`,
		at, at, a.refID, a.seq, r.id, a.seq)
	if err != nil {
		return false, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	return n == 1, nil, tx.Commit()
}

func (s *Service) noteNotExecuted(ctx context.Context, a attempt) (attempt, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET clean_sends = clean_sends + 1,
			first_sent_at = CASE WHEN clean_sends + 1 = sends THEN NULL ELSE first_sent_at END,
			unknown_since = CASE WHEN clean_sends + 1 = sends THEN NULL ELSE unknown_since END
		WHERE ref_id = ? AND seq = ? AND clean_sends < sends`, a.refID, a.seq); err != nil {
		return attempt{}, err
	}
	return s.attemptByKey(ctx, a.refID, a.seq)
}

func (s *Service) noteObject(ctx context.Context, a attempt, l Link) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = ?, url = ?, object_verified = 1
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND (object_id IS NULL OR object_id = ?)`, l.ObjectID, l.URL, a.refID, a.seq, l.ObjectID)
	return err
}

func (s *Service) noteSeen(ctx context.Context, a attempt, objectID string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = ?, url = NULL, object_verified = 0
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND object_verified = 0 AND (object_id IS NULL OR object_id = ?)`, objectID, a.refID, a.seq, objectID)
	return err
}

func (s *Service) markAttempt(ctx context.Context, a attempt, outcome string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = ? WHERE ref_id = ? AND seq = ? AND outcome IS NULL`,
		outcome, a.refID, a.seq)
	return err
}

func (s *Service) nextAttempt(ctx context.Context, r ref, a attempt) (ref, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1
		WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL AND state IN ('creating', 'failed')
			AND EXISTS (SELECT 1 FROM integration_ref_attempts WHERE ref_id = ? AND seq = ? AND outcome IN ('expired', 'abandoned'))
			AND NOT EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = ?
				AND (o.outcome IS NULL OR o.outcome NOT IN ('expired', 'abandoned') OR (o.outcome = 'abandoned' AND o.object_id IS NOT NULL)))`,
		r.id, a.seq, r.id, a.seq, r.id); err != nil {
		return ref{}, err
	}
	return s.refByID(ctx, r.id)
}

func (s *Service) noteLink(ctx context.Context, r ref, a attempt, req paylink.Request, l Link) (paylink.Link, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return paylink.Link{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'
		WHERE id = ? AND provider_object_id IS NULL AND state IN ('creating', 'failed') AND key_seq = ?
			AND EXISTS (SELECT 1 FROM integration_ref_attempts WHERE ref_id = ? AND seq = ? AND (outcome IS NULL OR outcome = 'made')
				AND (object_id IS NULL OR object_id = ?))`,
		l.ObjectID, l.URL, r.id, a.seq, a.refID, a.seq, l.ObjectID)
	if err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == 2067 {
			slog.Error("integrations: a provider answered with an object that belongs to another reference", "integration", r.integrationID)
		}
		return paylink.Link{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return paylink.Link{}, err
	}
	if n == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'made', object_id = ?, url = ?, object_verified = 1
			WHERE ref_id = ? AND seq = ? AND (outcome IS NULL OR outcome = 'made')`, l.ObjectID, l.URL, a.refID, a.seq); err != nil {
			return paylink.Link{}, err
		}
		if err := tx.Commit(); err != nil {
			return paylink.Link{}, err
		}
		return paylink.Link{URL: l.URL, ObjectID: l.ObjectID, AmountMinor: r.amountMinor, Currency: r.currency}, nil
	}
	_ = tx.Rollback()
	now, err := s.refByID(ctx, r.id)
	if err != nil {
		return paylink.Link{}, err
	}
	if now.objectID == "" && !paidWithoutLink(now) {
		slog.Info("integrations: an old key's object was not saved: the ref moved on", "ref", r.id)
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
	}
	return madeLink(now, req)
}

var errRecheck = errors.New("the record's payment links changed")

func recordRefs(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, req paylink.Request) ([]ref, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+refColumns+` FROM integration_refs
		WHERE app_id = ? AND record_id = ? AND (effect_id IS NULL OR effect_id != ?) ORDER BY id`, req.AppID, req.RecordID, req.EffectID)
	if err != nil {
		return nil, err
	}
	var out []ref
	for rows.Next() {
		r, err := scanRef(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func paidOnOwnLink(r ref) bool {
	return (r.state == "paid" || r.state == "refunded") && r.objectID != "" &&
		(r.paidObjectID == r.objectID || r.paidLinkID == r.objectID)
}

func linkFinished(r ref) bool {
	return r.state == "expired" || r.linkExpiredAt != "" || paidOnOwnLink(r)
}

func (s *Service) earlierLink(ctx context.Context, provider PaymentProvider, creds Credentials, client *Client, rec record,
	req paylink.Request, minor int64, cur string, who whose) (link paylink.Link, reuse bool, checked []int64, err error) {
	refs, err := recordRefs(ctx, s.store.db, req)
	if err != nil {
		return paylink.Link{}, false, nil, err
	}
	var open *ref
	for _, prior := range refs {
		if prior.objectID == "" {
			continue
		}
		if linkFinished(prior) {
			if prior.state != "paid" && prior.state != "refunded" && !paidOnOwnLink(prior) &&
				prior.integrationID == req.IntegrationID && prior.accountID == who.account && prior.accountMode == who.mode {
				if err := s.provePaymentEnded(ctx, provider, creds, client, rec, prior); err != nil {
					return paylink.Link{}, false, nil, err
				}
			}
			continue
		}
		switch {
		case prior.integrationID != req.IntegrationID:
			return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationChanged}
		case prior.accountID != who.account || prior.accountMode != who.mode:
			return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
		}
		state, err := provider.LinkState(ctx, creds, client, prior.successURL == "", prior.objectID)
		if err != nil {
			return paylink.Link{}, false, nil, s.failed(ctx, rec, nil, err)
		}
		switch state {
		case LinkOpen:
			if prior.state == "paid" || prior.state == "refunded" {
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
			}
			if open != nil {
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
			}
			open = &prior
		case LinkExpired:
			if err := s.provePaymentEnded(ctx, provider, creds, client, rec, prior); err != nil {
				return paylink.Link{}, false, nil, err
			}
			at := s.store.stamp(s.store.now())
			if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET state = 'expired', settled_at = coalesce(settled_at, ?)
				WHERE id = ? AND state IN ('creating', 'open')`, at, prior.id); err != nil {
				return paylink.Link{}, false, nil, err
			}
			if err := s.noteLinkExpired(ctx, prior.id, prior.objectID, at); err != nil {
				return paylink.Link{}, false, nil, err
			}
			checked = append(checked, prior.id)
		default:
			if prior.state != "paid" && prior.state != "refunded" {
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
			}
			checked = append(checked, prior.id)
		}
	}
	if open == nil {
		return paylink.Link{}, false, checked, nil
	}
	if open.amountMinor != minor || open.currency != cur || !sameMoneyFields(*open, req) {
		return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
	}
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET slot = ?, slot_effect_id = ?
		WHERE id = ? AND coalesce(slot_effect_id, effect_id, 0) < ?`, req.Slot, req.EffectID, open.id, req.EffectID); err != nil {
		return paylink.Link{}, false, nil, err
	}
	slog.Info("integrations: a record's payment link that may still be paid is given again", "integration", open.integrationID, "ref", open.id)
	return open.link(), true, nil, nil
}

func (s *Service) provePaymentEnded(ctx context.Context, provider PaymentProvider, creds Credentials, client *Client, rec record, prior ref) error {
	since, err := time.Parse(timeFormat, prior.createdAt)
	if err != nil {
		return err
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT token, seq FROM integration_ref_attempts WHERE ref_id = ? ORDER BY seq`, prior.id)
	if err != nil {
		return err
	}
	var keys []NewLink
	for rows.Next() {
		l := NewLink{AmountMinor: prior.amountMinor, Currency: prior.currency, SuccessURL: prior.successURL, CancelURL: prior.cancelURL}
		if err := rows.Scan(&l.Token, &l.KeySeq); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, l := range keys {
		none, err := nothingPayable(ctx, provider, creds, client, l, since)
		if err != nil {
			if ce, ok := IsCall(err); ok && ce.Kind == CallSettled {
				return &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
			}
			return s.failed(ctx, rec, nil, err)
		}
		if !none {
			slog.Info("integrations: an earlier link ended but another object of its token may be paid", "ref", prior.id, "key", l.KeySeq)
			return &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
		}
	}
	return nil
}

func (s *Service) takeEarlier(ctx context.Context, tx *sql.Tx, req paylink.Request, checked []int64) (ref, bool, error) {
	refs, err := recordRefs(ctx, tx, req)
	if err != nil {
		return ref{}, false, err
	}
	var live []ref
	for _, prior := range refs {
		switch {
		case prior.objectID != "":
			if linkFinished(prior) || slices.Contains(checked, prior.id) {
				continue
			}
			return ref{}, false, errRecheck
		case paidWithoutLink(prior), prior.state == "expired":
			continue
		case prior.effectID > req.EffectID:
			return ref{}, false, &paylink.Error{Code: apierr.CodeIntegrationLinkLookupPending, Wait: lookupWaitMin}
		}
		var ran bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_ref_attempts
			WHERE ref_id = ? AND (sends > clean_sends OR object_id IS NOT NULL OR outcome IS NOT NULL))`, prior.id).Scan(&ran); err != nil {
			return ref{}, false, err
		}
		if !ran && prior.state == "creating" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM integration_refs WHERE id = ?`, prior.id); err != nil {
				return ref{}, false, err
			}
			continue
		}
		var alive bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = ? AND NOT `+deadKey+`)`,
			prior.id).Scan(&alive); err != nil {
			return ref{}, false, err
		}
		if !alive {
			if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET state = 'expired', settled_at = coalesce(settled_at, ?)
				WHERE id = ? AND state IN ('creating', 'failed')`, s.store.stamp(s.store.now()), prior.id); err != nil {
				return ref{}, false, err
			}
			continue
		}
		live = append(live, prior)
	}
	switch len(live) {
	case 0:
		return ref{}, false, nil
	case 1:
	default:
		slog.Warn("integrations: a record has more than one payment link still being made; an admin decides", "app", req.AppID)
		return ref{}, false, &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
	}
	prior := live[0]
	if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET effect_id = ?, slot = ?, slot_effect_id = NULL WHERE id = ?`, req.EffectID, req.Slot, prior.id); err != nil {
		return ref{}, false, err
	}
	slog.Info("integrations: a record's payment link still being made is taken by its newer entry", "integration", prior.integrationID, "ref", prior.id)
	out, err := scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, prior.id))
	return out, err == nil, err
}

const deadKey = `(coalesce(o.outcome, '') = 'expired' OR (coalesce(o.outcome, '') = 'abandoned' AND o.object_id IS NULL)
	OR (o.outcome IS NULL AND o.sends = o.clean_sends AND o.object_id IS NULL))`

const payableRef = `(CASE WHEN r.provider_object_id IS NOT NULL
		THEN r.state != 'expired' AND r.link_expired_at IS NULL
			AND NOT (r.state IN ('paid', 'refunded') AND (coalesce(r.paid_object_id, '') = r.provider_object_id
				OR coalesce(r.paid_link_id, '') = r.provider_object_id))
		ELSE EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = r.id AND NOT ` + deadKey + `)
	END)`

func (s *Service) PayableMoney(ctx context.Context, q paylink.Querier, appID, recordID int64) (map[int64][]paylink.MoneyFields, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.record_id, r.amount_field, r.currency_field FROM integration_refs r
		WHERE r.app_id = ? AND (? = 0 OR r.record_id = ?) AND `+payableRef+` ORDER BY r.record_id, r.id`, appID, recordID, recordID)
	if err != nil {
		return nil, err
	}
	out := map[int64][]paylink.MoneyFields{}
	for rows.Next() {
		var id int64
		var f paylink.MoneyFields
		if err := rows.Scan(&id, &f.Amount, &f.Currency); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[id] = append(out[id], f)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) MayBePaid(ctx context.Context, q paylink.Querier, appID, recordID int64) (bool, []paylink.MoneyFields, error) {
	m, err := s.PayableMoney(ctx, q, appID, recordID)
	if err != nil {
		return false, nil, err
	}
	f, ok := m[recordID]
	return ok, f, nil
}

func (s *Service) noteLinkExpired(ctx context.Context, refID int64, objectID, at string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET link_expired_at = coalesce(link_expired_at, ?)
		WHERE id = ? AND provider_object_id = ?`, at, refID, objectID)
	return err
}

func sameMoneyFields(r ref, req paylink.Request) bool {
	if r.amountField == "" && r.currencyField == "" {
		return true
	}
	return r.amountField == req.AmountField && r.currencyField == req.CurrencyField
}
