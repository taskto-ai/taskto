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

const SquareAPIVersion = "2026-11-18"

const SquareSignatureHeader = "X-Square-Hmacsha256-Signature"

const (
	squareRetryWindow                = 24 * time.Hour
	squareSkew                       = 5 * time.Minute
	squareKeyKept      time.Duration = 0
	squareFindPageSize               = 1000
	squareFindMaxPages               = 20
	squareMaxName                    = 255
	squareUserAgent                  = "taskto"
)

var (
	squareIDRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,192}$`)
	squareEventIDRe  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	squareCurrencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

func squareHost(settings map[string]string) string { return square{}.Hosts(settings)[0] }

func squareLinkHost(settings map[string]string) string {
	if settings["environment"] == "production" {
		return "square.link"
	}
	return "sandbox.square.link"
}

func (square) APIVersion() string { return SquareAPIVersion }

func (square) KeyKept() time.Duration { return squareKeyKept }

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
		AllowTipping  bool   `json:"allow_tipping"`
		EnableCoupon  bool   `json:"enable_coupon"`
		EnableLoyalty bool   `json:"enable_loyalty"`
		RedirectURL   string `json:"redirect_url,omitempty"`
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
			seen := Link{ObjectID: seenObjectID(pl.ID)}
			link, ok := squareLinkOf(creds.Settings, l, pl)
			if !ok {
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
			return seen, false, false, &CallError{Kind: CallRefused, Status: status}
		}
		if page.Cursor == "" {
			return Link{}, false, false, nil
		}
		q.Set("cursor", page.Cursor)
	}
	return Link{}, false, false, &CallError{Kind: CallLookupIncomplete}
}

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
	return 0, &CallError{Kind: CallRefused, Status: status}
}

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
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, resp.StatusCode, &CallError{Kind: CallCredentials, Status: resp.StatusCode}
		}
		return nil, resp.StatusCode, &CallError{Kind: CallUnreachable, Status: resp.StatusCode}
	}
	return body, resp.StatusCode, nil
}

func squareFailure(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &CallError{Kind: CallCredentials, Status: status, OutcomeUnknown: true}
	case status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return &CallError{Kind: CallUnreachable, Status: status, OutcomeUnknown: true}
	}
	return &CallError{Kind: CallRefused, Status: status, OutcomeUnknown: true}
}
