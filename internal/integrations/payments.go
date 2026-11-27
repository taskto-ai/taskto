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

type PaymentProvider interface {
	CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error)
	FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (link Link, found, expired bool, err error)
	LinkState(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (LinkState, error)
	AccountID(ctx context.Context, creds Credentials, c *Client) (string, error)
	KeyKept() time.Duration
}

type payableChecker interface {
	NothingPayable(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (bool, error)
}

type PaymentPoller interface {
	ReadPayment(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (Observed, error)
}

type Observed struct {
	Expired  bool
	Livemode bool
	Payments []ObservedPayment
}

type ObservedPayment struct {
	ObjectID, PaymentID string
	AmountMinor         int64
	Currency            string
	Livemode            bool
	PaidAt              time.Time
}

type NewLink struct {
	Token       string
	KeySeq      int
	AmountMinor int64
	Currency    string
	Description string
	SuccessURL  string
	CancelURL   string
}

func (l NewLink) IdempotencyKey() string { return l.Token }

type Link struct {
	URL      string
	ObjectID string
}

type LinkState int

const (
	LinkOpen LinkState = iota + 1
	LinkExpired
	LinkSettled
)

type CallKind int

const (
	CallUnreachable CallKind = iota + 1
	CallRefused
	CallCredentials
	CallLookupIncomplete
	CallSettled
)

type CallError struct {
	Kind           CallKind
	Status         int
	NotExecuted    bool
	OutcomeUnknown bool
	RetryAfter     time.Duration
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

func IsCall(err error) (*CallError, bool) {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

const (
	stripeHost          = "api.stripe.com"
	maxAnswer           = 1 << 20
	maxLinkURL          = 2048
	maxLinkID           = 255
	maxProductName      = 250
	fallbackProductName = "Payment"
	stripeUserAgent     = "taskto"
	stripeRefMetadata   = "taskto_ref"
)

var (
	stripeSessionID    = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9]{1,200}$`)
	stripeAccountID    = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,200}$`)
	stripeLinkID       = regexp.MustCompile(`^plink_[A-Za-z0-9]{1,200}$`)
	stripeCheckoutHost = "checkout.stripe.com"
	stripeLinkHost     = "buy.stripe.com"
)

func (stripe) CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error) {
	name := strings.TrimSpace(l.Description)
	if name == "" {
		name = fallbackProductName
	}
	if utf8.RuneCountInString(name) > maxProductName {
		name = string([]rune(name)[:maxProductName])
	}
	hosted := l.SuccessURL == ""
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
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	link, ok := stripeLinkOf(hosted, l.Token, out.Object, out.ID, out.URL)
	if !ok {
		return Link{}, &CallError{Kind: CallRefused, Status: resp.StatusCode, OutcomeUnknown: true}
	}
	return link, nil
}

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
	findPageSize = 100
	findMaxPages = 20
	findSkew     = 10 * time.Minute
)

func (p stripe) FindPaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (Link, bool, bool, error) {
	return p.find(ctx, creds, c, l, since, true)
}

func (p stripe) NothingPayable(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time) (bool, error) {
	_, found, _, err := p.find(ctx, creds, c, l, since, false)
	return !found && err == nil, err
}

func (stripe) find(ctx context.Context, creds Credentials, c *Client, l NewLink, since time.Time, oneExpired bool) (Link, bool, bool, error) {
	hosted := l.SuccessURL == ""
	sawExpired := false
	expiredID := ""
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
				Mode              string            `json:"mode"`
				Active            *bool             `json:"active"`
				AmountTotal       int64             `json:"amount_total"`
				ClientReferenceID *string           `json:"client_reference_id"`
				Metadata          map[string]string `json:"metadata"`
			} `json:"data"`
		}
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
			seen := Link{ObjectID: seenObjectID(o.ID)}
			if hosted && (o.Active == nil || !*o.Active) {
				return seen, false, false, &CallError{Kind: CallSettled, Status: status}
			}
			if !hosted {
				if o.Mode != "payment" {
					return seen, false, false, &CallError{Kind: CallRefused, Status: status}
				}
				switch o.Status {
				case "open":
				case "expired":
					if oneExpired && sawExpired && expiredID != seen.ObjectID {
						return seen, false, false, &CallError{Kind: CallRefused, Status: status}
					}
					sawExpired, expiredID = true, seen.ObjectID
					continue
				default:
					if o.Status == "complete" {
						return seen, false, false, &CallError{Kind: CallSettled, Status: status}
					}
					return seen, false, false, &CallError{Kind: CallRefused, Status: status}
				}
			}
			link, ok := stripeLinkOf(hosted, l.Token, o.Object, o.ID, o.URL)
			if !ok || !strings.EqualFold(o.Currency, l.Currency) {
				return seen, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			amount := o.AmountTotal
			if hosted {
				if amount, err = stripeLinkAmount(ctx, creds, c, o.ID, l.Currency); err != nil {
					return seen, false, false, err
				}
			}
			if amount != l.AmountMinor {
				return seen, false, false, &CallError{Kind: CallRefused, Status: status}
			}
			return link, true, false, nil
		}
		if !more {
			if sawExpired {
				return Link{ObjectID: expiredID}, false, true, nil
			}
			return Link{}, false, false, nil
		}
		last := data[len(data)-1].ID
		if last == "" {
			return Link{}, false, false, &CallError{Kind: CallUnreachable, Status: status}
		}
		q.Set("starting_after", last)
	}
	return Link{}, false, false, &CallError{Kind: CallLookupIncomplete}
}

func seenObjectID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "(no id)"
	}
	if len(id) > maxLinkID {
		return id[:maxLinkID]
	}
	return id
}

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
		Mode   string `json:"mode"`
		Active *bool  `json:"active"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != want || out.ID != objectID {
		return 0, &CallError{Kind: CallUnreachable, Status: status}
	}
	if !hosted && out.Mode != "payment" {
		return 0, &CallError{Kind: CallRefused, Status: status}
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
	return 0, &CallError{Kind: CallRefused, Status: status}
}

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
		return nil, resp.StatusCode, stripeLookupFailure(resp.StatusCode, resp.Header)
	}
	return body, resp.StatusCode, nil
}

const maxRetryAfter = time.Hour

func stripeLookupFailure(status int, h http.Header) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &CallError{Kind: CallCredentials, Status: status}
	}
	ce := &CallError{Kind: CallUnreachable, Status: status}
	if status == http.StatusTooManyRequests {
		if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && secs > 0 {
			ce.RetryAfter = min(time.Duration(secs)*time.Second, maxRetryAfter)
		}
	}
	return ce
}

type polledSession struct {
	ID            string        `json:"id"`
	Object        string        `json:"object"`
	Mode          string        `json:"mode"`
	Status        string        `json:"status"`
	PaymentStatus string        `json:"payment_status"`
	PaymentIntent *polledIntent `json:"payment_intent"`
	PaymentLink   *string       `json:"payment_link"`
	AmountTotal   int64         `json:"amount_total"`
	Currency      string        `json:"currency"`
	Livemode      bool          `json:"livemode"`
}

const stripePollExpand = "payment_intent.latest_charge"

type polledIntent struct {
	ID            string
	ChargeCreated int64
}

func (pi *polledIntent) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &pi.ID)
	}
	var o struct {
		ID           string          `json:"id"`
		Object       string          `json:"object"`
		LatestCharge json.RawMessage `json:"latest_charge"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	if o.Object != "payment_intent" {
		return errors.New("integrations: payment_intent isn't a PaymentIntent")
	}
	pi.ID = o.ID
	if len(o.LatestCharge) > 0 && o.LatestCharge[0] == '{' {
		var ch struct {
			Object  string `json:"object"`
			Created int64  `json:"created"`
		}
		if err := json.Unmarshal(o.LatestCharge, &ch); err != nil {
			return err
		}
		if ch.Object == "charge" {
			pi.ChargeCreated = ch.Created
		}
	}
	return nil
}

func (o polledSession) payment() (ObservedPayment, bool) {
	if o.Object != "checkout.session" || o.Mode != "payment" || o.Status != "complete" || o.PaymentStatus != "paid" ||
		!stripeSessionIDRe.MatchString(o.ID) || o.AmountTotal <= 0 || !currencyCodeRe.MatchString(o.Currency) {
		return ObservedPayment{}, false
	}
	p := ObservedPayment{ObjectID: o.ID, AmountMinor: o.AmountTotal, Currency: strings.ToUpper(o.Currency), Livemode: o.Livemode}
	if o.PaymentIntent != nil {
		if !stripeIntentIDRe.MatchString(o.PaymentIntent.ID) {
			return ObservedPayment{}, false
		}
		p.PaymentID = o.PaymentIntent.ID
		if o.PaymentIntent.ChargeCreated > 0 {
			p.PaidAt = time.Unix(o.PaymentIntent.ChargeCreated, 0).UTC()
		}
	}
	return p, true
}

func (stripe) ReadPayment(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (Observed, error) {
	if hosted {
		return stripeReadLinkPayments(ctx, creds, c, objectID)
	}
	if !stripeSessionID.MatchString(objectID) {
		return Observed{}, &CallError{Kind: CallRefused}
	}
	q := url.Values{"expand[]": {stripePollExpand}}
	body, status, err := stripeGet(ctx, creds, c, "/v1/checkout/sessions/"+objectID+"?"+q.Encode())
	if err != nil {
		return Observed{}, err
	}
	var out polledSession
	if err := json.Unmarshal(body, &out); err != nil || out.Object != "checkout.session" || out.ID != objectID {
		return Observed{}, &CallError{Kind: CallUnreachable, Status: status}
	}
	switch out.Status {
	case "open":
		return Observed{Livemode: out.Livemode}, nil
	case "expired":
		return Observed{Livemode: out.Livemode, Expired: true}, nil
	case "complete":
		if out.PaymentStatus != "paid" {
			return Observed{Livemode: out.Livemode}, nil
		}
		p, ok := out.payment()
		if !ok {
			return Observed{}, &CallError{Kind: CallRefused, Status: status}
		}
		return Observed{Livemode: out.Livemode, Payments: []ObservedPayment{p}}, nil
	}
	return Observed{}, &CallError{Kind: CallRefused, Status: status}
}

func stripeReadLinkPayments(ctx context.Context, creds Credentials, c *Client, linkID string) (Observed, error) {
	if !stripeLinkID.MatchString(linkID) {
		return Observed{}, &CallError{Kind: CallRefused}
	}
	q := url.Values{"payment_link": {linkID}, "status": {"complete"}, "limit": {strconv.Itoa(findPageSize)},
		"expand[]": {"data." + stripePollExpand}}
	body, status, err := stripeGet(ctx, creds, c, "/v1/checkout/sessions?"+q.Encode())
	if err != nil {
		return Observed{}, err
	}
	var page struct {
		Object  string           `json:"object"`
		HasMore *bool            `json:"has_more"`
		Data    *[]polledSession `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Object != "list" || page.Data == nil || page.HasMore == nil {
		return Observed{}, &CallError{Kind: CallUnreachable, Status: status}
	}
	var out Observed
	data := *page.Data
	for i := len(data) - 1; i >= 0; i-- {
		o := data[i]
		if o.PaymentLink == nil || *o.PaymentLink != linkID {
			continue
		}
		if o.Status != "complete" || o.PaymentStatus != "paid" {
			continue
		}
		p, ok := o.payment()
		if !ok {
			return Observed{}, &CallError{Kind: CallRefused, Status: status}
		}
		out.Livemode = p.Livemode
		out.Payments = append(out.Payments, p)
	}
	return out, nil
}

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

func (stripe) KeyKept() time.Duration { return stripeKeyKept }

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
