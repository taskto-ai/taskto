package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
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
	if found && ref.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
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
		return paylink.Link{}, err
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
	if s.beforeBeginRef != nil {
		s.beforeBeginRef()
	}
	mode := Lookup(rec.Provider).Mode(rec.Settings, creds.APIKey.Reveal())
	ref, err = s.beginRef(ctx, req, minor, cur, whose{account: account, mode: mode}, success, cancel)
	if err != nil {
		return paylink.Link{}, err
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
			if !s.keyKept(a) {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
		}

		if ok, err := s.writeAhead(ctx, ref, a); err != nil {
			return paylink.Link{}, err
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
		made, err := provider.CreatePaymentLink(ctx, creds, client, ref.newLink(a, req.Description))
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
	case ok:
		slog.Info("integrations: a payment link made by an earlier call was found", "integration", r.integrationID, "provider", t.rec.Provider)
		if err := t.s.noteObject(ctx, a, found); err != nil {
			return unresolved, paylink.Link{}, err
		}
		return t.settle(ctx, r, a, found)
	case expired:
		return resolvedExpired, paylink.Link{}, t.s.markAttempt(ctx, a, attemptExpired)
	}
	if seen {
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	return unresolved, paylink.Link{}, nil
}

func (t try) settle(ctx context.Context, r *ref, a attempt, l Link) (resolution, paylink.Link, error) {
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
	return resolvedExpired, paylink.Link{}, t.s.markAttempt(ctx, a, attemptExpired)
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

func (s *Service) keyKept(a attempt) bool {
	first, err := time.Parse(timeFormat, a.firstSentAt)
	return err == nil && s.store.now().Sub(first) < keyTrusted
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

const keyTrusted = 23 * time.Hour

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
	coalesce(paid_object_id, ''), coalesce(payment_id, '')`

func scanRef(row interface{ Scan(...any) error }) (ref, error) {
	var r ref
	err := row.Scan(&r.id, &r.token, &r.integrationID, &r.appID, &r.recordID, &r.slot, &r.objectID, &r.url,
		&r.amountMinor, &r.currency, &r.keySeq, &r.state, &r.createdAt, &r.accountID, &r.accountMode, &r.successURL, &r.cancelURL,
		&r.paidObjectID, &r.paymentID)
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

func (s *Service) beginRef(ctx context.Context, req paylink.Request, minor int64, cur string, who whose, successURL, cancelURL string) (ref, error) {
	token, err := newToken()
	if err != nil {
		return ref{}, err
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ref{}, err
	}
	defer func() { _ = tx.Rollback() }()
	at := s.store.stamp(s.store.now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency, created_at, account_id, account_mode, success_url, cancel_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (effect_id) DO NOTHING`,
		token, req.IntegrationID, req.AppID, req.RecordID, req.Slot, req.EffectID, minor, cur, at, who.account, who.mode, successURL, cancelURL); err != nil {
		return ref{}, err
	}
	out, err := scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, req.EffectID))
	if err != nil {
		return ref{}, err
	}
	if out.objectID == "" && (out.accountID != who.account || out.accountMode != who.mode) {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
	}
	if out.objectID == "" && out.integrationID != req.IntegrationID {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if out.objectID == "" && (out.amountMinor != minor || out.currency != cur) {
		return ref{}, unavailable()
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
	if _, err := s.store.db.ExecContext(ctx, `INSERT INTO integration_ref_attempts (ref_id, seq, token, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (ref_id, seq) DO NOTHING`, r.id, r.keySeq, token, s.store.stamp(s.store.now())); err != nil {
		return attempt{}, err
	}
	return s.attemptByKey(ctx, r.id, r.keySeq)
}

func (s *Service) writeAhead(ctx context.Context, r ref, a attempt) (bool, error) {
	at := s.store.stamp(s.store.now())
	res, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET sends = sends + 1,
			first_sent_at = coalesce(first_sent_at, ?), unknown_since = coalesce(unknown_since, ?)
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL
			AND EXISTS (SELECT 1 FROM integration_refs WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL)`,
		at, at, a.refID, a.seq, r.id, a.seq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL`, l.ObjectID, l.URL, a.refID, a.seq)
	return err
}

func (s *Service) noteSeen(ctx context.Context, a attempt, objectID string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = ?, url = NULL, object_verified = 0
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND object_verified = 0`, objectID, a.refID, a.seq)
	return err
}

func (s *Service) markAttempt(ctx context.Context, a attempt, outcome string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = ? WHERE ref_id = ? AND seq = ? AND outcome IS NULL`,
		outcome, a.refID, a.seq)
	return err
}

func (s *Service) nextAttempt(ctx context.Context, r ref, a attempt) (ref, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1
		WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL
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
		WHERE id = ? AND provider_object_id IS NULL AND state IN ('creating', 'failed')`, l.ObjectID, l.URL, r.id)
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
	return s.refHolds(ctx, r, req)
}
