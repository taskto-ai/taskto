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

// The create_payment_link effect's side of the integrations (apps/paymentlink.go asks for it).
//
// One outbox entry makes at most one payment link. The entry has one ref (integration_refs, unique
// by the entry's number) whose random token is the provider's idempotency key and the reference its
// payment echoes back, and the ref keeps the provider's answer. So an entry done again after a
// crash finds its ref, and
//
//   - if the ref has the provider's object, the provider isn't asked again: the link is the ref's;
//   - if it has none, the call may or may not have been made: it is made again under the same key,
//     which the provider answers with the first call's object when there was one (Stripe keeps a
//     key for at least a day; a retry after longer than that, with no object noted, is the one case
//     that can make a second payable link, and its payment is recorded as a duplicate).
//
// A provider that answered an error it keeps under the key is asked again under a key of its own
// (NewLink.KeySeq), as the kept failure would be answered again for ever. Such an error (a 5xx) is
// an indeterminate result, though: the call may have made the object. So before a call under a new
// key, the provider is asked for an object of the ref's token, which is adopted as if the call had
// answered it; and if it can't be asked, nothing is made and the entry waits (FindPaymentLink).
//
// A ref belongs to what its first try was: the provider's account (read from the provider with the
// integration's key when the ref is made) with the mode of that key (a test key and a live key of
// one Stripe account answer with the same account id, and see different objects), the integration
// it was made under, and the shape of the request (the two pages the payer returns to, or none). A
// retry sends the same parameters and looks among the same kind of objects in the same account and
// mode, whatever the server's public address and the integration's key are by then. A key replaced
// by another account's, or by the same account's key of the other mode, leaves the unfinished refs as
// they are, and their entries fail (integration_account_changed): nothing is looked for or made
// where an object the old one made could not be found. A slot bound to another integration since
// the ref was made is refused too, whether or not the ref has its link (integration_changed): a
// ref's calls are its first integration's, which is the one whose key an admin reconciles it with.
//
// A key the provider may still hold an answer under is not reused once that answer is no use: the
// provider replays a call's saved answer to the same key, a failure and an object that has expired
// since included. So a try that looks for the ref's object (a ref with an error kept under its key, a
// failed ref tried again, one past the time the provider keeps keys) and finds none that a payer can
// use moves the ref on to a key of its own before it makes one.
//
// The ref is made before the call and the money is checked before the ref, so a refused amount
// leaves nothing behind. The key of the integration is opened here, for the call, and goes nowhere
// else: not into an error, a log line, the ref or the record.

// The server's own pages the payer returns to from the provider's hosted page (the web app's
// routes), when the server has a public URL: never a definition's, never a request's. Without one
// the payer stays on a confirmation page the provider hosts. A ref takes the pages the server has
// when the ref is made and keeps them: they are the parameters of its calls.
const (
	PaymentDonePath      = "/payment/done"
	PaymentCancelledPath = "/payment/cancelled"
)

func unavailable() error {
	return &paylink.Error{Code: apierr.CodeIntegrationUnavailable}
}

// OpenPaymentLink returns the payment link for the request's entry, asking the provider for it only
// if the entry hasn't made one. It checks, as of now and against the workspace of the app, that the
// integration exists, is switched on and is allowed in the app's space, and then the admin's
// limits on the amount and the currency.
func (s *Service) OpenPaymentLink(ctx context.Context, req paylink.Request) (paylink.Link, error) {
	// The integration is looked for in the app's workspace only: another workspace's id is not found.
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

	// What the entry already made.
	ref, found, err := s.refOf(ctx, req.EffectID)
	if err != nil {
		return paylink.Link{}, err
	}
	if found && (ref.appID != req.AppID || ref.recordID != req.RecordID) {
		return paylink.Link{}, unavailable() // an entry's number is its own: never another's
	}
	if found && ref.objectID != "" {
		return madeLink(ref, req)
	}
	if found && ref.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged} // before anything is asked of anyone
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
			return paylink.Link{}, unavailable() // no key, or one the server's key can't open
		}
		return paylink.Link{}, err
	}

	// Whose account the key is now, and in which mode: a ref is made, and its unfinished call is
	// looked for and repeated, only in the account and mode it was made in.
	client := s.Client(Lookup(rec.Provider), rec.Settings, s.clientOpts...)
	account, err := provider.AccountID(ctx, creds, client)
	if err != nil {
		ce, ok := IsCall(err)
		if !ok {
			return paylink.Link{}, err
		}
		return paylink.Link{}, s.callFailed(ctx, nil, rec, ce)
	}
	// With no public URL there are no pages of ours: both are empty and the provider hosts the
	// payer's confirmation (a link is made all the same). A ref already made keeps its own.
	success, cancel := s.pageURLs()
	if s.beforeBeginRef != nil {
		s.beforeBeginRef()
	}
	mode := Lookup(rec.Provider).Mode(rec.Settings, creds.APIKey.Reveal())
	ref, err = s.beginRef(ctx, req, minor, cur, whose{account: account, mode: mode}, success, cancel)
	if err != nil {
		return paylink.Link{}, err
	}
	if ref.objectID != "" { // made by a call that finished while this one began
		return madeLink(ref, req)
	}

	since, err := time.Parse(timeFormat, ref.createdAt)
	if err != nil {
		return paylink.Link{}, err
	}
	// A key of its own means an earlier call answered an error the provider kept, which it also
	// gives for a call it executed: the object may exist. Another under the new key could make a
	// second payable link for the same payment, so the first is looked for and adopted; and while
	// the provider can't say, no call is made. The same goes for a ref failed and tried again (a
	// refusal, a 4xx included, may be kept under the key too), and for the same key once the provider
	// may have forgotten it: a call under it would then make another object, as a call under a new
	// key would.
	if ref.keySeq > 0 || ref.retried || s.store.now().Sub(since) >= keyTrusted {
		nl := ref.newLink(minor, cur, req.Description)
		found, ok, err := provider.FindPaymentLink(ctx, creds, client, nl, since)
		if err != nil {
			return paylink.Link{}, s.callFailed(ctx, &ref, rec, err)
		}
		if ok {
			link, err := s.noteLink(ctx, ref, req, found)
			if err != nil {
				return paylink.Link{}, err
			}
			slog.Info("integrations: a payment link made by an earlier call was adopted", "integration", ref.integrationID, "provider", rec.Provider)
			return link, nil
		}
		// Nothing a payer can use is the ref's. The key may still hold the provider's answer to the call
		// that made an object that has expired since (the lookup passes over it), or a failure: the same
		// key would replay it as if it were a new link. So the call is made under a key of its own.
		if ref, err = s.nextKey(ctx, ref); err != nil {
			return paylink.Link{}, err
		}
		if ref.objectID != "" { // made by a call that finished meanwhile
			return madeLink(ref, req)
		}
	}
	// The key is used from here on, whatever becomes of the call: noted first, so that a try that finds
	// nothing of it moves on from it.
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_called_seq = ?
		WHERE id = ? AND provider_object_id IS NULL AND key_called_seq < ?`, ref.keySeq, ref.id, ref.keySeq); err != nil {
		return paylink.Link{}, err
	}
	link, err := provider.CreatePaymentLink(ctx, creds, client, ref.newLink(minor, cur, req.Description))
	if err != nil {
		return paylink.Link{}, s.callFailed(ctx, &ref, rec, err)
	}
	return s.noteLink(ctx, ref, req, link)
}

// madeLink is the link a ref already has. It belongs to the integration that made it: another one,
// bound since, is not the one the record may be written with, and no second link is made for the
// entry. It is checked wherever a finished ref is found, however late.
func madeLink(r ref, req paylink.Request) (paylink.Link, error) {
	if r.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	return r.link(), nil
}

// keyTrusted is how long a key is known to be kept by the provider (Stripe keeps one for at least 24
// hours, and may remove it after; a margin is left).
const keyTrusted = 23 * time.Hour

// providerDoes says whether the registry's provider id implements the kind.
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

// pageURLs are the addresses the payer returns to, on the server's public URL: "" when it has none.
func (s *Service) pageURLs() (success, cancel string) {
	base := strings.TrimRight(s.publicURL, "/")
	if base == "" {
		return "", ""
	}
	return base + PaymentDonePath, base + PaymentCancelledPath
}

// checkMoney converts the amount of the request to the currency's smallest unit, exactly, and
// checks it against the integration's limits. It never rounds: an amount with more decimals than the
// currency has, one of zero or less, one that isn't a number, one above the admin's ceiling and a
// currency the admin didn't list are each refused with a code that says which.
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

// ----- the refs -----

// ref is a row of integration_refs.
type ref struct {
	id, integrationID, appID, recordID int64
	token, slot                        string
	objectID, url                      string
	amountMinor                        int64
	currency                           string
	keySeq, keyCalledSeq               int
	state                              string
	createdAt                          string
	// accountID is the provider's account the ref was made under and accountMode the mode of the key
	// that read it (test or live), and successURL and cancelURL the pages its calls ask the payer to
	// return to, both empty for a link that is hosted by the provider.
	accountID, accountMode, successURL, cancelURL string
	// retried is set by beginRef (it is not stored) on a ref that had failed and starts anew: what the
	// provider kept of its failed call is not to be asked again under the same key.
	retried bool
}

// newLink is what the provider is asked for, from the ref and what this try asks: the ref's token
// and key, the pages of its first try.
func (r ref) newLink(minor int64, cur, description string) NewLink {
	return NewLink{Token: r.token, KeySeq: r.keySeq, AmountMinor: minor, Currency: cur, Description: description,
		SuccessURL: r.successURL, CancelURL: r.cancelURL}
}

// whose is who a ref's calls are made as: the account the key reads and its mode.
type whose struct{ account, mode string }

func (r ref) link() paylink.Link {
	return paylink.Link{URL: r.url, ObjectID: r.objectID, AmountMinor: r.amountMinor, Currency: r.currency}
}

const refColumns = `id, token, integration_id, app_id, record_id, slot, coalesce(provider_object_id, ''), coalesce(url, ''),
	amount_minor, currency, key_seq, key_called_seq, state, created_at, account_id, account_mode, success_url, cancel_url`

func scanRef(row interface{ Scan(...any) error }) (ref, error) {
	var r ref
	err := row.Scan(&r.id, &r.token, &r.integrationID, &r.appID, &r.recordID, &r.slot, &r.objectID, &r.url,
		&r.amountMinor, &r.currency, &r.keySeq, &r.keyCalledSeq, &r.state, &r.createdAt, &r.accountID, &r.accountMode, &r.successURL, &r.cancelURL)
	return r, err
}

// refOf finds the ref of the outbox entry.
func (s *Service) refOf(ctx context.Context, effectID int64) (ref, bool, error) {
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, effectID))
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

// beginRef makes the entry's ref, or finds it, ready for a call: in one transaction that holds the
// write lock, so two workers that took the same entry find one ref. A ref that has no object is
// updated to what this try asks (the entry's amount is fixed, but what the record holds may have
// been corrected); a failed one starts anew (retried) and is looked into before it is called again.
// What a ref was made with is the first try's and is never updated: its account and the key's mode,
// its integration and its pages. A ref with no object that was made in another account or mode than
// who is left exactly as it is and the try is refused (whatever its calls made is where they were
// made), and so is one made under another integration than the request's (integration_changed).
func (s *Service) beginRef(ctx context.Context, req paylink.Request, minor int64, cur string, who whose, successURL, cancelURL string) (ref, error) {
	token, err := newToken()
	if err != nil {
		return ref{}, err
	}
	var out ref
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
	out, err = scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, req.EffectID))
	if err != nil {
		return ref{}, err
	}
	if out.objectID == "" && (out.accountID != who.account || out.accountMode != who.mode) {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
	}
	if out.objectID == "" && out.integrationID != req.IntegrationID {
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if out.objectID == "" && (out.amountMinor != minor || out.currency != cur || out.state == "failed") {
		if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET amount_minor = ?, currency = ?, state = 'creating'
			WHERE id = ? AND provider_object_id IS NULL`, minor, cur, out.id); err != nil {
			return ref{}, err
		}
		out.retried = out.state == "failed"
		out.amountMinor, out.currency, out.state = minor, cur, "creating"
	}
	return out, tx.Commit()
}

// refByID reads a ref again.
func (s *Service) refByID(ctx context.Context, id int64) (ref, error) {
	return scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, id))
}

// nextKey moves a ref with no object on to the next key, once a lookup has found nothing a payer
// can use under the current one, if a call was made under the current one (a key that was never used
// holds no answer). Only from the key the try used: another try that moved it first is followed, and
// one that noted an object meanwhile is returned with it.
func (s *Service) nextKey(ctx context.Context, r ref) (ref, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1
		WHERE id = ? AND provider_object_id IS NULL AND key_seq = ? AND key_called_seq >= key_seq`, r.id, r.keySeq); err != nil {
		return ref{}, err
	}
	now, err := s.refByID(ctx, r.id)
	if err != nil {
		return ref{}, err
	}
	now.retried = r.retried
	return now, nil
}

// noteLink keeps the provider's answer on the ref: the object, the address, and that it is open, and
// returns the link to give. An answer that made an object is recorded whatever another try (one whose
// lease ran out, still working) noted of the ref meanwhile, a failure included: the link is about to
// be returned, and a ref that doesn't have it would let a later try make a second one. If the ref
// was changed by another try so that this answer was not recorded, the ref is read again and what it
// holds is the link, under the check every finished ref is given with (madeLink).
func (s *Service) noteLink(ctx context.Context, r ref, req paylink.Request, l Link) (paylink.Link, error) {
	res, err := s.store.db.ExecContext(ctx, `
		UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'
		WHERE id = ? AND provider_object_id IS NULL AND state IN ('creating', 'failed')`, l.ObjectID, l.URL, r.id)
	if err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == 2067 { // SQLITE_CONSTRAINT_UNIQUE: the object is another ref's
			slog.Error("integrations: a provider answered with an object that belongs to another reference", "integration", r.integrationID)
		}
		return paylink.Link{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return paylink.Link{}, err
	} else if n == 1 {
		return paylink.Link{URL: l.URL, ObjectID: l.ObjectID, AmountMinor: r.amountMinor, Currency: r.currency}, nil
	}
	// Another try noted its answer first (the same object, since the key is the same), or moved the ref
	// on: what the ref holds now is what is given, checked as any finished ref is.
	now, err := s.refByID(ctx, r.id)
	if err != nil {
		return paylink.Link{}, err
	}
	if now.objectID == "" {
		return paylink.Link{}, unavailable() // the ref has no object to give: the answer is not used
	}
	return madeLink(now, req)
}

// callFailed notes what a failed call means for the ref (nothing, when r is nil: a read made before
// the ref) and turns it into the entry's failure.
func (s *Service) callFailed(ctx context.Context, r *ref, rec record, err error) error {
	ce, ok := IsCall(err)
	if !ok {
		return err
	}
	if r != nil {
		switch {
		case ce.Kind != CallUnreachable:
			// A refusal ends the try: the ref is failed, and a retry of the entry starts it anew, which looks
			// for what the call may have made and moves to a key of its own before it calls again (the
			// provider may have kept even a 4xx under the key). The key moves here, from the one the
			// failed call used, when the provider said it kept the failure, so a late failure of an older
			// call (an entry done twice) can't skip a key that a later call is using.
			if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET state = 'failed', key_seq = key_seq + (? AND key_seq = ?)
				WHERE id = ? AND provider_object_id IS NULL`, boolInt(ce.KeyKept), r.keySeq, r.id); uerr != nil {
				return uerr
			}
		case ce.KeyKept:
			if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1 WHERE id = ? AND provider_object_id IS NULL AND key_seq = ?`, r.id, r.keySeq); uerr != nil {
				return uerr
			}
		}
	}
	// What is logged is what happened, never what was said: not the key, not the provider's words.
	slog.Warn("integrations: a payment link wasn't made", "integration", rec.ID, "provider", rec.Provider,
		"kind", int(ce.Kind), "status", ce.Status)
	switch ce.Kind {
	case CallCredentials:
		return &paylink.Error{Code: apierr.CodeIntegrationCredentialsRefused}
	case CallRefused:
		return &paylink.Error{Code: apierr.CodeIntegrationProviderRefused}
	case CallLookupIncomplete:
		return &paylink.Error{Code: apierr.CodeIntegrationLookupIncomplete}
	case CallSettled:
		return &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	return &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
