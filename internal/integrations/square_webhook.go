package integrations

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Square's webhooks: the signature over the notification URL and the body, and the events of a payment
// (see the notes at the top of square.go).

var errNoURL = errors.New("no webhook address to verify against")

// VerifyWebhook checks a Square delivery. The one signature in the header is compared, in constant
// time, with the HMAC of each configured secret over the webhook address that secret was saved for
// (the current secret over URL, the previous one over PreviousURL) and the exact body: verifyKeys MACs over the body for every well-formed delivery, as Stripe's verifier computes,
// whatever secrets there are. Only once it matched is the body read, for its one signed time, the
// event's created_at, which must be within squareRetryWindow (and squareSkew) of now.
func (square) VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error {
	keys := [verifyKeys][]byte{verifyPad, verifyPad}
	urls := [verifyKeys]string{secrets.URL, secrets.URL}
	real := 0
	for i, s := range []Secret{secrets.Current, secrets.Previous} {
		if s != "" {
			keys[real] = []byte(s.Reveal())
			urls[real] = secrets.URL
			if i == 1 && secrets.PreviousURL != "" {
				urls[real] = secrets.PreviousURL
			}
			real++
		}
	}
	values := h.Values(SquareSignatureHeader)
	if len(values) != 1 || len(values[0]) > maxSignatureHeader {
		return errMalformedHeader
	}
	got, err := base64.StdEncoding.DecodeString(values[0])
	if err != nil || len(got) != sha256.Size {
		return errMalformedHeader
	}
	match := 0
	for slot, key := range keys {
		if hmac.Equal(squareMAC(key, urls[slot], body), got) && slot < real {
			match = 1 // no early exit
		}
	}
	switch {
	case real == 0:
		return errNoSecret
	case secrets.URL == "":
		return errNoURL
	case match == 0 || len(body) == 0:
		return errBadSignature
	}
	var ev struct {
		CreatedAt string `json:"created_at"`
	}
	if json.Unmarshal(body, &ev) != nil {
		return errStaleTimestamp
	}
	at, err := time.Parse(time.RFC3339Nano, ev.CreatedAt)
	if err != nil {
		return errStaleTimestamp
	}
	if age := now.Sub(at); age > squareRetryWindow+squareSkew || age < -squareSkew {
		return errStaleTimestamp
	}
	return nil
}

func squareMAC(key []byte, notificationURL string, body []byte) []byte {
	if macHook != nil {
		macHook()
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(notificationURL))
	m.Write(body)
	return m.Sum(nil)
}

// squareEvent is the envelope of a Square event and the fields of a Payment that Normalize reads.
// Everything else (the buyer's email, address and card, the receipt, the note's other uses) is never
// decoded.
type squareEvent struct {
	MerchantID string `json:"merchant_id"`
	Type       string `json:"type"`
	EventID    string `json:"event_id"`
	CreatedAt  string `json:"created_at"`
	Data       struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Object struct {
			Payment *struct {
				ID            string       `json:"id"`
				Status        string       `json:"status"`
				TotalMoney    *squareMoney `json:"total_money"`
				RefundedMoney *squareMoney `json:"refunded_money"`
				Note          *string      `json:"note"`
				OrderID       string       `json:"order_id"`
			} `json:"payment"`
		} `json:"object"`
	} `json:"data"`
}

// Normalize reads a verified Square event. Square has no event per outcome, only the payment's
// creation and its updates, so the payment's own fields decide:
//
//	payment.succeeded: a payment.created or payment.updated whose payment is COMPLETED, has refunded
//	   nothing and carries a ref's token as its note. Square may tell one payment more than once (its
//	   creation and an update, or an update of something else); the ref's state change applies it once.
//	payment.refunded:  a payment.updated of a COMPLETED payment with refunded_money above zero: what has
//	   been refunded so far, mapped to the ref through the payment's id.
//
// APPROVED and PENDING payments are not yet paid, and a FAILED or CANCELED one leaves the link open
// for the payer to try again, so none of them is an event. A Square link never expires. A payment
// without a token of ours, refund.* events (one refund's own amount, not the total) and every other
// type are not events.
func (square) Normalize(body []byte) (Event, bool) {
	var ev squareEvent
	if json.Unmarshal(body, &ev) != nil || !squareEventIDRe.MatchString(ev.EventID) || !squareIDRe.MatchString(ev.MerchantID) {
		return Event{}, false
	}
	if ev.Type != "payment.created" && ev.Type != "payment.updated" {
		return Event{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, ev.CreatedAt)
	if err != nil {
		return Event{}, false
	}
	p := ev.Data.Object.Payment
	if ev.Data.Type != "payment" || p == nil || p.ID != ev.Data.ID || !squareIDRe.MatchString(p.ID) || p.Status != "COMPLETED" ||
		p.TotalMoney == nil || p.TotalMoney.Amount <= 0 || !squareCurrencyRe.MatchString(p.TotalMoney.Currency) {
		return Event{}, false
	}
	out := Event{ID: ev.EventID, ObjectID: p.ID, PaymentID: p.ID, OccurredAt: at.UTC()}
	if p.RefundedMoney != nil && p.RefundedMoney.Amount > 0 {
		if ev.Type != "payment.updated" || !squareCurrencyRe.MatchString(p.RefundedMoney.Currency) {
			return Event{}, false
		}
		out.Kind, out.AmountMinor, out.Currency = EventPaymentRefunded, p.RefundedMoney.Amount, p.RefundedMoney.Currency
		return out, true
	}
	if p.Note == nil || !refTokenRe.MatchString(*p.Note) {
		return Event{}, false // a payment that no link of ours made
	}
	out.Kind, out.RefToken, out.AmountMinor, out.Currency = EventPaymentSucceeded, *p.Note, p.TotalMoney.Amount, p.TotalMoney.Currency
	return out, true
}
