package testprovider

import (
	"encoding/json"
	"net/http"
	"slices"
)

type Session struct {
	ID                      string            `json:"id"`
	Object                  string            `json:"object"`
	AfterExpiration         any               `json:"after_expiration"`
	AllowPromotionCodes     any               `json:"allow_promotion_codes"`
	AmountSubtotal          int64             `json:"amount_subtotal"`
	AmountTotal             int64             `json:"amount_total"`
	BillingAddressCollect   any               `json:"billing_address_collection"`
	CancelURL               *string           `json:"cancel_url"`
	ClientReferenceID       *string           `json:"client_reference_id"`
	Created                 int64             `json:"created"`
	Currency                string            `json:"currency"`
	CustomFields            []any             `json:"custom_fields"`
	Customer                *string           `json:"customer"`
	CustomerCreation        string            `json:"customer_creation"`
	CustomerDetails         *CustomerDetails  `json:"customer_details"`
	CustomerEmail           *string           `json:"customer_email"`
	ExpiresAt               int64             `json:"expires_at"`
	Invoice                 *string           `json:"invoice"`
	Livemode                bool              `json:"livemode"`
	Locale                  *string           `json:"locale"`
	Metadata                map[string]string `json:"metadata"`
	Mode                    string            `json:"mode"`
	PaymentIntent           *string           `json:"payment_intent"`
	PaymentLink             *string           `json:"payment_link"`
	PaymentMethodCollection string            `json:"payment_method_collection"`
	PaymentMethodTypes      []string          `json:"payment_method_types"`
	PaymentStatus           string            `json:"payment_status"`
	RecoveredFrom           *string           `json:"recovered_from"`
	ReturnURL               *string           `json:"return_url"`
	SetupIntent             *string           `json:"setup_intent"`
	Status                  string            `json:"status"`
	SubmitType              *string           `json:"submit_type"`
	Subscription            *string           `json:"subscription"`
	SuccessURL              *string           `json:"success_url"`
	TotalDetails            TotalDetails      `json:"total_details"`
	UIMode                  string            `json:"ui_mode"`
	URL                     *string           `json:"url"`
}

const (
	SessionOpen     = "open"
	SessionComplete = "complete"
	SessionExpired  = "expired"

	PaymentUnpaid = "unpaid"
	PaymentPaid   = "paid"
)

type CustomerDetails struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type TotalDetails struct {
	AmountDiscount int64 `json:"amount_discount"`
	AmountShipping int64 `json:"amount_shipping"`
	AmountTax      int64 `json:"amount_tax"`
}

type PaymentLink struct {
	ID              string            `json:"id"`
	Object          string            `json:"object"`
	Active          bool              `json:"active"`
	AfterCompletion map[string]any    `json:"after_completion"`
	Currency        string            `json:"currency"`
	Livemode        bool              `json:"livemode"`
	Metadata        map[string]string `json:"metadata"`
	Restrictions    *LinkRestrictions `json:"restrictions"`
	SubmitType      string            `json:"submit_type"`
	URL             string            `json:"url"`
}

type LinkRestrictions struct {
	CompletedSessions CompletedSessions `json:"completed_sessions"`
}

type CompletedSessions struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

type Event struct {
	ID              string       `json:"id"`
	Object          string       `json:"object"`
	APIVersion      string       `json:"api_version"`
	Created         int64        `json:"created"`
	Data            EventData    `json:"data"`
	Livemode        bool         `json:"livemode"`
	PendingWebhooks int          `json:"pending_webhooks"`
	Request         EventRequest `json:"request"`
	Type            string       `json:"type"`
}

type EventData struct {
	Object json.RawMessage `json:"object"`
}

type EventRequest struct {
	ID             *string `json:"id"`
	IdempotencyKey *string `json:"idempotency_key"`
}

func (e Event) clone() Event {
	e.Data.Object = slices.Clone(e.Data.Object)
	return e
}

func (e Event) Body() []byte {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		panic("testprovider: event doesn't marshal: " + err.Error())
	}
	return b
}

func (e Event) ObjectAs(v any) error { return json.Unmarshal(e.Data.Object, v) }

const (
	EventSessionCompleted             = "checkout.session.completed"
	EventSessionAsyncPaymentSucceeded = "checkout.session.async_payment_succeeded"
	EventSessionAsyncPaymentFailed    = "checkout.session.async_payment_failed"
	EventSessionExpired               = "checkout.session.expired"
	EventIntentCreated                = "payment_intent.created"
	EventIntentProcessing             = "payment_intent.processing"
	EventIntentSucceeded              = "payment_intent.succeeded"
	EventIntentFailed                 = "payment_intent.payment_failed"
	EventChargeSucceeded              = "charge.succeeded"
	EventChargeFailed                 = "charge.failed"
	EventChargeUpdated                = "charge.updated"
	EventChargeRefunded               = "charge.refunded"
	EventRefundCreated                = "refund.created"
)

type reply struct {
	status     int
	body       []byte
	header     http.Header
	drop, hang bool
	executed   bool
	replayed   bool
}

func (r reply) clone() reply {
	r.header = r.header.Clone()
	r.body = append([]byte(nil), r.body...)
	return r
}

func jsonReply(status int, v any) reply {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("testprovider: reply doesn't marshal: " + err.Error())
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return reply{status: status, body: b, header: h}
}

type errorObject struct {
	Code          *string `json:"code,omitempty"`
	DocURL        string  `json:"doc_url,omitempty"`
	Message       string  `json:"message"`
	Param         *string `json:"param,omitempty"`
	RequestLogURL string  `json:"request_log_url,omitempty"`
	Type          string  `json:"type"`
}

func errorReply(status int, typ, code, message, param string) reply {
	e := errorObject{Message: message, Type: typ}
	if code != "" {
		e.Code = &code
		e.DocURL = "https://stripe.com/docs/error-codes/" + code
	}
	if param != "" {
		e.Param = &param
	}
	return jsonReply(status, map[string]any{"error": e})
}

func invalid(code, message, param string) reply {
	return errorReply(http.StatusBadRequest, "invalid_request_error", code, message, param)
}
