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

const StripeSignatureHeader = "Stripe-Signature"

const StripeTolerance = 5 * time.Minute

const (
	maxSignatureHeader  = 2048
	maxSignatureEntries = 32
)

const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentExpired   = "payment.expired"
	EventPaymentFailed    = "payment.failed"
	EventPaymentRefunded  = "payment.refunded"
)

type WebhookSecrets struct {
	Current  Secret
	Previous Secret
}

type Event struct {
	ID          string
	Kind        string
	RefToken    string
	ObjectID    string
	PaymentID   string
	AmountMinor int64
	Currency    string
	OccurredAt  time.Time
	Livemode    bool
}

type Receiver interface {
	Provider
	VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error
	Normalize(body []byte) (Event, bool)
}

var (
	errNoSecret        = errors.New("no webhook secret")
	errBadSignature    = errors.New("signature doesn't match")
	errStaleTimestamp  = errors.New("timestamp outside the tolerance")
	errMalformedHeader = errors.New("malformed signature header")
)

const verifyKeys = 2

var verifyPad = func() []byte {
	pad := make([]byte, 32)
	if _, err := rand.Read(pad); err != nil {
		panic("integrations: no randomness for the webhook verifier: " + err.Error())
	}
	return pad
}()

var macHook func()

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
	for slot, key := range keys {
		want := stripeMAC(key, stamp, body)
		for _, got := range sigs {
			if hmac.Equal(want, got) && slot < real {
				match = 1
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

var stripeKinds = map[string]string{
	"checkout.session.completed":               EventPaymentSucceeded,
	"checkout.session.async_payment_succeeded": EventPaymentSucceeded,
	"checkout.session.async_payment_failed":    EventPaymentFailed,
	"checkout.session.expired":                 EventPaymentExpired,
	"charge.refunded":                          EventPaymentRefunded,
}

var (
	stripeEventIDRe   = regexp.MustCompile(`^evt_[A-Za-z0-9_]{1,128}$`)
	stripeSessionIDRe = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9_]{1,200}$`)
	stripeChargeIDRe  = regexp.MustCompile(`^(ch|py)_[A-Za-z0-9_]{1,128}$`)
	stripeIntentIDRe  = regexp.MustCompile(`^pi_[A-Za-z0-9_]{1,128}$`)
	currencyCodeRe    = regexp.MustCompile(`^[a-z]{3}$`)
	refTokenRe        = regexp.MustCompile(`^[0-9a-f]{32}$`)
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
		if obj.PaymentIntent == nil || !stripeIntentIDRe.MatchString(*obj.PaymentIntent) {
			return Event{}, false
		}
		out.ObjectID, out.AmountMinor, out.PaymentID = obj.ID, obj.AmountRefunded, *obj.PaymentIntent
		return out, true
	}
	if obj.Object != "checkout.session" || obj.Mode != "payment" || !stripeSessionIDRe.MatchString(obj.ID) ||
		obj.ClientReferenceID == nil || !refTokenRe.MatchString(*obj.ClientReferenceID) || obj.AmountTotal <= 0 {
		return Event{}, false
	}
	if kind == EventPaymentSucceeded && obj.PaymentStatus != "paid" {
		return Event{}, false
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
