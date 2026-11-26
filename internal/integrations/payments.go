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
	"time"
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
	// FindPaymentLink looks for the object an earlier call for the same reference may have made
	// without its answer being noted: a call that failed with an indeterminate result (a 5xx, a
	// timeout, a dropped connection) may have been executed, and the next call has a key of its
	// own, which would make a second object. since is when the reference was first made; nothing
	// of it was created before. found is false only when the provider was read in full and has
	// no such object: a read that couldn't be finished is an error, never "not found". Its
	// failures are *CallError that are never KeyKept.
	FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (link Link, found bool, err error)
}

// NewLink is what a payment link is asked for. Everything in it is the server's: the amount is
// exact, in the currency's smallest unit; the token is the ref's; the two addresses are the
// server's own pages, never a definition's, and both are empty when the server has no public
// address: the payer then stays on a confirmation page the provider hosts.
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
	// SuccessURL and CancelURL are the server's own pages (both set), or both empty.
	SuccessURL string
	CancelURL  string
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
	// ObjectID is the provider's id of the object, which polling reads: a Checkout Session's
	// (cs_...) when the payer returns to the server's pages, a Payment Link's (plink_...) when not.
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
	// stripeRefMetadata is the key of a Payment Link's metadata that holds the ref's token.
	stripeRefMetadata = "taskto_ref"
)

var (
	// stripeSessionID is what the id of a Checkout Session looks like.
	stripeSessionID = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9]{1,200}$`)
	// stripeLinkID is what the id of a Payment Link looks like.
	stripeLinkID = regexp.MustCompile(`^plink_[A-Za-z0-9]{1,200}$`)
	// stripeCheckoutHost is the domain of the pages Stripe hosts for a Checkout Session, and
	// stripeLinkHost of a Payment Link's (the default submit_type, which is the one asked for, is
	// buy.stripe.com), where a link must point.
	stripeCheckoutHost = "checkout.stripe.com"
	stripeLinkHost     = "buy.stripe.com"
)

// CreatePaymentLink makes a hosted page for one payment of an exact amount, in one of two shapes
// that the server's address decides.
//
// With the server's own pages (a public URL is set) it makes a Checkout Session in payment mode:
// success_url and cancel_url are those pages, client_reference_id is the ref's token (what the
// event of the payment echoes back), and the session expires by itself within Stripe's default of
// 24 hours.
//
// Without a public URL the payer has no page of ours to return to. Stripe documents success_url
// only as "required conditionally" and every hosted example sets one, so a hosted session
// without one is not relied on. It makes a Payment Link instead, whose payer stays on a
// confirmation page that Stripe hosts (after_completion[type]=hosted_confirmation) and which
// takes one payment (restrictions[completed_sessions][limit]=1: an invoice's link can't be paid
// twice). The token is in the link's metadata, which Stripe copies to the session of the payment,
// and in the address as ?client_reference_id=, the documented way a link's sessions carry one. A
// link doesn't expire by itself.
//
// Either way the parameters are the same on every try of one reference, so Stripe's idempotency
// (the token as the key) finds the first call's object.
//
// Stripe's documentation relied on, at the pinned API version (StripeAPIVersion):
//   - Create a Checkout Session, https://docs.stripe.com/api/checkout/sessions/create: success_url
//     is "required conditionally" and not allowed for an embedded or elements ui_mode, cancel_url
//     is optional, expires_at runs 30 minutes to 24 hours (default 24), client_reference_id has at
//     most 200 characters;
//   - Create a payment link, https://docs.stripe.com/api/payment-link/create: after_completion
//     (type hosted_confirmation, "displays a message on the hosted surface after the purchase is
//     complete", or redirect), restrictions[completed_sessions][limit], line_items with price_data
//     (https://docs.stripe.com/payment-links/api), and metadata, "automatically copied to checkout
//     sessions created by this payment link";
//   - Track a payment link, https://docs.stripe.com/payment-links/url-parameters: client_reference_id
//     in the link's address, alphanumeric, dashes or underscores, up to 200 characters, reaches
//     checkout.session.completed;
//   - List Checkout Sessions, https://docs.stripe.com/api/checkout/sessions/list: payment_link is a
//     filter, which is how a later poll finds the payments of a link.
func (stripe) CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error) {
	name := strings.TrimSpace(l.Description)
	if name == "" {
		name = fallbackProductName
	}
	if utf8.RuneCountInString(name) > maxProductName {
		name = string([]rune(name)[:maxProductName])
	}
	hosted := l.SuccessURL == "" // no pages of ours: Stripe's own confirmation
	form := url.Values{}
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(l.Currency))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(l.AmountMinor, 10))
	form.Set("line_items[0][price_data][product_data][name]", name)
	endpoint := "/v1/checkout/sessions"
	if hosted {
		endpoint = "/v1/payment_links"
		form.Set("after_completion[type]", "hosted_confirmation")
		form.Set("restrictions[completed_sessions][limit]", "1")
		form.Set("metadata["+stripeRefMetadata+"]", l.Token)
	} else {
		form.Set("mode", "payment")
		form.Set("success_url", l.SuccessURL)
		form.Set("cancel_url", l.CancelURL)
		form.Set("client_reference_id", l.Token)
	}

	req, err := c.NewRequest(ctx, http.MethodPost, c.BaseURL(stripeHost)+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Link{}, &CallError{Kind: CallRefused}
	}
	stripeHeaders(req, creds)
	req.Header.Set("Idempotency-Key", l.IdempotencyKey())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
	if err := json.Unmarshal(body, &out); err != nil {
		// An answer that isn't the object asked for: the call was made, and nothing says what became
		// of it, so it is not tried again as if it had not been.
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode}
	}
	link, ok := stripeLinkOf(hosted, l.Token, out.Object, out.ID, out.URL)
	if !ok {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode}
	}
	return link, nil
}

func stripeHeaders(req *http.Request, creds Credentials) {
	req.Header.Set("Authorization", "Bearer "+creds.APIKey.Reveal())
	req.Header.Set("Stripe-Version", StripeAPIVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", stripeUserAgent)
}

// stripeLinkOf checks what Stripe says of an object it made (or of one a list shows) and returns
// the link: the object must be the kind asked for, with an id and an address of the shapes and
// the domain Stripe uses for it. A hosted Payment Link's address gets the ref's token as
// ?client_reference_id=, the documented way its payments carry one.
func stripeLinkOf(hosted bool, token, object, id, rawURL string) (Link, bool) {
	want, idPattern, host := "checkout.session", stripeSessionID, stripeCheckoutHost
	if hosted {
		want, idPattern, host = "payment_link", stripeLinkID, stripeLinkHost
	}
	if object != want || !idPattern.MatchString(id) || len(id) > maxLinkID {
		return Link{}, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != host || u.User != nil || len(rawURL) > maxLinkURL {
		return Link{}, false
	}
	if hosted {
		if u.RawQuery != "" || u.Fragment != "" {
			return Link{}, false
		}
		u.RawQuery = "client_reference_id=" + token
		rawURL = u.String()
		if len(rawURL) > maxLinkURL {
			return Link{}, false
		}
	}
	return Link{URL: rawURL, ObjectID: id}, true
}

const (
	// findPageSize is the most Stripe lists in a page, and findMaxPages how many pages a lookup
	// reads, newest first: the object looked for is among the newest, since its call was one of
	// the last the integration's account saw (a Checkout Session is also bounded by its creation
	// time). A lookup that reaches the last page with more behind it finds nothing out, so it
	// fails and the entry is tried again.
	findPageSize = 100
	findMaxPages = 20
	// findSkew is how far before the ref's creation a session may have been created by the
	// provider's clock.
	findSkew = 10 * time.Minute
)

// FindPaymentLink lists what the account made and picks the object that has the ref's token. Stripe
// documents the outcome of a request answered with a 5xx as indeterminate, "the operation might
// have produced side effects even though you received an error", and says not to "retry with a
// new idempotency key until you've resolved the original outcome", by retrieving the object and
// matching it on a local identifier kept in its metadata
// (https://docs.stripe.com/error-low-level#server-errors). A 500 is saved under the idempotency
// key and answered again to that key (https://docs.stripe.com/api/idempotent_requests), which is
// why the next call has a key of its own. So before it, the object a lost call made is looked
// for, at the pinned API version (StripeAPIVersion):
//   - List Checkout Sessions, https://docs.stripe.com/api/checkout/sessions/list: filters by
//     created, status, payment_intent and payment_link, and none by client_reference_id or
//     metadata, so the open sessions created since the ref was made are read and matched on
//     client_reference_id, the token (the object has it, https://docs.stripe.com/api/checkout/sessions/object);
//   - List all payment links, https://docs.stripe.com/api/payment-link/list: only active and the
//     cursors, so the active links are read newest first and matched on metadata[taskto_ref];
//   - the Search API (https://docs.stripe.com/search) doesn't list Checkout Sessions or Payment
//     Links, and its data can take a minute to be searchable ("don't use search for
//     read-after-write flows", which this is), so it isn't used.
//
// The list APIs read what was written. A session that is no longer open (paid or expired) or a
// link that is no longer active is not adopted: the payer was never given its address.
func (stripe) FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (Link, bool, error) {
	hosted := l.SuccessURL == ""
	path := "/v1/checkout/sessions"
	q := url.Values{"limit": {strconv.Itoa(findPageSize)}}
	if hosted {
		path = "/v1/payment_links"
		q.Set("active", "true")
	} else {
		q.Set("status", "open")
		q.Set("created[gte]", strconv.FormatInt(since.Add(-findSkew).Unix(), 10))
	}
	for range findMaxPages {
		req, err := c.NewRequest(ctx, http.MethodGet, c.BaseURL(stripeHost)+path+"?"+q.Encode(), nil)
		if err != nil {
			return Link{}, false, &CallError{Kind: CallUnreachable}
		}
		stripeHeaders(req, creds)
		resp, err := c.Do(req)
		if err != nil {
			return Link{}, false, &CallError{Kind: CallUnreachable}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
		_ = resp.Body.Close()
		if err != nil {
			return Link{}, false, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
		}
		if resp.StatusCode != http.StatusOK {
			return Link{}, false, stripeLookupFailure(resp.StatusCode)
		}
		var page struct {
			HasMore bool `json:"has_more"`
			Data    []struct {
				ID                string            `json:"id"`
				Object            string            `json:"object"`
				URL               string            `json:"url"`
				ClientReferenceID *string           `json:"client_reference_id"`
				Metadata          map[string]string `json:"metadata"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return Link{}, false, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
		}
		for _, o := range page.Data {
			var ours bool
			if hosted {
				ours = o.Metadata[stripeRefMetadata] == l.Token
			} else {
				ours = o.ClientReferenceID != nil && *o.ClientReferenceID == l.Token
			}
			if !ours {
				continue
			}
			link, ok := stripeLinkOf(hosted, l.Token, o.Object, o.ID, o.URL)
			if !ok {
				// Ours, and not a link the payer could be given: nothing says what it is.
				return Link{}, false, &CallError{Kind: CallRefused, Status: resp.StatusCode}
			}
			return link, true, nil
		}
		if !page.HasMore || len(page.Data) == 0 {
			return Link{}, false, nil
		}
		q.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
	return Link{}, false, &CallError{Kind: CallUnreachable} // not read in full
}

// stripeLookupFailure classifies an answer to a read: never KeyKept, since a read has no key.
func stripeLookupFailure(status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &CallError{Kind: CallCredentials, Status: status}
	}
	return &CallError{Kind: CallUnreachable, Status: status}
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
