package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The calls of the payments kind. A provider that makes payment links implements PaymentProvider;
// the registry's Provider is only what a connection holds.

// PaymentProvider is the part of a provider that makes payment links.
type PaymentProvider interface {
	// CreatePaymentLink asks the provider for a hosted page on which one payer pays one amount.
	// Its failures are *CallError, which never holds the provider's words: they could name a
	// customer, and what the provider says is not for a person or a record to read.
	CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error)
}

// NewLink is what a payment link is asked for. Everything in it is the server's: the amount is
// exact, in the currency's smallest unit; the token is the ref's; the two addresses are the
// server's own pages, never a definition's.
type NewLink struct {
	// Token is the reference the provider echoes back with the payment and, with KeySeq, the
	// idempotency key of the call.
	Token string
	// KeySeq is how many times the provider answered an error to this reference: from the second
	// try on, a call that was answered with a saved failure has a key of its own (Provider keeps
	// the failure under the key and answers it again to the same one).
	KeySeq      int
	AmountMinor int64
	// Currency is the code, upper case.
	Currency    string
	Description string
	SuccessURL  string
	CancelURL   string
}

// IdempotencyKey is the key the call is sent with: the token on the first try.
func (l NewLink) IdempotencyKey() string {
	if l.KeySeq == 0 {
		return l.Token
	}
	return l.Token + "-" + strconv.Itoa(l.KeySeq)
}

// Link is what the provider made.
type Link struct {
	// URL is the address the payer opens, on the provider's own domain.
	URL string
	// ObjectID is the provider's id of the object, which polling reads.
	ObjectID string
}

// CallKind says what kind of failure a call had, which decides what happens next.
type CallKind int

const (
	// CallUnreachable: no answer, or an answer that may pass (a timeout, 429, 5xx) or can't be
	// read. The call is tried again.
	CallUnreachable CallKind = iota + 1
	// CallRefused: the provider refused the request, and asking again the same way will not help.
	CallRefused
	// CallCredentials: the provider doesn't accept the integration's key.
	CallCredentials
)

// CallError is the failure of a call to a provider. It carries no text of the provider's.
type CallError struct {
	Kind CallKind
	// Status is the HTTP status answered, 0 for none.
	Status int
	// KeyKept is whether the provider may have kept this failure under the call's idempotency key,
	// so that asking again under the same key would get the same failure.
	KeyKept bool
}

func (e *CallError) Error() string {
	switch e.Kind {
	case CallCredentials:
		return "the provider doesn't accept the key (status " + strconv.Itoa(e.Status) + ")"
	case CallRefused:
		return "the provider refused the request (status " + strconv.Itoa(e.Status) + ")"
	}
	return "the provider didn't answer (status " + strconv.Itoa(e.Status) + ")"
}

// IsCall reports whether err is a *CallError and returns it.
func IsCall(err error) (*CallError, bool) {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

const (
	stripeHost = "api.stripe.com"
	// maxAnswer bounds what is read of an answer.
	maxAnswer = 1 << 20
	// The most of an address the write-back takes, which the app's fields are guaranteed to hold
	// (apps.minLinkLength, minPaymentIDLength).
	maxLinkURL = 2048
	maxLinkID  = 255
	// maxProductName is what a payer reads as what they pay for.
	maxProductName = 250
	// fallbackProductName is what the payer reads when the app's description is empty.
	fallbackProductName = "Payment"
	// stripeUserAgent says what is calling, and nothing about who.
	stripeUserAgent = "taskto"
)

var (
	// stripeSessionID is what the id of a Checkout Session looks like.
	stripeSessionID = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9]{1,200}$`)
	// stripeCheckoutHost is the domain of the pages Stripe hosts, where a link must point.
	stripeCheckoutHost = "checkout.stripe.com"
)

// CreatePaymentLink makes a Checkout Session in payment mode: a hosted page for one payment, which
// carries the ref's token as client_reference_id (what the event of the payment echoes back) and
// expires by itself within Stripe's default of 24 hours. Its parameters are the same on every try
// of one reference, so Stripe's idempotency (the token as the key) finds the first call's object.
func (stripe) CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error) {
	name := strings.TrimSpace(l.Description)
	if name == "" {
		name = fallbackProductName
	}
	if utf8.RuneCountInString(name) > maxProductName {
		name = string([]rune(name)[:maxProductName])
	}
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", l.SuccessURL)
	form.Set("cancel_url", l.CancelURL)
	form.Set("client_reference_id", l.Token)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(l.Currency))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(l.AmountMinor, 10))
	form.Set("line_items[0][price_data][product_data][name]", name)

	req, err := c.NewRequest(ctx, http.MethodPost, c.BaseURL(stripeHost)+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return Link{}, &CallError{Kind: CallRefused}
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey.Reveal())
	req.Header.Set("Stripe-Version", StripeAPIVersion)
	req.Header.Set("Idempotency-Key", l.IdempotencyKey())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", stripeUserAgent)

	resp, err := c.Do(req)
	if err != nil {
		// The call may or may not have been made: asked again under the same key, the provider
		// answers with the object if it was. Nothing of err is kept: it can quote the address.
		return Link{}, &CallError{Kind: CallUnreachable}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Link{}, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return Link{}, stripeFailure(resp.StatusCode, body)
	}
	var out struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != "checkout.session" || !stripeSessionID.MatchString(out.ID) || len(out.ID) > maxLinkID {
		// An answer that isn't the object asked for: the call was made, and nothing says what became
		// of it, so it is not tried again as if it had not been.
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode}
	}
	if u, err := url.Parse(out.URL); err != nil || u.Scheme != "https" || u.Hostname() != stripeCheckoutHost || u.User != nil || len(out.URL) > maxLinkURL {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode}
	}
	return Link{URL: out.URL, ObjectID: out.ID}, nil
}

// stripeFailure classifies an answer that isn't a success, from its status and the kind of error
// Stripe names, never from its words.
func stripeFailure(status int, body []byte) error {
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &CallError{Kind: CallCredentials, Status: status}
	case status == http.StatusTooManyRequests || status == http.StatusConflict || status == http.StatusRequestTimeout:
		// Not executed: asked again under the same key, it runs.
		return &CallError{Kind: CallUnreachable, Status: status}
	case status >= 500:
		// Stripe saves a failure of an executed request under its key: the same key gets it again.
		return &CallError{Kind: CallUnreachable, Status: status, KeyKept: true}
	case e.Error.Type == "idempotency_error":
		return &CallError{Kind: CallRefused, Status: status, KeyKept: true}
	}
	return &CallError{Kind: CallRefused, Status: status}
}
