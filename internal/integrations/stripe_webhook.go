package integrations

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What Stripe's documentation says of webhooks, as read on the day the adapter was written and
// pinned here so a change is a change of code and of this comment (docs/integrations.md):
//
//   - Header and scheme (docs.stripe.com/webhooks, "Verify webhook signatures manually"). The
//     Stripe-Signature header is comma-separated entries, "name=value": t, the Unix seconds at
//     which Stripe signed this delivery; one v1 per active signing secret (a rolled secret that
//     is still valid makes a second v1); v0, a fake signature of sandbox events, "not valid for
//     signature verification"; and optional debug entries (secret, destination, payload) that
//     "aren't covered by the HMAC" and must never decide anything. A verifier must accept entries
//     it doesn't know, use only t and v1 ("to prevent downgrade attacks"), and not depend on the
//     number or the order of entries.
//   - The signed payload is the timestamp as text, a ".", and the unmodified request body; the
//     signature is the hex HMAC-SHA256 of it, keyed with the endpoint's signing secret as text
//     (the whsec_ prefix included). Compare in constant time against every v1; a match makes the
//     delivery authentic.
//   - Tolerance: Stripe's libraries refuse a timestamp more than five minutes from now by
//     default, and "a tolerance value of 0" disables the check, which is why it is a constant
//     here. A retried event is signed afresh, with a new timestamp.
//   - Delivery: a non-2xx answer is a failed delivery and is retried for up to three days in
//     live mode (a few hours in a sandbox); a 3xx is a failure. Events may repeat and "don't
//     arrive in order": Stripe says to track event ids to find duplicates, and to answer 2xx
//     before doing any work.
//   - Rolling a signing secret can keep the previous one active for up to seven days. The store
//     keeps a replaced secret for a shorter overlap (previousOverlap): after it, only the new one
//     verifies, which is the safe side.
//
// Event shapes (docs.stripe.com/api/events/types, snapshot events, the format of an endpoint
// registered with event_payload=snapshot):
//
//   - checkout.session.completed: "a Checkout Session has been successfully completed". Its
//     payment_status says whether the money arrived: "paid", or "unpaid" while a delayed
//     payment method (a bank debit) settles.
//   - checkout.session.async_payment_succeeded: "a payment intent using a delayed payment method
//     finally succeeds"; checkout.session.async_payment_failed: it fails.
//   - checkout.session.expired: "a Checkout Session is expired". Our links are Checkout Sessions
//     with a client_reference_id, the one reference Stripe echoes back to us.
//   - charge.refunded: "a charge is refunded, including partial refunds"; its amount_refunded is
//     the total refunded so far.
//   - payment_intent.payment_failed is not terminal for a session: the payer may try another
//     card on the same page, and the session stays open. It is ignored.
//
// The one authoritative type of each normalized event is in stripeKinds below.

// StripeSignatureHeader is the header Stripe signs a delivery in.
const StripeSignatureHeader = "Stripe-Signature"

// StripeTolerance is how far a signature's timestamp may be from the server's clock, in either
// direction. It is Stripe's own libraries' default.
const StripeTolerance = 5 * time.Minute

// Bounds of the header: Stripe's has about six entries and 400 bytes.
const (
	maxSignatureHeader  = 2048
	maxSignatureEntries = 32
)

// Normalized kinds of payment events (the kind "payments").
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentExpired   = "payment.expired"
	EventPaymentFailed    = "payment.failed"
	EventPaymentRefunded  = "payment.refunded"
)

// WebhookSecrets are the secrets a delivery may be signed with: the current one and, for the
// overlap of a rotation, the one it replaced. Empty ones are not used.
type WebhookSecrets struct {
	Current  Secret
	Previous Secret
}

// Event is a provider event reduced to closed, typed fields: what the server keeps of a webhook
// (the raw body never is) and what the worker applies. Every string in it is an identifier
// checked against the provider's shape, never text a payer or a merchant wrote, so nothing a
// customer typed at the provider can reach a record through it.
type Event struct {
	// ID is the provider's id of the event, the key of a replay.
	ID string
	// Kind is the normalized event (EventPaymentSucceeded...).
	Kind string
	// RefToken is the server-issued token the provider echoed back, "" when the event carries none.
	RefToken string
	// ObjectID is the provider's id of what the event is about: a Checkout Session, or a charge.
	ObjectID string
	// PaymentID is the provider's id of the payment the object belongs to (a payment intent),
	// "" when there is none yet.
	PaymentID string
	// AmountMinor is in the currency's smallest unit; for a refund, what has been refunded so far.
	AmountMinor int64
	// Currency is the ISO 4217 code, upper case.
	Currency   string
	OccurredAt time.Time
	// Livemode says whether the event is of live money.
	Livemode bool
}

// Receiver is a provider that receives webhooks. A provider that doesn't implement it has no
// webhook route: its address answers like an unknown one.
type Receiver interface {
	Provider
	// VerifyWebhook checks that the delivery was signed with one of the secrets, over the exact
	// body and in constant time, and that its timestamp is within the provider's tolerance of
	// now. It reads nothing of the body but the bytes, so it runs before any parsing. Its errors
	// say nothing a caller may show: the route answers every one the same.
	VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error
	// Normalize reads a verified body. It reports false for an event the integration doesn't act
	// on, or whose shape isn't the one it relies on: that is answered 200 and not stored, since
	// a provider disables an endpoint that answers errors.
	Normalize(body []byte) (Event, bool)
}

var (
	errNoSecret        = errors.New("no webhook secret")
	errBadSignature    = errors.New("signature doesn't match")
	errStaleTimestamp  = errors.New("timestamp outside the tolerance")
	errMalformedHeader = errors.New("malformed signature header")
)

// verifyKeys is how many keys a delivery is verified against: the current secret and the previous
// one. Every well-formed delivery is checked against exactly this many, a slot with no secret
// holding verifyPad, so the number of MACs computed over the body is the same whether an
// integration is in the overlap of a rotation, has one secret, or has none.
const verifyKeys = 2

// verifyPad is the key of a slot with no secret: random, made when the server starts, known to
// nobody, so nothing signs for it.
var verifyPad = func() []byte {
	pad := make([]byte, 32)
	if _, err := rand.Read(pad); err != nil {
		panic("integrations: no randomness for the webhook verifier: " + err.Error())
	}
	return pad
}()

// macHook, when set (by a test), is called for each MAC computed over a delivery's body.
var macHook func()

// VerifyWebhook checks a Stripe delivery (see the notes above). A header that is well formed
// costs exactly verifyKeys MACs over the body, whatever secrets there are, and the comparison
// covers every v1 entry against every key without stopping at the first match, so the time it
// takes says nothing about which secret matched or how many there are.
func (stripe) VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error {
	keys := [verifyKeys][]byte{verifyPad, verifyPad}
	real := 0
	for _, s := range []Secret{secrets.Current, secrets.Previous} {
		if s != "" {
			keys[real] = []byte(s.Reveal())
			real++
		}
	}
	values := h.Values(StripeSignatureHeader)
	if len(values) != 1 || len(values[0]) > maxSignatureHeader {
		return errMalformedHeader
	}
	stamp, sigs, ok := parseStripeSignature(values[0])
	if !ok {
		return errMalformedHeader
	}
	match := 0
	for _, key := range keys {
		want := stripeMAC(key, stamp, body)
		for _, got := range sigs {
			if hmac.Equal(want, got) {
				match = 1 // no early exit
			}
		}
	}
	if real == 0 {
		return errNoSecret
	}
	if match == 0 {
		return errBadSignature
	}
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return errMalformedHeader
	}
	if age := now.Sub(time.Unix(secs, 0)); age > StripeTolerance || age < -StripeTolerance {
		return errStaleTimestamp
	}
	return nil
}

// parseStripeSignature reads the entries of a Stripe-Signature value: the one t and the v1
// signatures, decoded. Unknown entries, v0 and the debug entries are skipped. A header with no t,
// two of them, or no v1 is malformed.
func parseStripeSignature(header string) (stamp string, sigs [][]byte, ok bool) {
	entries := strings.Split(header, ",")
	if len(entries) > maxSignatureEntries {
		return "", nil, false
	}
	for _, e := range entries {
		name, value, found := strings.Cut(e, "=")
		if !found {
			continue
		}
		switch name {
		case "t":
			if stamp != "" || !digitsRe.MatchString(value) {
				return "", nil, false
			}
			stamp = value
		case "v1":
			// A v1 that isn't a SHA-256 in hex can't match: left out rather than refused, so a
			// future entry of another length doesn't break verification.
			if raw, err := hex.DecodeString(value); err == nil && len(raw) == sha256.Size {
				sigs = append(sigs, raw)
			}
		}
	}
	return stamp, sigs, stamp != "" && len(sigs) > 0
}

var digitsRe = regexp.MustCompile(`^[0-9]{1,15}$`)

func stripeMAC(key []byte, stamp string, body []byte) []byte {
	if macHook != nil {
		macHook()
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(stamp))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// stripeKinds is the one Stripe event type behind each normalized event. A payment makes several
// events at Stripe (the intent's and the charge's among them); naming one each means a payment
// is told once, and the ref's state change (a conditional update) settles the rest.
//
//	payment.succeeded: checkout.session.completed with payment_status "paid", and
//	   checkout.session.async_payment_succeeded: of one session exactly one of them says "paid"
//	   (a delayed payment's first event says "unpaid" and is ignored).
//	payment.expired:   checkout.session.expired.
//	payment.failed:    checkout.session.async_payment_failed: a delayed payment that failed. A
//	   card that is declined leaves the session open and is not a failure of the payment.
//	payment.refunded:  charge.refunded.
var stripeKinds = map[string]string{
	"checkout.session.completed":               EventPaymentSucceeded,
	"checkout.session.async_payment_succeeded": EventPaymentSucceeded,
	"checkout.session.async_payment_failed":    EventPaymentFailed,
	"checkout.session.expired":                 EventPaymentExpired,
	"charge.refunded":                          EventPaymentRefunded,
}

// The shapes of the identifiers Normalize accepts. Anything else is not an event it acts on.
var (
	stripeEventIDRe   = regexp.MustCompile(`^evt_[A-Za-z0-9_]{1,128}$`)
	stripeSessionIDRe = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9_]{1,200}$`)
	stripeChargeIDRe  = regexp.MustCompile(`^(ch|py)_[A-Za-z0-9_]{1,128}$`)
	stripeIntentIDRe  = regexp.MustCompile(`^pi_[A-Za-z0-9_]{1,128}$`)
	currencyCodeRe    = regexp.MustCompile(`^[a-z]{3}$`)
	// refTokenRe is a ref's token: 128 random bits in hex.
	refTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type stripeEvent struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// stripeObject holds the fields of a Checkout Session and of a charge that Normalize reads: a
// session's total is amount_total and a charge's refunded sum is amount_refunded. Everything
// else in the object (the payer's name and email, the description, the metadata) is never
// decoded.
type stripeObject struct {
	ID                string  `json:"id"`
	Object            string  `json:"object"`
	Mode              string  `json:"mode"`
	PaymentStatus     string  `json:"payment_status"`
	ClientReferenceID *string `json:"client_reference_id"`
	PaymentIntent     *string `json:"payment_intent"`
	AmountTotal       int64   `json:"amount_total"`
	AmountRefunded    int64   `json:"amount_refunded"`
	Currency          string  `json:"currency"`
}

// Normalize reads a verified Stripe event body (see stripeKinds). It reports false for a type it
// doesn't act on and for an event whose object isn't of the shape it relies on, such as a session
// that isn't ours (no client_reference_id of the shape of a ref's token), which is another
// integration's or another use of the same Stripe account.
func (stripe) Normalize(body []byte) (Event, bool) {
	var ev stripeEvent
	if json.Unmarshal(body, &ev) != nil || ev.Object != "event" || !stripeEventIDRe.MatchString(ev.ID) || ev.Created <= 0 {
		return Event{}, false
	}
	kind, ok := stripeKinds[ev.Type]
	if !ok {
		return Event{}, false
	}
	var obj stripeObject
	if json.Unmarshal(ev.Data.Object, &obj) != nil || !currencyCodeRe.MatchString(obj.Currency) {
		return Event{}, false
	}
	out := Event{
		ID: ev.ID, Kind: kind, Currency: strings.ToUpper(obj.Currency),
		OccurredAt: time.Unix(ev.Created, 0).UTC(), Livemode: ev.Livemode,
	}
	if ev.Type == "charge.refunded" {
		if obj.Object != "charge" || !stripeChargeIDRe.MatchString(obj.ID) || obj.AmountRefunded <= 0 {
			return Event{}, false
		}
		// A refund is mapped to its payment through the payment intent, the one id the charge
		// shares with the session's events: a charge with none (not made by a Checkout Session)
		// can't be mapped to anything of ours, and isn't stored.
		if obj.PaymentIntent == nil || !stripeIntentIDRe.MatchString(*obj.PaymentIntent) {
			return Event{}, false
		}
		out.ObjectID, out.AmountMinor, out.PaymentID = obj.ID, obj.AmountRefunded, *obj.PaymentIntent
		return out, true
	}
	// A Checkout Session of one-time payments, made by us: it carries our token.
	if obj.Object != "checkout.session" || obj.Mode != "payment" || !stripeSessionIDRe.MatchString(obj.ID) ||
		obj.ClientReferenceID == nil || !refTokenRe.MatchString(*obj.ClientReferenceID) || obj.AmountTotal <= 0 {
		return Event{}, false
	}
	if kind == EventPaymentSucceeded && obj.PaymentStatus != "paid" {
		return Event{}, false // a delayed payment that hasn't settled: its own event comes later
	}
	out.RefToken, out.ObjectID, out.AmountMinor = *obj.ClientReferenceID, obj.ID, obj.AmountTotal
	if obj.PaymentIntent != nil {
		if !stripeIntentIDRe.MatchString(*obj.PaymentIntent) {
			return Event{}, false
		}
		out.PaymentID = *obj.PaymentIntent
	}
	return out, true
}
