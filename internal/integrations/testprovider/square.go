package testprovider

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const SquareAPIVersion = "2026-11-18"

const SquareSignatureHeader = "X-Square-Hmacsha256-Signature"

var (
	SquareTestToken         = "sq-" + "sandbox-" + strings.Repeat("Fake", 6)
	SquareSignatureKey      = "sqsig" + strings.Repeat("Fake", 5)
	SquareOtherSignatureKey = "sqsig" + strings.Repeat("Rota", 5)
	SquareMerchantID        = "MFAKE" + strings.Repeat("M", 8)
	SquareLocationID        = "LFAKE" + strings.Repeat("L", 8)
)

type SquareEvent struct {
	Type string
	body []byte
}

func (e SquareEvent) Body() []byte { return bytes.Clone(e.body) }

type SquareLink struct {
	ID, OrderID, URL, Note, RedirectURL string
	Reference                           string
	Amount                              int64
	Currency                            string
	State                               string
	PaymentID                           string
	Refunded                            int64
	Deleted                             bool
}

type squareIdem struct {
	request []byte
	answer  []byte
}

type Square struct {
	srv      *httptest.Server
	clock    Clock
	token    string
	merchant string
	location string
	pageSize int
	payer    CustomerDetails

	mu         sync.Mutex
	links      map[string]*SquareLink
	order      []string
	byOrd      map[string]string
	idem       map[string]squareIdem
	faults     []squareFault
	calls      int
	requests   []Request
	creates    [][]byte
	ordersDown bool
}

type squareFault struct {
	status int
	code   string
	after  bool
}

type SquareOption func(*Square)

func WithSquareClock(c Clock) SquareOption { return func(s *Square) { s.clock = c } }

func WithSquareToken(token string) SquareOption { return func(s *Square) { s.token = token } }

func WithSquarePageSize(n int) SquareOption { return func(s *Square) { s.pageSize = n } }

func NewSquare(t testing.TB, opts ...SquareOption) *Square {
	t.Helper()
	s := &Square{
		clock: realClock{}, token: SquareTestToken, merchant: SquareMerchantID, location: SquareLocationID,
		pageSize: 1000, payer: DefaultPayer(),
		links: map[string]*SquareLink{}, byOrd: map[string]string{}, idem: map[string]squareIdem{},
	}
	for _, o := range opts {
		o(s)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Square) Host() string { return strings.TrimPrefix(s.srv.URL, "http://") }

func (s *Square) Clock() Clock { return s.clock }

func (s *Square) Payer() CustomerDetails { return s.payer }

func (s *Square) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *Square) Links() []SquareLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SquareLink, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.links[id])
	}
	return out
}

func (s *Square) FailNext(status int, code string, afterExecuting bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, squareFault{status: status, code: code, after: afterExecuting})
}

func squareError(w http.ResponseWriter, status int, category, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{
		"category": category, "code": code, "detail": "an error of the fake Square",
	}}})
}

func isFalse(b *bool) bool { return b != nil && !*b }

func squareJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Square) FailOrderReads(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ordersDown = down
}

func (s *Square) Creates() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.creates...)
}

func (s *Square) URL() string { return s.srv.URL }

func (s *Square) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Square) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path})
	if r.Header.Get("Square-Version") != SquareAPIVersion {
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_SQUARE_VERSION_FORMAT")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		squareError(w, http.StatusUnauthorized, "AUTHENTICATION_ERROR", "UNAUTHORIZED")
		return
	}
	const links = "/v2/online-checkout/payment-links"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == links:
		s.createLink(w, r)
	case r.Method == http.MethodGet && r.URL.Path == links:
		s.listLinks(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, links+"/"):
		l, ok := s.links[strings.TrimPrefix(r.URL.Path, links+"/")]
		if !ok || l.Deleted {
			squareError(w, http.StatusNotFound, "INVALID_REQUEST_ERROR", "NOT_FOUND")
			return
		}
		squareJSON(w, map[string]any{"payment_link": s.linkJSON(l)})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/orders/") && s.ordersDown:
		squareError(w, http.StatusInternalServerError, "API_ERROR", "INTERNAL_SERVER_ERROR")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/orders/"):
		id, ok := s.byOrd[strings.TrimPrefix(r.URL.Path, "/v2/orders/")]
		if !ok {
			squareError(w, http.StatusNotFound, "INVALID_REQUEST_ERROR", "NOT_FOUND")
			return
		}
		squareJSON(w, map[string]any{"order": s.orderJSON(s.links[id])})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/merchants/me":
		squareJSON(w, map[string]any{"merchant": map[string]any{
			"id": s.merchant, "business_name": "Fake shop", "country": "US", "currency": "USD", "status": "ACTIVE",
			"main_location_id": s.location,
		}})
	default:
		squareError(w, http.StatusNotFound, "INVALID_REQUEST_ERROR", "NOT_FOUND")
	}
}

type squareCreate struct {
	IdempotencyKey string `json:"idempotency_key"`
	Description    string `json:"description"`
	Order          *struct {
		LocationID  string `json:"location_id"`
		ReferenceID string `json:"reference_id"`
		LineItems   []struct {
			Name           string `json:"name"`
			Quantity       string `json:"quantity"`
			BasePriceMoney *struct {
				Amount   int64  `json:"amount"`
				Currency string `json:"currency"`
			} `json:"base_price_money"`
		} `json:"line_items"`
	} `json:"order"`
	CheckoutOptions *struct {
		AllowTipping  *bool  `json:"allow_tipping"`
		EnableCoupon  *bool  `json:"enable_coupon"`
		EnableLoyalty *bool  `json:"enable_loyalty"`
		RedirectURL   string `json:"redirect_url"`
	} `json:"checkout_options"`
	PaymentNote string `json:"payment_note"`
}

func (s *Square) createLink(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || r.Header.Get("Content-Type") != "application/json" {
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "BAD_REQUEST")
		return
	}
	s.creates = append(s.creates, raw)
	var req squareCreate
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_VALUE")
		return
	}
	var fault *squareFault
	if len(s.faults) > 0 {
		f := s.faults[0]
		s.faults = s.faults[1:]
		if !f.after {
			squareError(w, f.status, "API_ERROR", f.code)
			return
		}
		fault = &f
	}
	if req.IdempotencyKey != "" {
		if prev, ok := s.idem[req.IdempotencyKey]; ok {
			if !bytes.Equal(prev.request, raw) {
				squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "IDEMPOTENCY_KEY_REUSED")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prev.answer)
			return
		}
	}
	o := req.Order
	switch {
	case len(req.IdempotencyKey) > 192, o == nil, len(o.ReferenceID) > 40, len(req.PaymentNote) > 500, len(o.LineItems) != 1:
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_VALUE")
		return
	case o.LocationID != s.location:
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_LOCATION")
		return
	}
	if co := req.CheckoutOptions; co == nil || !isFalse(co.AllowTipping) || !isFalse(co.EnableCoupon) || !isFalse(co.EnableLoyalty) {
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_VALUE")
		return
	}
	li := o.LineItems[0]
	if li.Quantity != "1" || li.Name == "" || len(li.Name) > 512 || li.BasePriceMoney == nil || li.BasePriceMoney.Amount <= 0 ||
		len(li.BasePriceMoney.Currency) != 3 || (req.CheckoutOptions != nil && len(req.CheckoutOptions.RedirectURL) > 2048) {
		squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_VALUE")
		return
	}
	l := &SquareLink{
		ID: strings.ToUpper(randomID(16)), OrderID: randomID(29), URL: "https://sandbox.square.link/u/" + randomID(8),
		Note: req.PaymentNote, Reference: o.ReferenceID, Amount: li.BasePriceMoney.Amount, Currency: li.BasePriceMoney.Currency, State: "DRAFT",
	}
	l.RedirectURL = req.CheckoutOptions.RedirectURL
	s.links[l.ID] = l
	s.byOrd[l.OrderID] = l.ID
	s.order = append(s.order, l.ID)
	answer, _ := json.Marshal(map[string]any{"payment_link": s.linkJSON(l), "related_resources": map[string]any{"orders": []any{s.orderJSON(l)}}})
	if req.IdempotencyKey != "" {
		s.idem[req.IdempotencyKey] = squareIdem{request: raw, answer: answer}
	}
	if fault != nil {
		squareError(w, fault.status, "API_ERROR", fault.code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(answer)
}

func (s *Square) linkJSON(l *SquareLink) map[string]any {
	out := map[string]any{
		"id": l.ID, "version": 1, "order_id": l.OrderID, "url": l.URL,
		"long_url":   "https://sandbox.square.link/u/" + l.ID,
		"created_at": s.clock.Now().UTC().Format(time.RFC3339),
	}
	if l.Note != "" {
		out["payment_note"] = l.Note
	}
	opts := map[string]any{"allow_tipping": false}
	if l.RedirectURL != "" {
		opts["redirect_url"] = l.RedirectURL
	}
	out["checkout_options"] = opts
	return out
}

func (s *Square) orderJSON(l *SquareLink) map[string]any {
	out := map[string]any{
		"id": l.OrderID, "location_id": s.location, "state": l.State, "version": 1,
		"line_items":  []any{map[string]any{"quantity": "1", "base_price_money": money(l.Amount, l.Currency)}},
		"total_money": money(l.Amount, l.Currency),
	}
	if l.Reference != "" {
		out["reference_id"] = l.Reference
	}
	if l.PaymentID != "" {
		out["tenders"] = []any{map[string]any{"id": l.PaymentID, "type": "CARD", "amount_money": money(l.Amount, l.Currency)}}
	}
	return out
}

func money(amount int64, currency string) map[string]any {
	return map[string]any{"amount": amount, "currency": currency}
}

func (s *Square) listLinks(w http.ResponseWriter, r *http.Request) {
	limit := s.pageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 && n < limit {
			limit = n
		}
	}
	start := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(c, "c"))
		if err != nil || !strings.HasPrefix(c, "c") || n < 0 || n > len(s.order) {
			squareError(w, http.StatusBadRequest, "INVALID_REQUEST_ERROR", "INVALID_CURSOR")
			return
		}
		start = n
	}
	var live []string
	for _, id := range s.order {
		if !s.links[id].Deleted {
			live = append(live, id)
		}
	}
	if start > len(live) {
		start = len(live)
	}
	end := min(start+limit, len(live))
	out := map[string]any{}
	if end > start {
		page := []any{}
		for _, id := range live[start:end] {
			page = append(page, s.linkJSON(s.links[id]))
		}
		out["payment_links"] = page
	}
	if end < len(live) {
		out["cursor"] = "c" + strconv.Itoa(end)
	}
	squareJSON(w, out)
}

func (s *Square) PaySquare(linkID string) ([]SquareEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[linkID]
	switch {
	case !ok || l.Deleted:
		return nil, fmt.Errorf("testprovider: no payment link %q", linkID)
	case l.PaymentID != "":
		return nil, fmt.Errorf("testprovider: the payment link %q was paid already: a link is paid once", linkID)
	}
	l.PaymentID, l.State = randomID(29), "OPEN"
	return []SquareEvent{
		s.event("payment.created", "payment", l.PaymentID, map[string]any{"payment": s.paymentJSON(l, "APPROVED")}),
		s.event("payment.updated", "payment", l.PaymentID, map[string]any{"payment": s.paymentJSON(l, "COMPLETED")}),
		s.event("order.updated", "order", l.OrderID, map[string]any{"order_updated": map[string]any{
			"order_id": l.OrderID, "location_id": s.location, "state": l.State, "version": 2,
		}}),
	}, nil
}

func (s *Square) RefundSquare(linkID string, minor int64) ([]SquareEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[linkID]
	if !ok || l.PaymentID == "" {
		return nil, fmt.Errorf("testprovider: no payment of the link %q to refund", linkID)
	}
	if minor == 0 {
		minor = l.Amount - l.Refunded
	}
	if minor <= 0 || l.Refunded+minor > l.Amount {
		return nil, fmt.Errorf("testprovider: refunding %d of %d (%d refunded)", minor, l.Amount, l.Refunded)
	}
	l.Refunded += minor
	refundID := l.PaymentID + "_" + randomID(10)
	refund := func(status string) map[string]any {
		return map[string]any{"refund": map[string]any{
			"id": refundID, "status": status, "location_id": s.location, "payment_id": l.PaymentID, "order_id": l.OrderID,
			"amount_money": money(minor, l.Currency), "reason": "Requested by " + s.payer.Name,
		}}
	}
	return []SquareEvent{
		s.event("refund.created", "refund", refundID, refund("PENDING")),
		s.event("refund.updated", "refund", refundID, refund("COMPLETED")),
		s.event("payment.updated", "payment", l.PaymentID, map[string]any{"payment": s.paymentJSON(l, "COMPLETED")}),
	}, nil
}

func (s *Square) DeleteLink(linkID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[linkID]; ok {
		l.Deleted, l.State = true, "CANCELED"
	}
}

func (s *Square) paymentJSON(l *SquareLink, status string) map[string]any {
	now := s.clock.Now().UTC().Format(time.RFC3339Nano)
	first, last, _ := strings.Cut(s.payer.Name, " ")
	p := map[string]any{
		"id": l.PaymentID, "created_at": now, "updated_at": now, "status": status,
		"amount_money": money(l.Amount, l.Currency), "total_money": money(l.Amount, l.Currency),
		"approved_money": money(l.Amount, l.Currency), "source_type": "CARD",
		"location_id": s.location, "order_id": l.OrderID, "buyer_email_address": s.payer.Email,
		"billing_address": map[string]any{"first_name": first, "last_name": last},
		"card_details": map[string]any{"status": "CAPTURED", "card": map[string]any{
			"card_brand": "VISA", "last_4": "1111", "cardholder_name": s.payer.Name,
		}},
		"receipt_url":   "https://squareupsandbox.com/receipt/preview/" + l.PaymentID,
		"version_token": randomID(20),
	}
	if l.Note != "" {
		p["note"] = l.Note
	}
	if l.Refunded > 0 {
		p["refunded_money"] = money(l.Refunded, l.Currency)
	}
	return p
}

func (s *Square) event(typ, objType, id string, object map[string]any) SquareEvent {
	body, _ := json.Marshal(map[string]any{
		"merchant_id": s.merchant, "type": typ, "event_id": uuid(),
		"created_at": s.clock.Now().UTC().Format(time.RFC3339Nano),
		"data":       map[string]any{"type": objType, "id": id, "object": object},
	})
	return SquareEvent{Type: typ, body: body}
}

func NewSquareEvent(typ, objType, id string, object map[string]any, at time.Time) SquareEvent {
	body, _ := json.Marshal(map[string]any{
		"merchant_id": SquareMerchantID, "type": typ, "event_id": uuid(),
		"created_at": at.UTC().Format(time.RFC3339Nano),
		"data":       map[string]any{"type": objType, "id": id, "object": object},
	})
	return SquareEvent{Type: typ, body: body}
}

func RestampSquare(body []byte, at time.Time) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["created_at"] = at.UTC().Format(time.RFC3339Nano)
	out, _ := json.Marshal(m)
	return out
}

func SignSquare(key, notificationURL string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(notificationURL))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func uuid() string {
	const hexdigits = "0123456789abcdef"
	r := randomID(32)
	b := make([]byte, 0, 36)
	for i := range 32 {
		if i == 8 || i == 12 || i == 16 || i == 20 {
			b = append(b, '-')
		}
		b = append(b, hexdigits[int(r[i])%16])
	}
	return string(b)
}
