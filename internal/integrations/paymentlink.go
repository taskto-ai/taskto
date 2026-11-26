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
// An answer the provider keeps under the key (a 5xx), and a request that may have reached it and got
// no answer, leave the outcome unknown: the call may have made the object, now or a moment later.
// Stripe says not to "retry with a new idempotency key until you've resolved the original outcome"
// (https://docs.stripe.com/error-low-level#server-errors). So a ref whose outcome is unknown never gets
// a new key by itself: its entry repeats only the lookup, which reads the provider for an object of the
// ref's token, with a growing wait between reads and for at most outcomeWait. An object that turns up
// is adopted as if the call had answered it. If none does, the entry fails with
// integration_outcome_unknown, and an admin checks the provider's account and either confirms that no
// link exists, which allows exactly one new key and runs the entry again (ConfirmNoPaymentLink), or
// retries the entry once the object is visible, which adopts it.
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
// since included. So a try that looks for the ref's object (a ref with a refusal kept under its key,
// a failed ref tried again, one with a key of its own) and finds none that a payer can use moves the
// ref on to a key of its own before it makes one, but only when the earlier call's outcome is known: it
// was refused, or the object it made is seen expired and unpaid. The need to look is kept on the ref
// (lookup_required) until a lookup has read the provider in full, so a try that ends before its lookup
// does is followed by one that looks.
//
// Nothing is saved as the link before its state is read from the provider and says open (a Checkout
// Session) or active (a Payment Link): what a call answers, and a replay of a kept key above all, is the
// object as it was made, and it may have expired since. An expired one is never given; a completed one
// stops the call (nothing is made, a person reconciles the payment).
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
		return paylink.Link{}, s.callFailed(ctx, nil, rec, ce, false)
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
	hosted := ref.successURL == ""
	for range maxKeysPerTry {
		// What has to be settled before any call. A key of its own, a refusal kept, and a lookup that never
		// finished each mean an earlier call may have made an object, and so does an outcome that is
		// unknown; and a call made under a key the provider may have forgotten (a crash, with nothing
		// said of it) may have made one that its key no longer finds. Another call could make a second
		// payable link for the same payment, so the object is looked for first, and adopted.
		stale := !ref.lookupRequired && ref.keyCalledSeq >= ref.keySeq && s.store.now().Sub(since) >= keyTrusted
		if ref.keySeq > 0 || ref.lookupRequired || ref.unknownAt != "" || stale {
			nl := ref.newLink(minor, cur, req.Description)
			found, ok, expired, err := provider.FindPaymentLink(ctx, creds, client, nl, since)
			if err != nil {
				return paylink.Link{}, s.callFailed(ctx, &ref, rec, err, false)
			}
			if ok {
				state, link, err := s.settle(ctx, provider, creds, client, rec, &ref, req, hosted, found)
				if err != nil {
					return paylink.Link{}, err
				}
				if state == LinkOpen {
					slog.Info("integrations: a payment link made by an earlier call was adopted", "integration", ref.integrationID, "provider", rec.Provider)
					return link, nil
				}
				expired = true // it was open a moment ago and is not now: the call that made it is accounted for
			}
			switch {
			case expired:
				// Seen, expired and never paid: the earlier call's outcome is known, whatever it was before.
				if ref, err = s.nextKey(ctx, ref, true); err != nil {
					return paylink.Link{}, err
				}
			case ref.unknownAt != "" || stale:
				// The provider shows nothing of the call whose outcome is unknown. It may still make the
				// object, so nothing is called: the lookup is repeated until the wait ends.
				return paylink.Link{}, s.stillUnknown(ctx, ref)
			default:
				// Nothing a payer can use is the ref's, and the earlier call's outcome is known. The key may
				// still hold the provider's answer to it (a refusal, or an object that has expired since),
				// which the same key would replay: the call is made under a key of its own.
				if ref, err = s.nextKey(ctx, ref, false); err != nil {
					return paylink.Link{}, err
				}
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
		made, err := provider.CreatePaymentLink(ctx, creds, client, ref.newLink(minor, cur, req.Description))
		if err != nil {
			return paylink.Link{}, s.callFailed(ctx, &ref, rec, err, true)
		}
		state, link, err := s.settle(ctx, provider, creds, client, rec, &ref, req, hosted, made)
		if err != nil {
			return paylink.Link{}, err
		}
		if state == LinkOpen {
			return link, nil
		}
		// What the call answered (a replay of a kept key, a day on) has expired: not given. Its outcome is
		// known, and the call is made again under a key of its own.
		if ref, err = s.nextKey(ctx, ref, true); err != nil {
			return paylink.Link{}, err
		}
		if ref.objectID != "" {
			return madeLink(ref, req)
		}
	}
	return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

// settle reads the state of an object a call answered or a lookup found, at the provider, and saves
// it as the ref's link only if a payer can use it now: open, for a Checkout Session; active, for a
// Payment Link. It returns the state it found and, for LinkOpen, the link. An expired object is not
// saved (its caller moves on); a settled one stops the entry (a person reconciles it). If the state can't
// be read, the object is not saved either, and the ref's outcome is marked unknown: the object is there
// (a call answered it) but no call may be made for another until a lookup finds it.
func (s *Service) settle(ctx context.Context, provider PaymentProvider, creds Credentials, client *Client, rec record, r *ref, req paylink.Request, hosted bool, l Link) (LinkState, paylink.Link, error) {
	state, err := provider.LinkState(ctx, creds, client, hosted, l.ObjectID)
	if err != nil {
		if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET outcome_unknown_at = coalesce(outcome_unknown_at, ?)
			WHERE id = ? AND provider_object_id IS NULL`, s.store.stamp(s.store.now()), r.id); uerr != nil {
			return 0, paylink.Link{}, uerr
		}
		return 0, paylink.Link{}, s.callFailed(ctx, r, rec, err, false)
	}
	switch state {
	case LinkOpen:
		link, err := s.noteLink(ctx, *r, req, l)
		return state, link, err
	case LinkSettled:
		return state, paylink.Link{}, s.callFailed(ctx, r, rec, &CallError{Kind: CallSettled}, false)
	}
	return state, paylink.Link{}, nil
}

// stillUnknown is the entry's answer while a ref's outcome is unknown and the lookup shows nothing:
// wait and look again, and once outcomeWait has passed, fail for a person. Neither makes a call that
// creates anything.
func (s *Service) stillUnknown(ctx context.Context, r ref) error {
	now := s.store.now()
	since := now
	if r.unknownAt == "" {
		// A call under a key the provider may have forgotten, whose answer was never noted: the wait
		// begins now.
		if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET outcome_unknown_at = ? WHERE id = ? AND outcome_unknown_at IS NULL AND provider_object_id IS NULL`,
			s.store.stamp(now), r.id); err != nil {
			return err
		}
	} else if t, err := time.Parse(timeFormat, r.unknownAt); err == nil {
		since = t
	}
	elapsed := now.Sub(since)
	if elapsed >= outcomeWait {
		slog.Warn("integrations: a payment link's outcome is still unknown; an admin decides", "integration", r.integrationID)
		return &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
	}
	wait := min(max(elapsed/2, lookupWaitMin), lookupWaitMax, outcomeWait-elapsed)
	return &paylink.Error{Code: apierr.CodeIntegrationLinkLookupPending, Wait: max(wait, time.Second)}
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

const (
	// outcomeWait is how long a ref whose outcome is unknown is only looked for: Stripe's own
	// reconciliation of a request answered with an error is within its day of idempotency, and the lookup
	// reads a list that includes the newest objects at once, so six hours is long past the time an
	// object that was going to appear has. Then the entry fails (integration_outcome_unknown).
	outcomeWait = 6 * time.Hour
	// The wait between two lookups is half of the time the outcome has been unknown, within these:
	// quickly at first (the object usually shows at once), then rarely.
	lookupWaitMin = 30 * time.Second
	lookupWaitMax = 30 * time.Minute
	// maxKeysPerTry bounds how many calls one try makes: a call whose object turns out expired is
	// followed by one under a key of its own, once.
	maxKeysPerTry = 3
)

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
	// lookupRequired and unknownAt are the stored needs of the ref's next try: a lookup that read the
	// provider in full before any call, and when the outcome of a call became unknown ("" when it is
	// known) (the schema has the whole account).
	lookupRequired bool
	unknownAt      string
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
	amount_minor, currency, key_seq, key_called_seq, state, created_at, account_id, account_mode, success_url, cancel_url,
	lookup_required, coalesce(outcome_unknown_at, '')`

func scanRef(row interface{ Scan(...any) error }) (ref, error) {
	var r ref
	err := row.Scan(&r.id, &r.token, &r.integrationID, &r.appID, &r.recordID, &r.slot, &r.objectID, &r.url,
		&r.amountMinor, &r.currency, &r.keySeq, &r.keyCalledSeq, &r.state, &r.createdAt, &r.accountID, &r.accountMode, &r.successURL, &r.cancelURL, &r.lookupRequired, &r.unknownAt)
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
// been corrected); a failed one starts anew and is looked into before it is called again (the need is
// stored on it).
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
		// A failed ref starts anew and must be looked into before it is called again: what the provider kept
		// of its failed call is not to be asked again under the same key. That is kept on the ref, not in
		// this try, which may end before its lookup does.
		if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET amount_minor = ?, currency = ?, state = 'creating',
				lookup_required = max(lookup_required, state = 'failed')
			WHERE id = ? AND provider_object_id IS NULL`, minor, cur, out.id); err != nil {
			return ref{}, err
		}
		out.lookupRequired = out.lookupRequired || out.state == "failed"
		out.amountMinor, out.currency, out.state = minor, cur, "creating"
	}
	return out, tx.Commit()
}

// refByID reads a ref again.
func (s *Service) refByID(ctx context.Context, id int64) (ref, error) {
	return scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, id))
}

// nextKey ends a lookup that found nothing a payer can use: the need to look is met, and if a call was
// made under the current key (a key that was never used holds no answer) the ref moves on to the next.
// resolved says that the lookup saw an object of the ref that no payer can use, which accounts for the
// call whose outcome was unknown: it is no longer. Only from the key the try used: another try that
// moved it first is followed, and one that noted an object meanwhile is returned with it.
func (s *Service) nextKey(ctx context.Context, r ref, resolved bool) (ref, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET lookup_required = 0,
			outcome_unknown_at = CASE WHEN ? THEN NULL ELSE outcome_unknown_at END,
			key_seq = key_seq + (key_called_seq >= key_seq)
		WHERE id = ? AND provider_object_id IS NULL AND key_seq = ?`, resolved, r.id, r.keySeq); err != nil {
		return ref{}, err
	}
	return s.refByID(ctx, r.id)
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
// the ref) and turns it into the entry's failure. created says that the failure is of the call that
// makes the object, not of a read.
func (s *Service) callFailed(ctx context.Context, r *ref, rec record, err error, created bool) error {
	ce, ok := IsCall(err)
	if !ok {
		return err
	}
	if r != nil {
		switch {
		case ce.OutcomeUnknown:
			// The call may have been executed: the provider may still make the object. No new key is taken
			// until that is resolved, whichever key the call used (the older call of an entry done twice
			// is as unknown as the newer one).
			if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET outcome_unknown_at = coalesce(outcome_unknown_at, ?)
				WHERE id = ? AND provider_object_id IS NULL`, s.store.stamp(s.store.now()), r.id); uerr != nil {
				return uerr
			}
		case ce.Kind != CallUnreachable:
			// A refusal ends the try: the ref is failed, and a retry of the entry starts it anew, which looks
			// for what the call may have made and moves to a key of its own before it calls again (the
			// provider may have kept even a 4xx under the key). The key moves here, from the one the
			// failed call used, when the provider said it kept the failure, so a late failure of an older
			// call (an entry done twice) can't skip a key that a later call is using.
			if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET state = 'failed', lookup_required = 1, key_seq = key_seq + (? AND key_seq = ?)
				WHERE id = ? AND provider_object_id IS NULL`, boolInt(ce.KeyKept), r.keySeq, r.id); uerr != nil {
				return uerr
			}
		case created:
			// An answer that says the call was not executed (rate limited, a conflict, a timeout of the
			// provider's own): the key was not used.
			if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_called_seq = key_seq - 1
				WHERE id = ? AND provider_object_id IS NULL AND key_called_seq = ? AND key_seq = ?`, r.id, r.keySeq, r.keySeq); uerr != nil {
				return uerr
			}
		}
	}
	// What is logged is what happened, never what was said: not the key, not the provider's words.
	slog.Warn("integrations: a payment link wasn't made", "integration", rec.ID, "provider", rec.Provider,
		"kind", int(ce.Kind), "status", ce.Status)
	if ce.OutcomeUnknown {
		return &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
	}
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
