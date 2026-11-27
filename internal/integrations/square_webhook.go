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

var errNoURL = errors.New("no webhook address to verify against")

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
	match, addressed := 0, 0
	for slot, key := range keys {
		if slot < real && urls[slot] != "" {
			addressed = 1
		}
		if hmac.Equal(squareMAC(key, urls[slot], body), got) && slot < real && urls[slot] != "" {
			match = 1
		}
	}
	switch {
	case real == 0:
		return errNoSecret
	case addressed == 0:
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
		return Event{}, false
	}
	out.Kind, out.RefToken, out.AmountMinor, out.Currency = EventPaymentSucceeded, *p.Note, p.TotalMoney.Amount, p.TotalMoney.Currency
	return out, true
}
