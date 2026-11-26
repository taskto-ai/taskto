package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The calls of the payments kind. A provider that makes payment links implements PaymentProvider;
// the registry's Provider is only what a connection holds.

// PaymentProvider is the part of a provider that makes payment links.
type PaymentProvider interface {
	// CreatePaymentLink asks the provider for a hosted page on which one payer pays one amount, in one
	// request that is never sent twice by the HTTP client itself. Its failures are *CallError, which
	// never holds the provider's words: they could name a customer, and what the provider says is not
	// for a person or a record to read. A failure is NotExecuted only when the request certainly never
	// ran (it never left, or the provider documents the answer as given before execution); any other
	// is OutcomeUnknown.
	CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error)
	// FindPaymentLink looks for the object a call under one key may have made without its answer
	// being noted: an object that carries l.Token, the key's own token, and nothing of any other key.
	// A call whose outcome is unknown (a 5xx, a timeout, a dropped connection, any error after the
	// request may have run) may have made one, and a call under a new key would make a second. since
	// is when the reference was first made; nothing of it was created before. found is false only when
	// the provider was read in full and has no such object that a payer can use: a read that couldn't
	// be finished is an error, never "not found". expired says that it saw an object of the token that
	// no payer can use and that was never paid (an expired Checkout Session): the key's outcome is then
	// known. Its failures are *CallError.
	FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (link Link, found, expired bool, err error)
	// LinkState reads, from the provider, whether the object (the one a call answered or a lookup
	// found) can take a payment now. What a call answered, and above all what the provider replays for
	// a key it kept, is the object as it was made: a session replayed a day later says "open" in its
	// own words and may have expired since. Nothing is saved as the link before this says LinkOpen.
	LinkState(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (LinkState, error)
	// AccountID is the id of the provider's account the credentials belong to. A reference keeps
	// it: what a call made is only to be looked for, and a call only to be repeated, in the account
	// it was made in, and a key replaced by another account's must not move an unfinished call there.
	// Its failures are *CallError.
	AccountID(ctx context.Context, creds Credentials, c *Client) (string, error)
}

// NewLink is what a payment link is asked for. Everything in it is the server's: the amount is
// exact, in the currency's smallest unit; the token is the key's; the two addresses are the
// server's own pages, never a definition's, and both are empty when the server has no public
// address: the payer then stays on a confirmation page the provider hosts.
type NewLink struct {
	// Token is the key's own token: the idempotency key of the call, the reference the provider echoes
	// back with the payment, and what a lookup for the key's object matches. Every key of a ref has
	// its own (the first key's is the ref's), so what one key made is never taken for another's.
	Token string
	// KeySeq is the number of the key among the ref's (0 for the first). It is not part of the key.
	KeySeq      int
	AmountMinor int64
	// Currency is the code, upper case.
	Currency    string
	Description string
	// SuccessURL and CancelURL are the server's own pages (both set), or both empty. They are the
	// reference's, as its first try asked: a retry sends the same parameters (a provider answers
	// a key used with other parameters with an error) and looks among the same kind of objects.
	SuccessURL string
	CancelURL  string
}

// IdempotencyKey is the key the call is sent with: the key's own token.
func (l NewLink) IdempotencyKey() string { return l.Token }

// Link is what the provider made.
type Link struct {
	// URL is the address the payer opens, on the provider's own domain.
	URL string
	// ObjectID is the provider's id of the object, which polling reads: a Checkout Session's
	// (cs_...) when the payer returns to the server's pages, a Payment Link's (plink_...) when not.
	ObjectID string
}

// LinkState is what an object at the provider can do now.
type LinkState int

const (
	// LinkOpen: a payer can pay it.
	LinkOpen LinkState = iota + 1
	// LinkExpired: no payer can use it, and it was never paid (an expired Checkout Session).
	LinkExpired
	// LinkSettled: it was paid, or no payer can use it for a reason that doesn't say it wasn't paid (a
	// completed session, a Payment Link deactivated or at its limit).
	LinkSettled
)

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
	// CallLookupIncomplete: the provider was read, every read worked, and the account holds more
	// than a lookup reads, so it can't be said whether the key made the object. It tells nothing: a
	// key whose outcome was unknown stays so.
	CallLookupIncomplete
	// CallSettled: the lookup found an object of the reference that was paid, or that no payer can
	// use any more for a reason that doesn't say it wasn't paid (a completed session, a Payment Link
	// deactivated or at its limit). Nothing is made: a person reconciles that payment.
	CallSettled
)

// CallError is the failure of a call to a provider. It carries no text of the provider's.
type CallError struct {
	Kind CallKind
	// Status is the HTTP status answered, 0 for none.
	Status int
	// NotExecuted, for a call that makes an object, is that the request certainly never ran: it never
	// left (no connection was made), or the provider answered with a refusal it documents as given
	// before execution, which keeps nothing under the key (a rate limit, a lock timeout, the key in use
	// by a request still running). The same key is asked again later, never a new one.
	NotExecuted bool
	// OutcomeUnknown, for a call that makes an object, is that it may have run and what it made, if
	// anything, is not known: every failure after the request may have left that isn't NotExecuted
	// (any 4xx or 5xx, 408 and 409 included, a timeout, a dropped connection, an answer that can't be
	// read). The provider says to resolve the original outcome before a call under a new key
	// (https://docs.stripe.com/error-low-level#server-errors), and may keep even a 4xx under the key
	// once execution began (https://docs.stripe.com/error-low-level#content-errors), so the reference
	// gets no new key until it is resolved.
	OutcomeUnknown bool
}

func (e *CallError) Error() string {
	switch e.Kind {
	case CallCredentials:
		return "the provider doesn't accept the key (status " + strconv.Itoa(e.Status) + ")"
	case CallRefused:
		return "the provider refused the request (status " + strconv.Itoa(e.Status) + ")"
	case CallLookupIncomplete:
		return "the provider holds more than a lookup reads"
	case CallSettled:
		return "the provider holds a payment of the reference that was paid or can no longer be used"
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
	// stripeAccountID is what the id of an account looks like.
	stripeAccountID = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,200}$`)
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
// Either way the parameters are the same on every call under one key, so Stripe's idempotency (the
// key's token as the key) answers a repeated call with what the first one made.
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
		return Link{}, &CallError{Kind: CallRefused, NotExecuted: true}
	}
	stripeHeaders(req, creds)
	req.Header.Set("Idempotency-Key", l.IdempotencyKey())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, sent, err := sendOnce(c, req)
	if err != nil {
		// Unless no connection was ever made, the request may have reached the provider, which then
		// holds what it made and says nothing of it. Nothing of err is kept: it can quote the address.
		if !sent {
			return Link{}, &CallError{Kind: CallUnreachable, NotExecuted: true}
		}
		return Link{}, &CallError{Kind: CallUnreachable, OutcomeUnknown: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Link{}, &CallError{Kind: CallUnreachable, Status: resp.StatusCode, OutcomeUnknown: true}
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
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	link, ok := stripeLinkOf(hosted, l.Token, out.Object, out.ID, out.URL)
	if !ok {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	return link, nil
}

// sendOnce sends a request that makes an object exactly once, and says whether it may have left.
//
// Go's transport sends a request again by itself, on a new connection, when a reused one breaks
// before the answer and the request is "replayable": its body can be read again (GetBody) and it has an
// Idempotency-Key header, which ours has. The first copy may have reached the provider and run, and
// the error returned is then the second's, a failed dial that says nothing left. So the body is made
// one that can't be read again, which stops every such resend (only a request that wrote nothing at
// all could be, and with a body it isn't either). And whether anything may have left is told by the
// connection, not by the error: sent is false only if no connection was ever handed to the request,
// so not a byte of it was written.
func sendOnce(c *Client, req *http.Request) (*http.Response, bool, error) {
	req.GetBody = nil
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { sent.Store(true) }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := c.Do(req)
	return resp, sent.Load(), err
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
	// time). A lookup that reaches the last page with more behind it finds nothing out: it fails
	// with CallLookupIncomplete, which tells nothing of the key's outcome.
	findPageSize = 100
	findMaxPages = 20
	// findSkew is how far before the ref's creation a session may have been created by the
	// provider's clock.
	findSkew = 10 * time.Minute
)

// FindPaymentLink lists what the account made and picks the object that has the key's token. Stripe
// documents the outcome of a request answered with a 5xx as indeterminate, "the operation might
// have produced side effects even though you received an error", and says not to "retry with a
// new idempotency key until you've resolved the original outcome", by retrieving the object and
// matching it on a local identifier kept in its metadata
// (https://docs.stripe.com/error-low-level#server-errors). A 500 is saved under the idempotency
// key and answered again to that key (https://docs.stripe.com/api/idempotent_requests), so asking
// again under the key can say nothing more. So the object a lost call made is looked for, by the
// key's own token, at the pinned API version (StripeAPIVersion):
//   - List Checkout Sessions, https://docs.stripe.com/api/checkout/sessions/list: filters by
//     created, status, payment_intent and payment_link, and none by client_reference_id or
//     metadata, so the sessions created since the ref was made are read and matched on
//     client_reference_id, the token (the object has it, https://docs.stripe.com/api/checkout/sessions/object);
//   - List all payment links, https://docs.stripe.com/api/payment-link/list: only active and the
//     cursors, so the links are read newest first, active or not, and matched on metadata[taskto_ref];
//   - the Search API (https://docs.stripe.com/search) doesn't list Checkout Sessions or Payment
//     Links, and its data can take a minute to be searchable ("don't use search for
//     read-after-write flows", which this is), so it isn't used.
//
// An object is adopted only if it asks what the payment asks now: a session's amount_total and
// currency, and a link's currency and the line items it lists
// (https://docs.stripe.com/api/payment-link/line_items). The list APIs read what was written. Every
// state is listed (sessions of any status since the reference was made, links active or not), since
// an object of the token that is no longer payable may have been paid: an open session or an active
// link is adopted, an expired session is passed over (it can't be paid), and a completed session or
// an inactive link (deactivated, or at its limit of payments) stops the call with a refusal, so no
// second object is made and a person reconciles the payment.
func (stripe) FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (Link, bool, bool, error) {
	hosted := l.SuccessURL == ""
	// sawExpired: an object of the token that can't be paid and never was.
	sawExpired := false
	path := "/v1/checkout/sessions"
	q := url.Values{"limit": {strconv.Itoa(findPageSize)}}
	if hosted {
		path = "/v1/payment_links"
	} else {
		q.Set("created[gte]", strconv.FormatInt(since.Add(-findSkew).Unix(), 10))
	}
	for range findMaxPages {
		body, status, err := stripeGet(ctx, creds, c, path+"?"+q.Encode())
		if err != nil {
			return Link{}, false, false, err
		}
		var page struct {
			Object  string `json:"object"`
			HasMore *bool  `json:"has_more"`
			Data    *[]struct {
				ID                string            `json:"id"`
				Object            string            `json:"object"`
				URL               string            `json:"url"`
				Currency          string            `json:"currency"`
				Status            string            `json:"status"`
				Active            *bool             `json:"active"`
				AmountTotal       int64             `json:"amount_total"`
				ClientReferenceID *string           `json:"client_reference_id"`
				Metadata          map[string]string `json:"metadata"`
			} `json:"data"`
		}
		// A negative answer is only as good as the page it rests on: an answer that isn't a list,
		// that doesn't say whether there is more, or that says there is more and shows none, says
		// nothing about the object.
		if err := json.Unmarshal(body, &page); err != nil || page.Object != "list" || page.Data == nil || page.HasMore == nil || (*page.HasMore && len(*page.Data) == 0) {
			return Link{}, false, false, &CallError{Kind: CallUnreachable, Status: status}
		}
		data, more := *page.Data, *page.HasMore
		for _, o := range data {
			var ours bool
			if hosted {
				ours = o.Metadata[stripeRefMetadata] == l.Token
			} else {
				ours = o.ClientReferenceID != nil && *o.ClientReferenceID == l.Token
			}
			if !ours {
				continue
			}
			// Every state is read, not only the payable ones: an object of the token that was paid, or
			// that a payer can no longer use for another reason, shows the earlier try did make one, and
			// must not read as "not made" (the next call would make a second payable link).
			if hosted && (o.Active == nil || !*o.Active) {
				// Deactivated by a person, or by its limit of payments: no one can say it wasn't paid.
				return Link{}, false, false, &CallError{Kind: CallSettled, Status: status}
			}
			if !hosted {
				switch o.Status {
				case "open":
				case "expired":
					// Can't be paid, and the payer was never given an address that works: the call that
					// made it is accounted for, and another may be made.
					sawExpired = true
					continue
				default:
					if o.Status == "complete" {
						return Link{}, false, false, &CallError{Kind: CallSettled, Status: status}
					}
					return Link{}, false, false, &CallError{Kind: CallRefused, Status: status} // a state not said
				}
			}
			link, ok := stripeLinkOf(hosted, l.Token, o.Object, o.ID, o.URL)
			if !ok || !strings.EqualFold(o.Currency, l.Currency) {
				// Ours, and not a link the payer could be given: nothing says what it is.
				return Link{}, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			amount := o.AmountTotal
			if hosted {
				// A link's list entry has no amount: its line items do.
				if amount, err = stripeLinkAmount(ctx, creds, c, o.ID, l.Currency); err != nil {
					return Link{}, false, false, err
				}
			}
			if amount != l.AmountMinor {
				// Made for another amount than the one asked for now: its address is never given
				// to a payer, and no other is made while it stands.
				return Link{}, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			return link, true, false, nil
		}
		if !more {
			return Link{}, false, sawExpired, nil
		}
		last := data[len(data)-1].ID
		if last == "" {
			return Link{}, false, false, &CallError{Kind: CallUnreachable, Status: status} // no cursor to read on from
		}
		q.Set("starting_after", last)
	}
	// Every page read was whole, and there is more behind them: asking again reads the same.
	return Link{}, false, false, &CallError{Kind: CallLookupIncomplete}
}

// LinkState retrieves the object and says what it can do now: a Checkout Session by its status
// (https://docs.stripe.com/api/checkout/sessions/retrieve: open, complete or expired), a Payment Link
// by whether it is active (https://docs.stripe.com/api/payment-links/payment_links/retrieve; one at
// its limit of payments or deactivated is not, and nothing says it wasn't paid).
func (stripe) LinkState(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (LinkState, error) {
	want, idPattern, path := "checkout.session", stripeSessionID, "/v1/checkout/sessions/"
	if hosted {
		want, idPattern, path = "payment_link", stripeLinkID, "/v1/payment_links/"
	}
	if !idPattern.MatchString(objectID) {
		return 0, &CallError{Kind: CallRefused}
	}
	body, status, err := stripeGet(ctx, creds, c, path+objectID)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Active *bool  `json:"active"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != want || out.ID != objectID {
		return 0, &CallError{Kind: CallUnreachable, Status: status}
	}
	if hosted {
		switch {
		case out.Active == nil:
			return 0, &CallError{Kind: CallUnreachable, Status: status}
		case *out.Active:
			return LinkOpen, nil
		}
		return LinkSettled, nil
	}
	switch out.Status {
	case "open":
		return LinkOpen, nil
	case "expired":
		return LinkExpired, nil
	case "complete":
		return LinkSettled, nil
	}
	return 0, &CallError{Kind: CallRefused, Status: status} // a state not said
}

// stripeLinkAmount is what a Payment Link asks, from its line items
// (https://docs.stripe.com/api/payment-link/line_items), which must be in the currency and fit
// one page.
func stripeLinkAmount(ctx context.Context, creds Credentials, c *Client, id, currency string) (int64, error) {
	body, status, err := stripeGet(ctx, creds, c, "/v1/payment_links/"+id+"/line_items?limit="+strconv.Itoa(findPageSize))
	if err != nil {
		return 0, err
	}
	var items struct {
		Object  string `json:"object"`
		HasMore bool   `json:"has_more"`
		Data    *[]struct {
			Currency    string `json:"currency"`
			AmountTotal int64  `json:"amount_total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &items); err != nil || items.Object != "list" || items.Data == nil {
		return 0, &CallError{Kind: CallUnreachable, Status: status}
	}
	if items.HasMore {
		return 0, &CallError{Kind: CallRefused, Status: status}
	}
	var sum int64
	for _, it := range *items.Data {
		if !strings.EqualFold(it.Currency, currency) {
			return 0, &CallError{Kind: CallRefused, Status: status}
		}
		sum += it.AmountTotal
	}
	return sum, nil
}

// stripeGet reads one answer of a list or a retrieval: the body and the status, or a *CallError.
func stripeGet(ctx context.Context, creds Credentials, c *Client, pathAndQuery string) ([]byte, int, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, c.BaseURL(stripeHost)+pathAndQuery, nil)
	if err != nil {
		return nil, 0, &CallError{Kind: CallUnreachable}
	}
	stripeHeaders(req, creds)
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
		return nil, resp.StatusCode, stripeLookupFailure(resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

// stripeLookupFailure classifies an answer to a read, which makes nothing.
func stripeLookupFailure(status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &CallError{Kind: CallCredentials, Status: status}
	}
	return &CallError{Kind: CallUnreachable, Status: status}
}

// stripeFailure classifies an answer to a call that makes an object and isn't a success, from its
// status and the code Stripe names, never from its words. Only what Stripe documents as refused before
// the request ran is NotExecuted:
//   - 429 with the code rate_limit (https://docs.stripe.com/rate-limits) or lock_timeout, an object lock
//     that timed out (https://docs.stripe.com/rate-limits#object-lock-timeouts);
//   - 409 with the code idempotency_key_in_use: another request with the same key is still running
//     (https://docs.stripe.com/error-codes#idempotency-key-in-use). That request may make the object, so
//     the same key is asked again later, and its answer is what the first one made.
//
// Every other answer leaves the outcome unknown. A 5xx is indeterminate
// (https://docs.stripe.com/error-low-level#server-errors); Stripe saves the result of a request whose
// execution began under its key, a 4xx included (https://docs.stripe.com/error-low-level#content-errors,
// https://docs.stripe.com/api/idempotent_requests), so a refusal may follow side effects; a 429 of
// another kind, a 409 that is not the key in use and a 408 say nothing documented of execution.
func stripeFailure(status int, body []byte) error {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusTooManyRequests && (e.Error.Code == "rate_limit" || e.Error.Code == "lock_timeout"),
		status == http.StatusConflict && e.Error.Code == "idempotency_key_in_use":
		return &CallError{Kind: CallUnreachable, Status: status, NotExecuted: true}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &CallError{Kind: CallCredentials, Status: status, OutcomeUnknown: true}
	case status >= 500 || status == http.StatusTooManyRequests || status == http.StatusConflict || status == http.StatusRequestTimeout:
		return &CallError{Kind: CallUnreachable, Status: status, OutcomeUnknown: true}
	}
	return &CallError{Kind: CallRefused, Status: status, OutcomeUnknown: true}
}

// AccountID is the id of the account the key belongs to: Stripe's "Retrieve account" with no id
// answers the account of the authenticating key (https://docs.stripe.com/api/accounts/retrieve).
// The id is an acct_ followed by letters and digits, and an answer without one is no answer.
func (stripe) AccountID(ctx context.Context, creds Credentials, c *Client) (string, error) {
	body, status, err := stripeGet(ctx, creds, c, "/v1/account")
	if err != nil {
		return "", err
	}
	var out struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != "account" || !stripeAccountID.MatchString(out.ID) {
		return "", &CallError{Kind: CallUnreachable, Status: status}
	}
	return out.ID, nil
}
