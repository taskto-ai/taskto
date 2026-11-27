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

// The create_payment_link effect's side of the integrations (apps/paymentlink.go asks for it).
//
// One outbox entry makes at most one payment link a payer can use. The entry has one ref
// (integration_refs, unique by the entry's number), and the ref has keys (integration_ref_attempts):
// each an idempotency key with a random token of its own, which the call under it puts in the object
// (client_reference_id, metadata[taskto_ref]) and the payment echoes back. The first key's token is
// the ref's.
//
// The rule is never to risk a second payable link for one ref, so the server never decides after the
// fact that a call didn't reach the provider. Before every call under a key, the call is written
// ahead on the key's row (sends), durably, and that record is never taken back. A key is resolved
// only by
//
//   - an answer (2xx) whose object was then read at the provider (LinkState);
//   - a lookup that finds an object carrying the key's own token (FindPaymentLink), never another
//     key's; or
//   - an answer the provider documents as given before execution (CallError.NotExecuted: a rate
//     limit, a lock timeout, the key in use by a request still running), or a request that never
//     left (no connection was handed to it): such a call counts as clean (clean_sends), and the same
//     key is asked again later, never a new one.
//
// Every other end of a call (any 4xx or 5xx, a timeout, a dropped connection, an answer that can't be
// read, a crash between the write-ahead and the answer being noted) leaves the key's outcome unknown:
// sends exceed clean_sends. A ref whose key's outcome is unknown never gets a new key by itself. Each
// try of its entry looks for the key's object by its token and, while the provider still keeps the key
// (keyTrusted from its first call), asks again under the same key with the same parameters, which the
// provider answers with what the first call made, or runs if it never ran. A failed read or call
// changes nothing: whatever the failure (the provider unreachable, the key refused, an answer that
// can't be read, a list that can't be read whole), the read of whose account the key is, the lookup and
// each of its pages, and the read of an object's state alike, the entry waits and tries again on the
// reconciliation schedule (a growing wait, outcomeWait in all from when the outcome became unknown),
// which uses up none of its tries and nothing ends early. If that ends
// with the outcome still unknown, the entry fails with integration_outcome_unknown, and an admin who
// finds no link at the provider confirms it (ConfirmNoPaymentLink): the only way to a new key for such
// a ref.
//
// A key moves on by itself only once its outcome is known and left nothing a payer can use: its object
// was seen expired and never paid (or an admin confirmed it made nothing). And only while every key of
// the ref is so: none whose outcome is unknown, made or settled, and none abandoned with an object on
// record (nextAttempt). One that was paid, or can't be used for a reason that doesn't say it
// wasn't, stops the entry (integration_link_already_paid) for a person to reconcile.
//
// Nothing is saved as the link before its state is read from the provider and says open (a Checkout
// Session) or active (a Payment Link): what a call answers, and a replay of a kept key above all, is the
// object as it was made, and it may have expired since. An object an answer named is kept on the key
// before its state is read, so a try that can't read it doesn't lose it. So is one a lookup found
// carrying the key's token, in every case: one it couldn't check (a Payment Link whose line items
// couldn't be read) or that doesn't ask what the ref asks is kept unchecked (object_verified 0), which
// blocks a new key and an admin's confirmation that none exists as any object on record does, and is
// adopted only once a later lookup checks it (the entry waits, and then fails for a person).
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
	if found && (ref.objectID != "" || paidWithoutLink(ref)) {
		return s.savedLink(ctx, rec, provider, ref, req)
	}
	if found && ref.state == "expired" {
		// Retired with no link: every key of it was confirmed to have left nothing a payer can use, and a
		// newer entry of the record made the record's link in its place (takeEarlier). Nothing more is
		// made for it.
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	if found && ref.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged} // before anything is asked of anyone
	}
	if !found && req.LookupOnly {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationConditionChanged} // nothing was made
	}
	// A failure that tells nothing (the provider didn't answer) waits on the reconciliation's schedule
	// while the ref's key has an unknown outcome, and is an ordinary transient failure otherwise.
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
		// A key whose outcome is unknown is still looked for, whatever the limits say now: they refuse a
		// new call (read again before each one leaves, limitsAllow), never the lookup of what an earlier
		// call may have made. The ref asks what it was made for.
		minor, cur = ref.amountMinor, ref.currency
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
		return paylink.Link{}, s.failed(ctx, rec, pending, err)
	}
	// With no public URL there are no pages of ours: both are empty and the provider hosts the
	// payer's confirmation (a link is made all the same). A ref already made keeps its own.
	success, cancel := s.pageURLs()
	mode := Lookup(rec.Provider).Mode(rec.Settings, creds.APIKey.Reveal())
	who := whose{account: account, mode: mode}
	for i := 0; ; i++ {
		// An entry with no ref of its own reads the record's earlier links first: one a payer may still
		// use is given again, and nothing is made (earlierLink).
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
			continue // the record's newest link changed since it was read: read it again
		}
		if err != nil {
			if errors.Is(err, errRecheck) {
				return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
			}
			return paylink.Link{}, err
		}
		break
	}
	if ref.objectID != "" { // made by a call that finished while this one began
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
			// The ref has its link, or was retired, since it was read: what it holds is the answer.
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
			// The key is accounted for and left nothing a payer can use: the ref takes the next one.
			if ref, err = s.nextAttempt(ctx, ref, a); err != nil {
				return paylink.Link{}, err
			}
			if ref.objectID != "" {
				return madeLink(ref, req)
			}
			if ref.keySeq == a.seq {
				// Not moved on: a key of the ref may hold something a payer can use. A person decides.
				slog.Warn("integrations: a payment link's key can't move on; an admin decides", "ref", ref.id)
				return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
			}
			continue
		case attemptMade:
			// Its object was noted on the ref in the same transaction: another try finished meanwhile.
			return s.refHolds(ctx, ref, req)
		}

		if a.unknown() {
			// Resolve the key before anything else: an object an answer named, or one carrying its token.
			res, link, err := t.resolve(ctx, &ref, a)
			switch {
			case err != nil:
				return paylink.Link{}, err
			case res == resolvedOpen:
				return link, nil
			case res == resolvedExpired:
				continue
			}
			// Nothing of the key was seen. Only the same key is asked again, and only while the provider
			// still keeps it: it answers with what the first call made, or runs a call that never ran.
			if !s.keyKept(a) {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
		}

		// Only looking: nothing is sent that could make a link. A key whose outcome is unknown waits on
		// the lookup (and then an admin decides); a fresh one (after an expired key) holds nothing.
		if req.LookupOnly {
			if a.unknown() {
				return paylink.Link{}, s.stillUnknown(ctx, a)
			}
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationConditionChanged}
		}
		// The admin's limits as they are now: a try that moved on from an expired key asks for a new link,
		// and a ceiling lowered since the try began refuses it. They are read in the transaction that
		// writes the call ahead (which holds the write lock, so a change of the limits commits before it,
		// and is seen, or after it), and once more right before the call leaves. A key whose outcome is
		// unknown is not asked again past them; it is only looked for.
		refusedBy := func(err error) error {
			if a.unknown() {
				return s.stillUnknown(ctx, a)
			}
			return err
		}
		// The call is written ahead before it leaves, and that record is never taken back.
		if ok, refused, err := s.writeAhead(ctx, ref, a, req); err != nil {
			return paylink.Link{}, err
		} else if refused != nil {
			return paylink.Link{}, refusedBy(refused)
		} else if !ok {
			// Another try resolved the key or moved the ref on meanwhile: read it again.
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
		// A change of the limits committed after the write-ahead stops the call that hasn't left: it is
		// noted as one that never ran, so the key's outcome is as it was before it.
		if refused := s.limitsAllow(ctx, s.store.db, req); refused != nil {
			if errors.As(refused, new(*paylink.Error)) {
				if a, err = s.noteNotExecuted(ctx, a); err != nil {
					return paylink.Link{}, err
				}
				return paylink.Link{}, refusedBy(refused)
			}
			return paylink.Link{}, refused
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
			// Nothing ran under the key: it is asked again later, as any failure that may pass.
			return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
		}
		if s.afterCall != nil {
			if err := s.afterCall(); err != nil {
				return paylink.Link{}, err
			}
		}
		// The answer's object is kept on the key before its state is read: a try that can't read it finds
		// it there, and never loses it.
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
		// What the call answered (a replay of a kept key, a day on) has expired: not given. The key is
		// accounted for, and the loop moves on to the next.
	}
	return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

// try is what one try of an entry calls the provider with.
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

// resolution is what a try learned of a key.
type resolution int

const (
	// unresolved: nothing was seen of the key's object; its outcome is as it was.
	unresolved resolution = iota
	// resolvedOpen: its object can take a payment, and is the ref's link now.
	resolvedOpen
	// resolvedExpired: its object was seen expired and never paid; the ref may take the next key.
	resolvedExpired
)

// resolve looks for what a key whose outcome is unknown made: the object an answer named (or a lookup
// checked), read again at the provider, or else one carrying the key's token, found by a lookup (and
// kept on the key before its state is read, as an answer's is). A lookup that fails, or can't read the
// whole account, tells nothing: the outcome stays unknown. One that may pass (unreachable, a list not
// read whole) lets the same key be asked again; any other (the key refused, a failure that isn't the
// provider's answer) waits on the reconciliation's schedule at once. It fails the try only for what a
// person must see now: an object of the key that was paid or can't be given.
//
// An object of the key's token the lookup saw and couldn't check (its amount unread) or refused is kept
// on the key unchecked, whatever happens next: it is on record, so the key's outcome can't be confirmed
// away and the ref takes no new key. Only a later lookup that checks it adopts it; until then the entry
// waits on the reconciliation's schedule (and then fails for a person, who checks it at the provider),
// and the key isn't asked again on top of it.
func (t try) resolve(ctx context.Context, r *ref, a attempt) (resolution, paylink.Link, error) {
	if a.objectID != "" && a.objectVerified {
		return t.settle(ctx, r, a, Link{ObjectID: a.objectID, URL: a.url})
	}
	seen := a.objectID != ""
	found, ok, expired, err := t.provider.FindPaymentLink(ctx, t.creds, t.client, r.newLink(a, t.req.Description), t.since)
	if err != nil {
		if found.ObjectID != "" {
			// Seen, before anything else: never lost, whatever the failure.
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
				// An object of the key's token that is not a link a payer can be given (another amount, a
				// state not said): never given, and no other key while it stands.
				logFailure(t.rec, ce)
				return unresolved, paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationProviderRefused}
			case CallUnreachable, CallLookupIncomplete:
				if !seen {
					logFailure(t.rec, ce)
					return unresolved, paylink.Link{}, nil // nothing learned: the same key may be asked again
				}
			}
		}
		// The key refused, a failure that isn't the provider's answer, or an object of the key that is on
		// record and still unchecked: nothing to ask the same key with. The entry waits on the
		// reconciliation's schedule.
		return unresolved, paylink.Link{}, t.s.readFailed(ctx, t.rec, a, err)
	}
	switch {
	case ok && a.objectID != "" && found.ObjectID != a.objectID:
		// Another object of the key's token than the one on record: two objects of one key, and nothing
		// says which the call made. The one on record stays (it may be open and paid), neither is given,
		// and no other key is taken: the entry waits, and then fails for a person.
		slog.Warn("integrations: a lookup found a second object of a key's token; an admin decides", "ref", r.id)
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	case ok:
		slog.Info("integrations: a payment link made by an earlier call was found", "integration", r.integrationID, "provider", t.rec.Provider)
		// Checked, and kept on the key before its state is read: a try that can't read it finds it there,
		// and an admin can't confirm that none exists.
		if err := t.s.noteObject(ctx, a, found); err != nil {
			return unresolved, paylink.Link{}, err
		}
		return t.settle(ctx, r, a, found)
	case expired:
		if a.objectID != "" && found.ObjectID != a.objectID {
			// The key has another object on record than the one seen expired: that says nothing of the one
			// on record, which may be open. The entry waits, and a person sees it if it never resolves.
			return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
		}
		// And as the key is now, not as this try read it: another may have noted an object meanwhile.
		if ok, err := t.s.markExpiredOf(ctx, a, found.ObjectID); err != nil {
			return unresolved, paylink.Link{}, err
		} else if !ok {
			return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
		}
		return resolvedExpired, paylink.Link{}, nil
	}
	if seen {
		// The object on record wasn't found this time: nothing says what became of it. The entry waits.
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	return unresolved, paylink.Link{}, nil
}

// settle reads the state of an object of the key (one a call answered or a lookup found) at the
// provider, and saves it as the ref's link only if a payer can use it now: open, for a Checkout
// Session; active, for a Payment Link. An expired object resolves the key as expired, and a settled one
// stops the entry (a person reconciles it). If the state can't be read, whatever the failure (the key
// refused included), nothing is resolved: the object stays on the key, and the entry waits on the
// reconciliation's schedule.
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
	// Expired: the key's outcome, only while no other object of the key's token may still be paid (one
	// whose answer was lost, say, beside the one that expired): the token's objects are listed again,
	// and the key moves on only when that lookup is whole and finds nothing a payer could use. Otherwise
	// the entry waits, and then fails for a person.
	if !t.noOtherPayable(ctx, r, a, l.ObjectID) {
		slog.Warn("integrations: an object of a key expired beside another that may be paid; the entry waits", "ref", r.id)
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	// And only while this is the object on record (or none is): another object of the key may be open.
	if ok, err := t.s.markExpiredOf(ctx, a, l.ObjectID); err != nil {
		return unresolved, paylink.Link{}, err
	} else if !ok {
		return unresolved, paylink.Link{}, t.s.stillUnknown(ctx, a)
	}
	return resolvedExpired, paylink.Link{}, nil
}

// noOtherPayable lists the objects of the key's token again after the object expired named expired: it
// says true only when that lookup is whole and finds none a payer could use (expired ones, or none).
// Any failure, an object it can't read or check, or one it would adopt says there may be one.
func (t try) noOtherPayable(ctx context.Context, r *ref, a attempt, expired string) bool {
	found, ok, _, err := t.provider.FindPaymentLink(ctx, t.creds, t.client, r.newLink(a, t.req.Description), t.since)
	if err == nil && !ok {
		return true
	}
	slog.Info("integrations: another object of an expired key's token may be paid", "ref", r.id, "expired", expired != "", "seen", found.ObjectID != "")
	return false
}

// markExpiredOf gives the key the outcome expired for its object objectID, only while the object on
// record is that one or none: says false, and changes nothing, otherwise.
func (s *Service) markExpiredOf(ctx context.Context, a attempt, objectID string) (bool, error) {
	res, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'expired'
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND (object_id IS NULL OR object_id = ?)`, a.refID, a.seq, objectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// settleAttempt notes that the key's object was paid, or can't be used for a reason that doesn't say it
// wasn't, and is the entry's failure: nothing else is made for the ref, and a person reconciles it.
func (s *Service) settleAttempt(ctx context.Context, a attempt) error {
	if err := s.markAttempt(ctx, a, attemptSettled); err != nil {
		return err
	}
	return &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
}

// failed is the entry's answer to a failed read made before the ref is ready (whose account the key
// is). While the ref's key has an unknown outcome, any failure waits on the reconciliation's schedule
// (readFailed); otherwise it is the failure's own code.
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

// readFailed is the entry's answer to a read at the provider that failed while key a's outcome is
// unknown: whose account the key is, the lookup (any of its pages, and a link's line items), or an
// object's state. No such failure tells anything of what the key made, whatever it is: the provider
// unreachable, the key refused, an answer that can't be read, or an error that isn't the provider's
// answer at all. So none ends the entry early or uses up its tries: it waits on the reconciliation's
// schedule (stillUnknown), whose own bound ends it with integration_outcome_unknown.
func (s *Service) readFailed(ctx context.Context, rec record, a attempt, err error) error {
	if ce, ok := IsCall(err); ok {
		logFailure(rec, ce)
	} else {
		// Not the provider's answer; never logged as it is either, since it may quote the address.
		slog.Warn("integrations: a read for a payment link failed", "integration", rec.ID, "provider", rec.Provider)
	}
	return s.stillUnknown(ctx, a)
}

// logFailure logs what happened, never what was said: not the key, not the provider's words.
func logFailure(rec record, ce *CallError) {
	slog.Warn("integrations: a call for a payment link failed", "integration", rec.ID, "provider", rec.Provider,
		"kind", int(ce.Kind), "status", ce.Status, "not_executed", ce.NotExecuted, "outcome_unknown", ce.OutcomeUnknown)
}

// stillUnknown is the entry's answer while a key's outcome is unknown and nothing resolved it: wait
// and try again, and once outcomeWait has passed since the outcome became unknown, fail for a person
// with integration_outcome_unknown (what ConfirmNoPaymentLink accepts). Neither takes a new key.
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

// keyKept says whether the provider still keeps the key's first call, so that a call under the same
// key is answered with what that call made (or runs, if it never did) and can't make a second object.
func (s *Service) keyKept(a attempt) bool {
	first, err := time.Parse(timeFormat, a.firstSentAt)
	return err == nil && s.store.now().Sub(first) < keyTrusted
}

// madeLink is the link a ref already has. It belongs to the integration that made it: another one,
// bound since, is not the one the record may be written with, and no second link is made for the
// entry. It is checked wherever a finished ref is found, however late.
func madeLink(r ref, req paylink.Request) (paylink.Link, error) {
	if r.integrationID != req.IntegrationID {
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationChanged}
	}
	if r.url == "" {
		// Paid before the entry noted its link (a verified payment event learned the object): there is
		// no address to give, and none is made. The record is the event's to settle; the entry stops.
		return paylink.Link{}, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
	}
	return r.link(), nil
}

// savedLink is the link the entry saved on an earlier try, given again (the try that saved it stopped
// before the record held it, or the entry is done again): only if a payer can still use it. It was
// open when it was saved, and may have expired or been paid since, so its state is read again at the
// provider, as the object of an answer is before it is saved (try.settle): an open one is given; an
// expired or paid one, or one the ref's own events ended, is not, and nothing else is made for the
// entry (integration_link_already_paid: a person reconciles it). A read that fails tells nothing, and
// gives nothing and makes nothing: the entry tries again.
func (s *Service) savedLink(ctx context.Context, rec record, provider PaymentProvider, r ref, req paylink.Request) (paylink.Link, error) {
	if r.integrationID != req.IntegrationID || r.url == "" {
		return madeLink(r, req)
	}
	if r.state == "paid" || r.state == "refunded" || r.state == "expired" {
		// A verified event of the ref ended the link: no payer can use it.
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
	// The object is the ref's mode's: a key of the other mode can't read it, and says nothing of it.
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
	// And the account's: the object is looked for only where it was made. A key of another account
	// can't read it and says nothing of it; a rotated key of the same account reads it.
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

// paidWithoutLink is a ref a payment settled before its entry had the link: it has no address.
func paidWithoutLink(r ref) bool {
	return r.url == "" && (r.state == "paid" || r.state == "refunded")
}

// refHolds is what the ref holds now, read again: its link, checked as any finished ref is, or
// nothing to give.
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
	// outcomeWait is how long a key whose outcome is unknown is reconciled by itself: Stripe's own
	// reconciliation of a request answered with an error is within its day of idempotency, and the lookup
	// reads a list that includes the newest objects at once, so six hours is long past the time an
	// object that was going to appear has. Then the entry fails (integration_outcome_unknown).
	outcomeWait = 6 * time.Hour
	// The wait between two tries is half of the time the outcome has been unknown, within these:
	// quickly at first (the object usually shows at once), then rarely.
	lookupWaitMin = 30 * time.Second
	lookupWaitMax = 30 * time.Minute
	// maxKeysPerTry bounds how many keys one try goes through: a call whose object turns out expired
	// is followed by one under a key of its own.
	maxKeysPerTry = 3
)

// keyTrusted is how long from its first call a key is known to be kept by the provider (Stripe keeps
// one for at least 24 hours, and may remove it after; a margin is left). Past it, a call under the
// key could run again and make a second object, so it is not made.
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

// limitsAllow says whether the integration may make the request's link now: it is still there, on and
// allowed in the app's space, and the amount and currency are within the admin's limits as they are now.
// It reads through q: the write-ahead's transaction, or the database.
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

// ----- the refs and their keys -----

// ref is a row of integration_refs.
type ref struct {
	id, integrationID, appID, recordID int64
	token, slot                        string
	objectID, url                      string
	amountMinor                        int64
	currency                           string
	// keySeq is the number of the ref's current key (integration_ref_attempts.seq).
	keySeq    int
	state     string
	createdAt string
	// accountID is the provider's account the ref was made under and accountMode the mode of the key
	// that read it (test or live), and successURL and cancelURL the pages its calls ask the payer to
	// return to, both empty for a link that is hosted by the provider.
	accountID, accountMode, successURL, cancelURL string
	// paidObjectID and paymentID are what the verified event that paid the ref said: the provider's
	// object the payment arrived on and the payment itself (apply.go), empty until then.
	paidObjectID, paymentID string
	// effectID is the outbox entry the ref is for now (0 once it was cleaned up).
	effectID int64
	// paidLinkID is the provider's link the payment that paid the ref came through, when the event
	// was about another object (a Checkout Session of a Stripe Payment Link): "" otherwise.
	paidLinkID string
	// linkExpiredAt is when the ref's own link was seen expired ("" if it wasn't): kept apart from its
	// state, which stays paid for a ref paid on another key's object.
	linkExpiredAt string
	// amountField and currencyField are the record's fields the ref's money was read from ("" for a
	// ref made before refs kept them, or an effect's own currency).
	amountField, currencyField string
	// polls counts the reads the reconciliation polling made of the ref's object (poll.go).
	polls int
}

// newLink is what the provider is asked for under key a: the key's token, the ref's money and the
// pages of its first try, and the entry's description (fixed when it was queued).
func (r ref) newLink(a attempt, description string) NewLink {
	return NewLink{Token: a.token, KeySeq: a.seq, AmountMinor: r.amountMinor, Currency: r.currency, Description: description,
		SuccessURL: r.successURL, CancelURL: r.cancelURL}
}

// whose is who a ref's calls are made as: the account the key reads and its mode.
type whose struct{ account, mode string }

func (r ref) link() paylink.Link {
	return paylink.Link{URL: r.url, ObjectID: r.objectID, AmountMinor: r.amountMinor, Currency: r.currency}
}

// refColumns are integration_refs' own, so a query of that table alone reads a ref.
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

// refOf finds the ref of the outbox entry.
func (s *Service) refOf(ctx context.Context, effectID int64) (ref, bool, error) {
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, effectID))
	if errors.Is(err, sql.ErrNoRows) {
		return ref{}, false, nil
	}
	return r, err == nil, err
}

// refByToken finds the ref a token the server issued belongs to: the token of any of its keys (the
// first key's is the ref's own). A payment echoes the token of the key whose object was paid, which is
// a later key's after an expired one, so this, and never integration_refs.token alone, is how a
// payment finds its ref.
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

// beginRef makes the entry's ref, or finds it, ready for a call: in one transaction that holds the
// write lock, so two workers that took the same entry find one ref.
// What a ref was made with is the first try's and is never updated: its account and the key's mode,
// its integration, its pages and its money (an entry asks the same every time: its payload is fixed
// when it is queued). A ref with no object that was made in another account or mode than who is left
// exactly as it is and the try is refused (whatever its calls made is where they were made), and so is
// one made under another integration than the request's (integration_changed), or for other money.
//
// An entry that has no ref yet takes the record's earlier one still being made instead of making its
// own, in the same transaction (takeEarlier): one link a payer may use per record.
// checked are the record's earlier refs with an object that earlierLink read at the provider and found
// unable to take payment: only over those is a new ref made.
func (s *Service) beginRef(ctx context.Context, req paylink.Request, minor int64, cur string, who whose, successURL, cancelURL string, checked []int64) (ref, error) {
	token, err := newToken()
	if err != nil {
		return ref{}, err
	}
	// The transaction begins IMMEDIATE (the database's _txlock): it holds the write lock from its
	// first statement, so the record's earlier ref is read and taken, or a new one made, by one entry
	// at a time.
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
		// Not what the ref was made for (its keys ask what it asked): the record's earlier link, still
		// being made, asks other money. Nothing else is made while it may still be paid.
		return ref{}, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
	}
	return out, tx.Commit()
}

// refByID reads a ref again.
func (s *Service) refByID(ctx context.Context, id int64) (ref, error) {
	return scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, id))
}

// The outcomes of a key (integration_ref_attempts.outcome); "" while it has none.
const (
	attemptMade      = "made"
	attemptExpired   = "expired"
	attemptSettled   = "settled"
	attemptAbandoned = "abandoned"
)

// attempt is a row of integration_ref_attempts: one idempotency key of a ref and the calls under it.
type attempt struct {
	refID int64
	seq   int
	token string
	// sends is how many calls were written ahead under the key, cleanSends how many of them certainly
	// never ran.
	sends, cleanSends         int
	firstSentAt, unknownSince string
	outcome                   string
	// objectID and url are the object an answer named, or a lookup saw, before its state was read, and
	// objectVerified whether it may be adopted (an answer's, or one a lookup checked; url is "" if not).
	objectID, url  string
	objectVerified bool
}

// unknown says whether the key may have made an object nobody has seen: a call under it may have run,
// and nothing resolved it.
func (a attempt) unknown() bool { return a.outcome == "" && a.sends > a.cleanSends }

const attemptColumns = `ref_id, seq, token, sends, clean_sends, coalesce(first_sent_at, ''), coalesce(unknown_since, ''),
	coalesce(outcome, ''), coalesce(object_id, ''), coalesce(url, ''), object_verified`

func scanAttempt(row interface{ Scan(...any) error }) (attempt, error) {
	var a attempt
	err := row.Scan(&a.refID, &a.seq, &a.token, &a.sends, &a.cleanSends, &a.firstSentAt, &a.unknownSince, &a.outcome, &a.objectID, &a.url, &a.objectVerified)
	return a, err
}

// attemptByKey reads a key's row.
func (s *Service) attemptByKey(ctx context.Context, refID int64, seq int) (attempt, error) {
	return scanAttempt(s.store.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM integration_ref_attempts WHERE ref_id = ? AND seq = ?`, refID, seq))
}

// attemptOf reads the ref's current key, if it has a row yet.
func (s *Service) attemptOf(ctx context.Context, r ref) (attempt, bool, error) {
	a, err := s.attemptByKey(ctx, r.id, r.keySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, nil
	}
	return a, err == nil, err
}

// currentAttempt is the ref's current key, its row made if it has none yet: the first key takes the
// ref's token, a later one a token of its own. Two tries that make it at once find one row. A row is
// made only while the key is the ref's current one and the ref can still note a link (creating, or
// failed), in the same statement: a ref a newer entry retired (takeEarlier), or that has its link,
// never takes a key again, whatever a try that read it before then does (errRefMoved).
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

// errRefMoved is currentAttempt's answer when the ref can no longer take the key it was read with.
var errRefMoved = errors.New("the payment link's reference moved on")

// writeAhead records, before a call under the key leaves, that it is sent: the key's outcome is unknown
// from here until something resolves it, whatever becomes of this process. It says false, and records
// nothing, when the key is no longer the ref's current one or has an outcome (another try moved on).
//
// The admin's limits are read in the same transaction (limitsAllow), which holds the write lock: refused
// says why they don't allow the call now, and nothing is written ahead then.
func (s *Service) writeAhead(ctx context.Context, r ref, a attempt, req paylink.Request) (ok bool, refused, err error) {
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

// noteNotExecuted records that a call under the key certainly never ran, and returns the key as it is
// now. Only when every call under it is such does the key hold nothing at the provider: its outcome is
// known again, and its window starts with its next call.
func (s *Service) noteNotExecuted(ctx context.Context, a attempt) (attempt, error) {
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET clean_sends = clean_sends + 1,
			first_sent_at = CASE WHEN clean_sends + 1 = sends THEN NULL ELSE first_sent_at END,
			unknown_since = CASE WHEN clean_sends + 1 = sends THEN NULL ELSE unknown_since END
		WHERE ref_id = ? AND seq = ? AND clean_sends < sends`, a.refID, a.seq); err != nil {
		return attempt{}, err
	}
	return s.attemptByKey(ctx, a.refID, a.seq)
}

// noteObject keeps the object an answer under the key named, or one a lookup found and checked, before
// its state is read: one that may be adopted.
func (s *Service) noteObject(ctx context.Context, a attempt, l Link) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = ?, url = ?, object_verified = 1
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND (object_id IS NULL OR object_id = ?)`, l.ObjectID, l.URL, a.refID, a.seq, l.ObjectID)
	return err
}

// noteSeen keeps an object of the key's token that a lookup saw and couldn't adopt (unchecked, or not
// what the ref asks): on record, so nothing takes the key for one that made nothing, and never adopted
// as it is. It never replaces an object that may be adopted.
func (s *Service) noteSeen(ctx context.Context, a attempt, objectID string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = ?, url = NULL, object_verified = 0
		WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND object_verified = 0 AND (object_id IS NULL OR object_id = ?)`, objectID, a.refID, a.seq, objectID)
	return err
}

// markAttempt gives the key an outcome, if it has none.
func (s *Service) markAttempt(ctx context.Context, a attempt, outcome string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = ? WHERE ref_id = ? AND seq = ? AND outcome IS NULL`,
		outcome, a.refID, a.seq)
	return err
}

// nextAttempt moves the ref on from key a, whose outcome is known and left nothing a payer can use,
// and reads the ref again. Only from that key: another try that moved it first is followed, and one
// that noted an object meanwhile is returned with it. And only while every key of the ref left nothing a
// payer can use: none has an unknown outcome or an object that was made or settled, and none was
// abandoned with an object on record (an admin's "none exists" that something seen contradicts). An
// object a lookup saw and couldn't check is on record as any other is: the key holding it has no
// outcome while it stands (or settled), so it blocks the move, until the object itself is seen expired.
// And only for a ref that can still note a link (creating, or failed): one a newer entry retired
// (takeEarlier) takes no key again. A ref refused is read back still on key a.
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

// noteLink keeps the key's object on the ref as its link, and the key's outcome as made, in one
// transaction, and returns the link to give. An answer that made an object is recorded whatever another
// try noted of the ref meanwhile: the link is about to be returned, and a ref that doesn't have it would
// let a later try make a second one. If another try noted the ref's link first, what the ref holds is
// given, under the check every finished ref is given with (madeLink).
//
// Only the key that is still the ref's current one, with no outcome but made, notes its object: a try
// that read its object open and was slow to save it, while another try saw that object expired and moved
// the ref to the next key, never installs it (it can't be paid, and the next key's call makes the link).
// It reads the ref again, and the entry tries again. A ref marked failed meanwhile (a late answer under
// the same key) still notes it. Nor does an object other than the one the key has on record (noted by
// another try since this one read the key): that one may be paid, and stays.
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
		if errors.As(err, &se) && se.Code() == 2067 { // SQLITE_CONSTRAINT_UNIQUE: the object is another ref's
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
	// Another try noted its answer first (the same object, since the key is the same), or moved the ref
	// on: what the ref holds now is what is given, checked as any finished ref is. A ref with no link yet
	// is on another key: the entry tries again, and resolves that one.
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

// ----- one payable link per record -----
//
// An outbox entry has one ref, and a record may ask for its link more than once: the invoice is sent
// again (an action that runs the effect after an edit), or its expired link replaced. Each is a new
// entry, and a new ref would make a new link while the record's earlier one may still take payment:
// two links a payer could pay for one invoice. So an entry that has no ref of its own reads every
// earlier ref of the record first, whatever field, slot, integration or release it was made for (a
// release may write the link to another field, a slot may be bound to another integration since, and
// a database from before refs kept their fields may have several refs of the record), and makes a
// new one only when nothing of any of them can still be paid:
//
//   - one with a link that isn't finished (confirmed expired, or paid on that very link): its state is
//     read at the provider. Open, it is given again (the same link is written back, and nothing is
//     made), or, if it asks other money, reads it from other fields or isn't the record's only open
//     one, the entry fails with integration_link_still_open. Expired, or paid and complete, it no
//     longer counts. Settled while the ref doesn't know it paid, the entry stops for a person
//     (integration_link_already_paid). A read that fails makes nothing. One made under another
//     integration, account or mode can't be read with this key: the entry fails with the code that
//     says which.
//   - one still being made, or whose call's outcome is unknown: the entry takes it (it becomes the
//     entry's ref) and goes on resolving it as its own, so it waits on its reconciliation, adopts its
//     object, or fails for an admin exactly as the first entry would have. A ref under which no call
//     ever ran holds nothing at the provider: it is removed, in the transaction that holds the write
//     lock, which also stops any call under it from being written ahead (writeAhead needs the ref). One
//     whose every key is confirmed to have left nothing is retired. Two such refs of one record (left
//     by the writer before refs were per record) can't both be taken: the entry fails for an admin
//     (integration_outcome_unknown).
//   - one paid before it learned its link, or retired: it no longer counts.
//
// The reads at the provider happen before the transaction, which reads the record's refs again under
// the write lock: if one that may be paid wasn't checked, the entry reads them again (errRecheck).

// errRecheck is beginRef's answer when the record's refs changed since earlierLink checked them.
var errRecheck = errors.New("the record's payment links changed")

// recordRefs are the record's refs other than the entry's own, oldest first.
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

// paidOnOwnLink says whether the ref was paid on its own link: the payment's object is the link, or
// came from it (a Checkout Session of a one-use Stripe Payment Link, which that payment used up).
func paidOnOwnLink(r ref) bool {
	return (r.state == "paid" || r.state == "refunded") && r.objectID != "" &&
		(r.paidObjectID == r.objectID || r.paidLinkID == r.objectID)
}

// linkFinished says whether nothing of the ref's link can be paid any more, as far as the server
// knows: it was confirmed expired, or it was paid on that very link.
func linkFinished(r ref) bool {
	return r.state == "expired" || r.linkExpiredAt != "" || paidOnOwnLink(r)
}

// earlierLink reads the record's earlier links at the provider, for an entry that has no ref of its
// own. It says reuse with the link a payer may still use, which is the entry's answer; or the ids of the
// earlier refs it found unable to take payment (checked), over which a new ref may be made; beginRef
// decides under the write lock about the refs that have no link.
func (s *Service) earlierLink(ctx context.Context, provider PaymentProvider, creds Credentials, client *Client, rec record,
	req paylink.Request, minor int64, cur string, who whose) (link paylink.Link, reuse bool, checked []int64, err error) {
	refs, err := recordRefs(ctx, s.store.db, req)
	if err != nil {
		return paylink.Link{}, false, nil, err
	}
	var open *ref
	for _, prior := range refs {
		if prior.objectID == "" || linkFinished(prior) {
			continue
		}
		// It may still be paid. One made under another integration or account can't be read with this
		// key, and a ref paid on another key's object still has its own link on record, which may be
		// open: the entry stops with the code that says which, for an admin.
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
				// Paid on another key's object, and its own link still takes payment: a person reconciles it.
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
			}
			if open != nil {
				// Two links of the record a payer may use (left by the writer before refs were per record):
				// neither is given, and nothing is made, until one has expired.
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
			}
			open = &prior
		case LinkExpired:
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
				// Paid, or unable to take payment for a reason that doesn't say it wasn't, and the ref doesn't
				// know of a payment: a person reconciles it before anything else is made for the record.
				return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkAlreadyPaid}
			}
			checked = append(checked, prior.id) // paid, and complete: nothing of it can be paid again
		}
	}
	if open == nil {
		return paylink.Link{}, false, checked, nil
	}
	if open.amountMinor != minor || open.currency != cur || !sameMoneyFields(*open, req) {
		return paylink.Link{}, false, nil, &paylink.Error{Code: apierr.CodeIntegrationLinkStillOpen}
	}
	// Given again by an entry of the live release: its payment is written through the slot that
	// release declares, which is bound to the same integration (checked above). Only forward: an older
	// entry (a release behind, resuming late) never moves it back. A payment that arrives before any
	// entry gives the link again finds the slot of the live release bound to the ref's integration
	// (apps.WriteEvent), so a rename alone loses nothing.
	if _, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET slot = ?, slot_effect_id = ?
		WHERE id = ? AND coalesce(slot_effect_id, effect_id, 0) < ?`, req.Slot, req.EffectID, open.id, req.EffectID); err != nil {
		return paylink.Link{}, false, nil, err
	}
	slog.Info("integrations: a record's payment link that may still be paid is given again", "integration", open.integrationID, "ref", open.id)
	return open.link(), true, nil, nil
}

// takeEarlier decides, in beginRef's transaction (under the write lock), what an entry with no ref of
// its own does with the record's earlier refs: it takes the one still being made (took, and the ref is
// now the entry's), or a new ref is to be made (not took). checked are the earlier refs with a link
// that earlierLink found unable to take payment.
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
			return ref{}, false, errRecheck // a link earlierLink didn't read
		case paidWithoutLink(prior), prior.state == "expired":
			continue // paid before it learned its link, or retired: nothing of it can be paid
		case prior.effectID > req.EffectID:
			// A newer entry has the record's link in hand: this one is replaced by it (the apps find so when
			// it runs again), and takes nothing meanwhile.
			return ref{}, false, &paylink.Error{Code: apierr.CodeIntegrationLinkLookupPending, Wait: lookupWaitMin}
		}
		var ran bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_ref_attempts
			WHERE ref_id = ? AND (sends > clean_sends OR object_id IS NOT NULL OR outcome IS NOT NULL))`, prior.id).Scan(&ran); err != nil {
			return ref{}, false, err
		}
		if !ran && prior.state == "creating" {
			// Nothing ever ran under it, so nothing of it is at the provider: it goes, and with it any call
			// another try was about to write ahead under it.
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
			// Every call under it ran, and each key is confirmed to have left nothing a payer can use (its
			// object seen expired, or an admin's confirmation that it made none), though the try that learned
			// it stopped before it moved on. It holds nothing, and asks the money it was made for: a new ref
			// is made for what the entry asks now. The ref stays, with its keys, retired (expired): its own
			// entry, or a try of it still running, takes no key for it again (currentAttempt, nextAttempt,
			// writeAhead).
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
		// Two refs of the record that may each still come to have a link: taking one would leave the
		// other, and its link, beside the one this entry makes. A person decides.
		slog.Warn("integrations: a record has more than one payment link still being made; an admin decides", "app", req.AppID)
		return ref{}, false, &paylink.Error{Code: apierr.CodeIntegrationOutcomeUnknown}
	}
	prior := live[0]
	// Still being made, or its outcome unknown: the entry takes it and resolves it as its own, under the
	// slot the entry's release declares (beginRef refuses it if that slot is bound to another integration).
	if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET effect_id = ?, slot = ?, slot_effect_id = NULL WHERE id = ?`, req.EffectID, req.Slot, prior.id); err != nil {
		return ref{}, false, err
	}
	slog.Info("integrations: a record's payment link still being made is taken by its newer entry", "integration", prior.integrationID, "ref", prior.id)
	out, err := scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE id = ?`, prior.id))
	return out, err == nil, err
}

// deadKey holds of a key (integration_ref_attempts o) that can leave nothing a payer can use: its object
// was seen expired, an admin confirmed it made none (and nothing seen contradicts it), or no call under
// it ever ran. Every term is true or false, never NULL (a key with no outcome yet has outcome NULL), so
// NOT deadKey holds of every other key.
const deadKey = `(coalesce(o.outcome, '') = 'expired' OR (coalesce(o.outcome, '') = 'abandoned' AND o.object_id IS NULL)
	OR (o.outcome IS NULL AND o.sends = o.clean_sends AND o.object_id IS NULL))`

// payableRef holds of a ref (integration_refs r) that may still take a payment, or still come to have
// a link: one with a link that isn't confirmed expired and wasn't paid on that very link, or one with
// none yet that has a key that isn't dead (being made, or its outcome unknown).
const payableRef = `(CASE WHEN r.provider_object_id IS NOT NULL
		THEN r.state != 'expired' AND r.link_expired_at IS NULL
			AND NOT (r.state IN ('paid', 'refunded') AND (coalesce(r.paid_object_id, '') = r.provider_object_id
				OR coalesce(r.paid_link_id, '') = r.provider_object_id))
		ELSE EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = r.id AND NOT ` + deadKey + `)
	END)`

// PayableMoney says which records of the app have a ref that may still be paid (payableRef), and for
// each the record's fields the money of each such ref was read from (paylink.Opener). recordID 0 is
// every record of the app.
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

// MayBePaid says whether a ref of the app's record may still take a payment, or still come to have a
// link, and the record's fields the money of each such ref was read from.
func (s *Service) MayBePaid(ctx context.Context, q paylink.Querier, appID, recordID int64) (bool, []paylink.MoneyFields, error) {
	m, err := s.PayableMoney(ctx, q, appID, recordID)
	if err != nil {
		return false, nil, err
	}
	f, ok := m[recordID]
	return ok, f, nil
}

// noteLinkExpired keeps that the ref's own link was seen expired, whatever its state: a ref paid on
// another key's object stays paid, as it was settled.
func (s *Service) noteLinkExpired(ctx context.Context, refID int64, objectID, at string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET link_expired_at = coalesce(link_expired_at, ?)
		WHERE id = ? AND provider_object_id = ?`, at, refID, objectID)
	return err
}

// sameMoneyFields says whether the request reads its money from the fields the ref's was read from:
// only then is the ref's link the request's, since the record's money is locked by the ref's fields
// while the link may be paid. A ref made before refs kept them says nothing, and is taken as the same
// (its money locks every field any release of the app read money from).
func sameMoneyFields(r ref, req paylink.Request) bool {
	if r.amountField == "" && r.currencyField == "" {
		return true
	}
	return r.amountField == req.AmountField && r.currencyField == req.CurrencyField
}
