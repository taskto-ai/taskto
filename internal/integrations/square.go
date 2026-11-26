package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The Square adapter: payment links through Square's Checkout API, and its webhooks.
//
// What Square's documentation says, as read on the day the adapter was written and pinned here, so
// a change is a change of code and of this comment (docs/integrations.md#square). The API reference
// is Square's published OpenAPI specification (github.com/square/connect-api-specification, api.json,
// release 2026-11-18), the source of https://developer.squareup.com/reference/square.
//
//   - Version. Every call sends Square-Version: SquareAPIVersion, the specification's current
//     version (https://developer.squareup.com/docs/build-basics/versioning-overview).
//   - Hosts. connect.squareup.com in production and connect.squareupsandbox.com in the sandbox: the
//     environment is a setting with two choices, never a URL, and an integration reaches its own only.
//   - Create a payment link: POST /v2/online-checkout/payment-links
//     (https://developer.squareup.com/reference/square/checkout-api/create-payment-link).
//     idempotency_key has at most 192 characters; order "describes the Order for which to create a
//     checkout link" (location_id required, reference_id "a client-specified ID to associate an entity
//     in another system", at most 40 characters); checkout_options.redirect_url is "the confirmation
//     page URL to redirect the buyer to after Square processes the payment" (at most 2048);
//     allow_tipping; payment_note, at most 500: "After processing the payment, Square adds this note
//     to the resulting Payment as well as the post payment confirmation email". The answer is a
//     payment_link (id, version, order_id, url, long_url, checkout_options, payment_note, created_at).
//     The payment_note is how a payment carries our token back: Payment.note is in the payment's
//     events, where nothing else of the link or the order is.
//   - One payment per link: "The buyer can use the payment link only once"; when paid, the order moves
//     from DRAFT to OPEN and gets a Tender (https://developer.squareup.com/docs/checkout-api/quick-pay-checkout).
//     A link doesn't expire. Deleting one "sets the corresponding order state to CANCELED"
//     (https://developer.squareup.com/docs/checkout-api/manage-checkout).
//   - List and retrieve: GET /v2/online-checkout/payment-links (cursor, and a limit that "is
//     advisory", at most 1000), GET /v2/online-checkout/payment-links/{id}, GET /v2/orders/{order_id},
//     and GET /v2/merchants/me, the merchant "currently accessible to this call".
//   - Idempotency (https://developer.squareup.com/docs/build-basics/common-api-patterns/idempotency): a
//     repeated call with the same key and the same request "returns the response as the first
//     successful" call, and the same key with other details is an error ("you used the idempotency key
//     previously", the error code IDEMPOTENCY_KEY_REUSED). Square publishes no period for which a key
//     is kept: see squareKeyKept.
//   - Errors (https://developer.squareup.com/docs/build-basics/handling-errors): a body with an errors
//     array of {category, code, detail, field}; 401 and 403 for AUTHENTICATION_ERROR (UNAUTHORIZED,
//     ACCESS_TOKEN_EXPIRED, ACCESS_TOKEN_REVOKED, FORBIDDEN, INSUFFICIENT_SCOPES), 429 for RATE_LIMITED.
//     Square documents no answer as given before the request ran, not even 429 ("Square might
//     temporarily stop processing your requests"), so every answer that isn't a success leaves the
//     outcome of a call that makes an object unknown (squareFailure); only a request that never left
//     is NotExecuted.
//   - Webhooks (https://developer.squareup.com/docs/webhooks/step3validate): the
//     x-square-hmacsha256-signature header is the Base64 HMAC-SHA256, keyed with the subscription's
//     signature key, of the notification URL followed by the raw body; Square's own SDKs compare it in
//     constant time (square-go-sdk, webhooks/client/verify_signature.go). No header carries a signed
//     time: the event's created_at, inside the signed body, is the only one. Square retries a failed
//     delivery "for up to 24 hours after the originating event", the same event_id each time, in no
//     guaranteed order (https://developer.squareup.com/docs/webhooks/overview), so a delivery is
//     accepted for that long after created_at (squareRetryWindow), and a replay inside it is stopped by
//     the event's id, which is stored once, and by the ref's state, which moves only forward.
//   - Events: payment.created and payment.updated carry data.object.payment, a Payment
//     (status APPROVED, PENDING, COMPLETED, CANCELED or FAILED; amount_money, total_money,
//     refunded_money "the total amount of the payment refunded to date", order_id, note). refund.created
//     and refund.updated carry one refund's own amount, not the total, and are not read.

// SquareAPIVersion is the version of Square's API the adapter is written against, sent in the
// Square-Version header. Upgrading it is a change of code, tested against the fake.
const SquareAPIVersion = "2026-11-18"

// SquareSignatureHeader is the header Square signs a delivery in.
const SquareSignatureHeader = "X-Square-Hmacsha256-Signature"

const (
	// squareRetryWindow is how long after an event Square still delivers it (its last retry is 24
	// hours after the event), and squareSkew how far the clocks may differ. A delivery is verified
	// when its event was created at most squareRetryWindow+squareSkew ago and at most squareSkew ahead.
	squareRetryWindow = 24 * time.Hour
	squareSkew        = 5 * time.Minute
	// squareKeyKept is how long a key is relied on to be kept. Square publishes no retention period for
	// idempotency keys, so a call is asked again under its key only shortly after it was first sent,
	// while the reconciliation's first tries run; after that a key whose outcome is unknown is only
	// looked for by its token, which the list of payment links always shows.
	squareKeyKept = time.Hour
	// squareFindPageSize is the page asked of a list (Square's maximum; it is advisory), and
	// squareFindMaxPages how many pages a lookup reads before it says it couldn't read them all.
	squareFindPageSize = 1000
	squareFindMaxPages = 20
	// squareMaxName is the longest line item name Square takes.
	squareMaxName   = 255
	squareUserAgent = "taskto"
)

var (
	// squareIDRe is what Square's ids of links, orders, payments and merchants look like.
	squareIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,192}$`)
	// squareEventIDRe is the shape of an event_id (a UUID).
	squareEventIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// squareCurrencyRe is a currency code as Square writes it.
	squareCurrencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// squareHost is the API host of the integration's environment.
func squareHost(settings map[string]string) string { return square{}.Hosts(settings)[0] }

// squareLinkHost is where a payment link of the environment must point: Square's short links,
// sandbox.square.link in the sandbox (the documented example) and square.link in production.
func squareLinkHost(settings map[string]string) string {
	if settings["environment"] == "production" {
		return "square.link"
	}
	return "sandbox.square.link"
}

func (square) APIVersion() string { return SquareAPIVersion }

// KeyKept: see squareKeyKept.
func (square) KeyKept() time.Duration { return squareKeyKept }

// EventsCarryNoMode: Square's events don't say whether they are of the sandbox, and each environment
// has its own webhook subscriptions and signature keys (modeOfAccount).
func (square) EventsCarryNoMode() bool { return true }

type squareMoney struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type squareLink struct {
	ID              string `json:"id"`
	OrderID         string `json:"order_id"`
	URL             string `json:"url"`
	PaymentNote     string `json:"payment_note"`
	CheckoutOptions *struct {
		RedirectURL string `json:"redirect_url"`
	} `json:"checkout_options"`
}

type squareOrder struct {
	ID          string            `json:"id"`
	LocationID  string            `json:"location_id"`
	ReferenceID string            `json:"reference_id"`
	State       string            `json:"state"`
	TotalMoney  *squareMoney      `json:"total_money"`
	Tenders     []json.RawMessage `json:"tenders"`
}

// CreatePaymentLink makes a payment link for one order of one line of the exact amount, at the
// integration's location. The key's token is the idempotency key, the order's reference_id and the
// payment_note, which Square copies to the payment and so to its events. With the server's own pages
// the payer is sent back to the done page (Square has no cancel page: a payer who leaves just
// doesn't pay); without them the payer stays on Square's own confirmation. Tipping is off, so what is
// paid is what was asked.
func (square) CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error) {
	name := strings.TrimSpace(l.Description)
	if name == "" {
		name = fallbackProductName
	}
	if utf8.RuneCountInString(name) > squareMaxName {
		name = string([]rune(name)[:squareMaxName])
	}
	type lineItem struct {
		Name           string      `json:"name"`
		Quantity       string      `json:"quantity"`
		BasePriceMoney squareMoney `json:"base_price_money"`
	}
	type options struct {
		AllowTipping bool   `json:"allow_tipping"`
		RedirectURL  string `json:"redirect_url,omitempty"`
	}
	body := struct {
		IdempotencyKey string `json:"idempotency_key"`
		Order          struct {
			LocationID  string     `json:"location_id"`
			ReferenceID string     `json:"reference_id"`
			LineItems   []lineItem `json:"line_items"`
		} `json:"order"`
		CheckoutOptions options `json:"checkout_options"`
		PaymentNote     string  `json:"payment_note"`
	}{IdempotencyKey: l.IdempotencyKey(), PaymentNote: l.Token, CheckoutOptions: options{RedirectURL: l.SuccessURL}}
	body.Order.LocationID = creds.Settings["location_id"]
	body.Order.ReferenceID = l.Token
	body.Order.LineItems = []lineItem{{Name: name, Quantity: "1", BasePriceMoney: squareMoney{Amount: l.AmountMinor, Currency: l.Currency}}}
	raw, err := json.Marshal(body)
	if err != nil {
		return Link{}, &CallError{Kind: CallRefused, NotExecuted: true}
	}

	req, err := c.NewRequest(ctx, http.MethodPost, c.BaseURL(squareHost(creds.Settings))+"/v2/online-checkout/payment-links", bytes.NewReader(raw))
	if err != nil {
		return Link{}, &CallError{Kind: CallRefused, NotExecuted: true}
	}
	squareHeaders(req, creds)
	req.Header.Set("Content-Type", "application/json")

	resp, sent, err := sendOnce(c, req)
	if err != nil {
		if !sent {
			return Link{}, &CallError{Kind: CallUnreachable, NotExecuted: true}
		}
		return Link{}, &CallError{Kind: CallUnreachable, OutcomeUnknown: true}
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Link{}, &CallError{Kind: CallUnreachable, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	if resp.StatusCode != http.StatusOK {
		return Link{}, squareFailure(resp.StatusCode)
	}
	var out struct {
		PaymentLink *squareLink `json:"payment_link"`
	}
	if err := json.Unmarshal(answer, &out); err != nil || out.PaymentLink == nil {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	link, ok := squareLinkOf(creds.Settings, l, *out.PaymentLink)
	if !ok {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	return link, nil
}

// squareLinkOf checks what Square says of a payment link it made, or one a list shows, against what
// the key asked: its token as the note, its redirect (the server's done page, or none), an id and an
// order, and an address on the environment's link host. It returns the link.
func squareLinkOf(settings map[string]string, l NewLink, pl squareLink) (Link, bool) {
	redirect := ""
	if pl.CheckoutOptions != nil {
		redirect = pl.CheckoutOptions.RedirectURL
	}
	if pl.PaymentNote != l.Token || redirect != l.SuccessURL || !squareIDRe.MatchString(pl.ID) || len(pl.ID) > maxLinkID ||
		!squareIDRe.MatchString(pl.OrderID) {
		return Link{}, false
	}
	u, err := url.Parse(pl.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != squareLinkHost(settings) || u.User != nil || u.Port() != "" ||
		len(pl.URL) > maxLinkURL {
		return Link{}, false
	}
	return Link{URL: pl.URL, ObjectID: pl.ID}, true
}

// FindPaymentLink reads the account's payment links, every page up to squareFindMaxPages, for the one
// whose payment_note is the key's token. The list has no filter (and no order Square documents), so
// it is read in full: a lookup that stops with pages left is CallLookupIncomplete, which tells nothing.
// The one found is adopted only if it asks what is asked now (its order's total and currency, and the
// same redirect) and its order is unpaid; a paid one, or one whose order was cancelled (a deleted link),
// is CallSettled, for a person to reconcile. Every failure after the link of the token was seen returns
// its id (and no URL), as the interface asks. A Square link never expires, so expired is always false.
func (square) FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, _ time.Time) (Link, bool, bool, error) {
	q := url.Values{"limit": {strconv.Itoa(squareFindPageSize)}}
	for range squareFindMaxPages {
		body, status, err := squareGet(ctx, creds, c, "/v2/online-checkout/payment-links?"+q.Encode())
		if err != nil {
			return Link{}, false, false, err
		}
		var page struct {
			PaymentLinks *[]squareLink `json:"payment_links"`
			Cursor       string        `json:"cursor"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return Link{}, false, false, &CallError{Kind: CallUnreachable, Status: status}
		}
		// An empty account answers {} (no payment_links at all): that is a whole, empty page only when
		// there is no cursor either.
		links := []squareLink{}
		if page.PaymentLinks != nil {
			links = *page.PaymentLinks
		}
		if page.Cursor != "" && len(links) == 0 {
			return Link{}, false, false, &CallError{Kind: CallUnreachable, Status: status}
		}
		for _, pl := range links {
			if pl.PaymentNote != l.Token {
				continue
			}
			// Ours: whatever fails from here returns the link seen, unchecked, for the key to keep, so
			// that a refusal is never taken for "nothing made" and no second link is made beside it.
			seen := Link{ObjectID: seenObjectID(pl.ID)}
			link, ok := squareLinkOf(creds.Settings, l, pl)
			if !ok {
				// Not a link the payer could be given: nothing says what it is.
				return seen, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			order, err := squareOrderOf(ctx, creds, c, pl.OrderID)
			if err != nil {
				return seen, false, false, err
			}
			if order.TotalMoney == nil || order.TotalMoney.Amount != l.AmountMinor || order.TotalMoney.Currency != l.Currency ||
				order.ReferenceID != l.Token {
				return seen, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			switch squareOrderState(order) {
			case LinkOpen:
				return link, true, false, nil
			case LinkSettled:
				return seen, false, false, &CallError{Kind: CallSettled, Status: status}
			}
			return seen, false, false, &CallError{Kind: CallRefused, Status: status} // a state not said
		}
		if page.Cursor == "" {
			return Link{}, false, false, nil
		}
		q.Set("cursor", page.Cursor)
	}
	return Link{}, false, false, &CallError{Kind: CallLookupIncomplete}
}

// LinkState reads the link and its order: a link whose order is a DRAFT or OPEN with no tender can take
// the payment; one with a tender was paid, and a COMPLETED or CANCELED order (a deleted link) can't
// be paid, for a reason that doesn't say it wasn't. Square's links don't expire. hosted makes no
// difference: both shapes are payment links.
func (square) LinkState(ctx context.Context, creds Credentials, c *Client, _ bool, objectID string) (LinkState, error) {
	if !squareIDRe.MatchString(objectID) {
		return 0, &CallError{Kind: CallRefused}
	}
	body, status, err := squareGet(ctx, creds, c, "/v2/online-checkout/payment-links/"+objectID)
	if err != nil {
		return 0, err
	}
	var out struct {
		PaymentLink *squareLink `json:"payment_link"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.PaymentLink == nil || out.PaymentLink.ID != objectID ||
		!squareIDRe.MatchString(out.PaymentLink.OrderID) {
		return 0, &CallError{Kind: CallUnreachable, Status: status}
	}
	order, err := squareOrderOf(ctx, creds, c, out.PaymentLink.OrderID)
	if err != nil {
		return 0, err
	}
	if st := squareOrderState(order); st != 0 {
		return st, nil
	}
	return 0, &CallError{Kind: CallRefused, Status: status} // a state not said
}

// squareOrderState is what an order of a link says of it: open, settled, or 0 for a state Square
// doesn't document.
func squareOrderState(o squareOrder) LinkState {
	if len(o.Tenders) > 0 {
		return LinkSettled
	}
	switch o.State {
	case "DRAFT", "OPEN":
		return LinkOpen
	case "COMPLETED", "CANCELED":
		return LinkSettled
	}
	return 0
}

// squareOrderOf retrieves an order.
func squareOrderOf(ctx context.Context, creds Credentials, c *Client, id string) (squareOrder, error) {
	if !squareIDRe.MatchString(id) {
		return squareOrder{}, &CallError{Kind: CallRefused}
	}
	body, status, err := squareGet(ctx, creds, c, "/v2/orders/"+id)
	if err != nil {
		return squareOrder{}, err
	}
	var out struct {
		Order *squareOrder `json:"order"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Order == nil || out.Order.ID != id {
		return squareOrder{}, &CallError{Kind: CallUnreachable, Status: status}
	}
	return *out.Order, nil
}

// AccountID is the merchant the token belongs to: GET /v2/merchants/me.
func (square) AccountID(ctx context.Context, creds Credentials, c *Client) (string, error) {
	body, status, err := squareGet(ctx, creds, c, "/v2/merchants/me")
	if err != nil {
		return "", err
	}
	var out struct {
		Merchant *struct {
			ID string `json:"id"`
		} `json:"merchant"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Merchant == nil || !squareIDRe.MatchString(out.Merchant.ID) {
		return "", &CallError{Kind: CallUnreachable, Status: status}
	}
	return out.Merchant.ID, nil
}

func squareHeaders(req *http.Request, creds Credentials) {
	req.Header.Set("Authorization", "Bearer "+creds.APIKey.Reveal())
	req.Header.Set("Square-Version", SquareAPIVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", squareUserAgent)
}

// squareGet reads one answer of a list or a retrieval: the body and the status, or a *CallError.
func squareGet(ctx context.Context, creds Credentials, c *Client, pathAndQuery string) ([]byte, int, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, c.BaseURL(squareHost(creds.Settings))+pathAndQuery, nil)
	if err != nil {
		return nil, 0, &CallError{Kind: CallUnreachable}
	}
	squareHeaders(req, creds)
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, &CallError{Kind: CallUnreachable}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, resp.StatusCode, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		// A read makes nothing: a key that isn't accepted is the credentials', anything else may pass.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, resp.StatusCode, &CallError{Kind: CallCredentials, Status: resp.StatusCode}
		}
		return nil, resp.StatusCode, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
	}
	return body, resp.StatusCode, nil
}

// squareFailure classifies an answer to a call that makes an object and isn't a success, from its
// status alone. Square documents no answer as given before the request ran, so none is NotExecuted
// and every one leaves the outcome unknown: the key is looked for by its token and asked again, never
// replaced. What kind of failure it is decides only what a person is told: the credentials (401, 403),
// one that may pass (429, 408, a 5xx), or a refusal (any other 4xx, IDEMPOTENCY_KEY_REUSED among them).
func squareFailure(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &CallError{Kind: CallCredentials, Status: status, OutcomeUnknown: true}
	case status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return &CallError{Kind: CallUnreachable, Status: status, OutcomeUnknown: true}
	}
	return &CallError{Kind: CallRefused, Status: status, OutcomeUnknown: true}
}
