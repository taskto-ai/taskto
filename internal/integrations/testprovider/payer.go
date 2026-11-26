package testprovider

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"
)

// What a payer does on Stripe's page, and what follows. The methods move a session through its
// states and make the events Stripe sends for one payment, in the order it sends them. Stripe
// sends several events for one card payment: a payment_intent.created, a charge.succeeded, a
// payment_intent.succeeded, a charge.updated and the checkout.session.completed (Stripe doesn't
// promise their order). A test gives the integration all of them and checks that the payment is
// applied once.
//
// The intent, the charge and the refund are the minimum of their objects: what a reader of the
// events would use. Every method returns the events it made, which a Sender delivers.

// Payer-side errors.
var (
	ErrNoSuchSession = errors.New("testprovider: no such checkout session")
	ErrNotOpen       = errors.New("testprovider: the session isn't open")
	ErrNotPaid       = errors.New("testprovider: the session isn't paid")
	ErrOverRefund    = errors.New("testprovider: the refund is more than what is left")
	ErrNoSuchLink    = errors.New("testprovider: no such payment link")
	ErrLinkInactive  = errors.New("testprovider: the payment link isn't active")
)

// DefaultPayer is the person on Stripe's page unless a fake is given another (WithPayer,
// SetPayer): their details end up on the session. They are personal data the integration must
// never keep. Each call returns a value of its own, and each fake keeps its own copy, so a test
// that changes one fake's payer changes no other's.
func DefaultPayer() CustomerDetails {
	return CustomerDetails{Email: "payer@example.test", Name: "A. Payer"}
}

// WithPayer sets the details of the payer on this fake's sessions.
func WithPayer(d CustomerDetails) Option { return func(s *Stripe) { s.payer = d } }

// Payer returns the details the fake's payer gives.
func (s *Stripe) Payer() CustomerDetails {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.payer
}

// SetPayer changes the payer for the sessions paid from now on, on this fake only.
func (s *Stripe) SetPayer(d CustomerDetails) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payer = d
}

// openSession finds an open session, expiring those whose time has come. It returns the time it
// read from the clock, which the caller uses for what it makes: the decision that the session is
// open and the time of what follows are of one reading, so a clock moving meanwhile can't put a
// payment after its session's expiry.
func (s *Stripe) openSession(id string) (*session, time.Time, error) {
	ss, ok := s.sessions[id]
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	at := s.clock.Now()
	s.expireDueAt(at)
	if ss.Status != SessionOpen {
		return nil, time.Time{}, fmt.Errorf("%w: %s is %s", ErrNotOpen, id, ss.Status)
	}
	return ss, at, nil
}

func (s *Stripe) intentOf(ss *session, status string) map[string]any {
	return map[string]any{
		"id": ss.piID, "object": "payment_intent", "amount": ss.AmountTotal,
		"amount_received": map[bool]int64{true: ss.AmountTotal}[status == "succeeded"],
		"currency":        ss.Currency, "status": status, "livemode": s.live,
		"metadata": ss.piMetadata, "description": nullable(ss.description), "latest_charge": nullable(ss.chargeID),
		"created": ss.Created,
	}
}

func (s *Stripe) chargeOf(ss *session, status string) map[string]any {
	return map[string]any{
		"id": ss.chargeID, "object": "charge", "amount": ss.AmountTotal,
		"amount_captured": map[bool]int64{true: ss.AmountTotal}[status == "succeeded"],
		"amount_refunded": ss.refunded, "currency": ss.Currency, "paid": status == "succeeded",
		"refunded": ss.refunded >= ss.AmountTotal, "status": status, "payment_intent": ss.piID,
		"livemode": s.live, "metadata": ss.piMetadata,
		"receipt_url":     nullable(ss.receiptURL),
		"billing_details": map[string]any{"email": s.payer.Email, "name": s.payer.Name},
	}
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// begin gives the session its intent when a payment is attempted.
func (s *Stripe) begin(ss *session, at time.Time) []*Event {
	if ss.piID != "" {
		return nil
	}
	ss.piID = "pi_" + randomID(24)
	pi := ss.piID
	ss.PaymentIntent = &pi
	return []*Event{s.emit(EventIntentCreated, s.intentOf(ss, "requires_payment_method"), at)}
}

func values(evs []*Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = e.clone()
	}
	return out
}

// Pay is the payer paying with a card: the session completes and is paid at once. The events are
// those of a card payment, in the order a payer's success makes them.
func (s *Stripe) Pay(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, at, err := s.openSession(sessionID)
	if err != nil {
		return nil, err
	}
	return values(s.payLocked(ss, at)), nil
}

// payLocked pays an open session by card at time at, and returns the events. The caller holds s.mu.
func (s *Stripe) payLocked(ss *session, at time.Time) []*Event {
	evs := s.begin(ss, at)
	ss.chargeID = "ch_" + randomID(24)
	ss.receiptURL = "https://pay.stripe.com/receipts/payment/" + randomID(40)
	ss.Status, ss.PaymentStatus, ss.URL = SessionComplete, PaymentPaid, nil
	d := s.payer
	ss.CustomerDetails = &d
	return append(evs,
		s.emit(EventChargeSucceeded, s.chargeOf(ss, "succeeded"), at),
		s.emit(EventIntentSucceeded, s.intentOf(ss, "succeeded"), at),
		s.emit(EventChargeUpdated, s.chargeOf(ss, "succeeded"), at),
		s.emit(EventSessionCompleted, ss.snapshot(), at),
	)
}

// PayLink is a payer paying through a payment link with a card: a Checkout Session that carries
// the link's id is made and completes at once, with the events of a card payment. A link that
// isn't active, or has taken its limit of completed sessions (restrictions[completed_sessions]),
// takes no payment. The session that reaches the limit deactivates the link.
func (s *Stripe) PayLink(linkID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.payLinkLocked(linkID, "")
}

// PayLinkAt is a payer opening the address of a hosted payment link, as it was handed out, and paying
// with a card. The link is found by its address, and the address's client_reference_id parameter, if it
// has one, is copied into the Checkout Session the payer's visit makes, the way Stripe does
// (https://docs.stripe.com/payment-links/url-parameters): a value of letters, digits, dashes and
// underscores up to 200 characters, which Stripe takes and any other it leaves out.
func (s *Stripe) PayLinkAt(rawURL string) ([]Event, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoSuchLink, err)
	}
	ref := u.Query().Get("client_reference_id")
	if len(ref) > 200 || strings.Trim(ref, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		ref = ""
	}
	u.RawQuery, u.Fragment = "", ""
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.links {
		if l.URL == u.String() {
			return s.payLinkLocked(id, ref)
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSuchLink, u.String())
}

// payLinkLocked is PayLink for a payer whose session carries the reference ref ("" for none). The caller
// holds s.mu.
func (s *Stripe) payLinkLocked(linkID, ref string) ([]Event, error) {
	l, ok := s.links[linkID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchLink, linkID)
	}
	if !l.Active || l.limitReached() {
		return nil, fmt.Errorf("%w: %s", ErrLinkInactive, linkID)
	}
	at := s.clock.Now()
	var sum int64
	for _, it := range l.items {
		sum += it.UnitAmount * it.Quantity
	}
	id := "cs_" + s.livemodeWord() + "_" + randomID(58)
	link := linkID
	ss := &session{
		piMetadata: map[string]string{},
		Session: Session{
			ID: id, Object: "checkout.session", AmountSubtotal: sum, AmountTotal: sum,
			ClientReferenceID: optional(ref),
			Created:           at.Unix(), Currency: l.Currency, CustomFields: []any{}, CustomerCreation: "if_required",
			ExpiresAt: at.Add(24 * time.Hour).Unix(), Livemode: s.live, Metadata: maps.Clone(l.Metadata),
			Mode: "payment", PaymentLink: &link, PaymentMethodCollection: "always",
			PaymentMethodTypes: []string{"card"}, PaymentStatus: PaymentUnpaid, Status: SessionOpen,
			SubmitType: ptr(l.SubmitType), UIMode: "hosted_page",
		},
	}
	s.sessions[id] = ss
	s.sessionOrder = append(s.sessionOrder, id)
	evs := s.payLocked(ss, at)
	l.completed++
	if l.limitReached() {
		l.Active = false
	}

	return values(evs), nil
}

// PayDelayed is the payer choosing a payment method that settles later (a bank debit): the
// session completes but is still unpaid. SettleDelayed ends it.
func (s *Stripe) PayDelayed(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, at, err := s.openSession(sessionID)
	if err != nil {
		return nil, err
	}
	evs := s.begin(ss, at)
	ss.Status, ss.URL = SessionComplete, nil
	ss.delayed = true
	d := s.payer
	ss.CustomerDetails = &d
	evs = append(evs,
		s.emit(EventIntentProcessing, s.intentOf(ss, "processing"), at),
		s.emit(EventSessionCompleted, ss.snapshot(), at),
	)
	return values(evs), nil
}

// SettleDelayed ends a delayed payment: it succeeds (the session becomes paid) or fails.
func (s *Stripe) SettleDelayed(sessionID string, succeeded bool) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, sessionID)
	}
	if !ss.delayed {
		return nil, fmt.Errorf("%w: %s isn't waiting for a delayed payment", ErrNotOpen, sessionID)
	}
	ss.delayed = false // both outcomes end the wait; a failed payment stays complete and unpaid
	at := s.clock.Now()
	if !succeeded {
		return values([]*Event{
			s.emit(EventIntentFailed, s.intentOf(ss, "requires_payment_method"), at),
			s.emit(EventSessionAsyncPaymentFailed, ss.snapshot(), at),
		}), nil
	}
	ss.chargeID = "ch_" + randomID(24)
	ss.receiptURL = "https://pay.stripe.com/receipts/payment/" + randomID(40)
	ss.PaymentStatus = PaymentPaid
	return values([]*Event{
		s.emit(EventChargeSucceeded, s.chargeOf(ss, "succeeded"), at),
		s.emit(EventIntentSucceeded, s.intentOf(ss, "succeeded"), at),
		s.emit(EventSessionAsyncPaymentSucceeded, ss.snapshot(), at),
	}), nil
}

// Decline is a payer's attempt that the card refuses: the session stays open (the payer may try
// another card) and Stripe sends the failure events.
func (s *Stripe) Decline(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, at, err := s.openSession(sessionID)
	if err != nil {
		return nil, err
	}
	evs := s.begin(ss, at)
	ss.attempts++
	ss.chargeID = "ch_" + randomID(24) // the intent's latest charge, in the failure events too
	failed := s.chargeOf(ss, "failed")
	failed["failure_code"] = "card_declined"
	evs = append(evs,
		s.emit(EventChargeFailed, failed, at),
		s.emit(EventIntentFailed, s.intentOf(ss, "requires_payment_method"), at),
	)
	return values(evs), nil
}

// Expire expires an open session now, as Stripe does when its time comes (the clock does that
// too) and sends checkout.session.expired.
func (s *Stripe) Expire(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, at, err := s.openSession(sessionID)
	if err != nil {
		return nil, err
	}
	before := len(s.events)
	s.expireLocked(ss, at)
	return values(s.events[before:]), nil
}

// Refund refunds amount (all that is left when 0) of a paid session: Stripe sends refund.created
// and charge.refunded, and updates the charge.
func (s *Stripe) Refund(sessionID string, amount int64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, sessionID)
	}
	if ss.PaymentStatus != PaymentPaid {
		return nil, fmt.Errorf("%w: %s", ErrNotPaid, sessionID)
	}
	left := ss.AmountTotal - ss.refunded
	if amount == 0 {
		amount = left
	}
	if amount < 1 || amount > left {
		return nil, fmt.Errorf("%w: %d of %d", ErrOverRefund, amount, left)
	}
	ss.refunded += amount
	at := s.clock.Now()
	refund := map[string]any{
		"id": "re_" + randomID(24), "object": "refund", "amount": amount, "currency": ss.Currency,
		"charge": ss.chargeID, "payment_intent": ss.piID, "status": "succeeded", "created": at.Unix(),
	}
	return values([]*Event{
		s.emit(EventRefundCreated, refund, at),
		s.emit(EventChargeRefunded, s.chargeOf(ss, "succeeded"), at),
		s.emit(EventChargeUpdated, s.chargeOf(ss, "succeeded"), at),
	}), nil
}

// Session returns the session as the API would show it now.
func (s *Stripe) Session(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireDue()
	ss, ok := s.sessions[id]
	if !ok {
		return Session{}, false
	}
	return ss.snapshot(), true
}

// Sessions returns every session, oldest first.
func (s *Stripe) Sessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireDue()
	out := make([]Session, 0, len(s.sessionOrder))
	for _, id := range s.sessionOrder {
		out = append(out, s.sessions[id].snapshot())
	}
	return out
}
