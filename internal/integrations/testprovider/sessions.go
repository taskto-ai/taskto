package testprovider

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type session struct {
	Session
	description string
	piMetadata  map[string]string
	piID        string
	chargeID    string
	receiptURL  string
	refunded    int64
	attempts    int
	delayed     bool
}

func ptr[T any](v T) *T { return &v }

func optionalBool(form url.Values, name string) any {
	if v, ok := form[name]; ok {
		return v[len(v)-1] == "true"
	}
	return nil
}

var submitTypes = []string{"auto", "book", "donate", "pay"}

func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return ptr(*p)
}

func (ss *session) snapshot() Session {
	out := ss.Session
	out.CancelURL, out.ClientReferenceID, out.Customer = copyOf(out.CancelURL), copyOf(out.ClientReferenceID), copyOf(out.Customer)
	out.CustomerDetails, out.CustomerEmail, out.Invoice = copyOf(out.CustomerDetails), copyOf(out.CustomerEmail), copyOf(out.Invoice)
	out.Locale, out.PaymentIntent, out.PaymentLink = copyOf(out.Locale), copyOf(out.PaymentIntent), copyOf(out.PaymentLink)
	out.RecoveredFrom, out.ReturnURL, out.SetupIntent = copyOf(out.RecoveredFrom), copyOf(out.ReturnURL), copyOf(out.SetupIntent)
	out.SubmitType, out.Subscription, out.SuccessURL = copyOf(out.SubmitType), copyOf(out.Subscription), copyOf(out.SuccessURL)
	out.URL = copyOf(out.URL)
	out.CustomFields = slices.Clone(out.CustomFields)
	out.PaymentMethodTypes = slices.Clone(out.PaymentMethodTypes)
	out.Metadata = maps.Clone(ss.Metadata)
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	return out
}

func (s *Stripe) createSession(r *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, sessionParamRes); rp != nil {
		return *rp
	}
	mode := form.Get("mode")
	if mode == "" {
		return *missing("mode")
	}
	if mode != "payment" {
		return invalid("", "The fake supports only mode=payment: the integration takes one-time payments.", "mode")
	}
	if ui := form.Get("ui_mode"); ui != "" && ui != "hosted_page" {
		return invalid("", "The fake supports only the hosted page (ui_mode=hosted_page).", "ui_mode")
	}
	success := form.Get("success_url")
	if success == "" {
		return *missing("success_url")
	}
	for _, p := range []string{"success_url", "cancel_url"} {
		if v := form.Get(p); v != "" && !validURL(v) {
			return invalid("url_invalid", "Not a valid URL", p)
		}
	}
	if v := form.Get("submit_type"); v != "" && !slices.Contains(submitTypes, v) {
		return invalid("", "Invalid submit_type: must be one of "+strings.Join(submitTypes, ", ")+".", "submit_type")
	}
	if rp := boolParam(form, "allow_promotion_codes"); rp != nil {
		return *rp
	}
	items, currency, rp := parseLineItems(form, maxSessionLines)
	if rp != nil {
		return *rp
	}
	sum, rp := total(items, currency)
	if rp != nil {
		return *rp
	}
	if ref := form.Get("client_reference_id"); utf8.RuneCountInString(ref) > 200 {
		return invalid("string_too_long", "Invalid string: client_reference_id must be at most 200 characters.", "client_reference_id")
	}
	meta, rp := metadataOf(form, "metadata")
	if rp != nil {
		return *rp
	}
	piMeta, rp := metadataOf(form, "payment_intent_data[metadata]")
	if rp != nil {
		return *rp
	}

	now := s.clock.Now()
	expires := now.Add(24 * time.Hour)
	if v := form.Get("expires_at"); v != "" {
		at, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return *badInteger("expires_at", v)
		}
		if at < now.Add(30*time.Minute).Unix() || at > now.Add(24*time.Hour).Unix() {
			return invalid("", "The `expires_at` timestamp must be between 30 minutes and 24 hours after the Checkout Session is created.", "expires_at")
		}
		expires = time.Unix(at, 0)
	}

	id := "cs_" + s.livemodeWord() + "_" + randomID(58)
	ss := &session{
		description: form.Get("payment_intent_data[description]"),
		piMetadata:  piMeta,
		Session: Session{
			ID: id, Object: "checkout.session",
			AmountSubtotal: sum, AmountTotal: sum,
			CancelURL: optional(form.Get("cancel_url")), ClientReferenceID: optional(form.Get("client_reference_id")),
			Created: now.Unix(), Currency: currency, CustomFields: []any{},
			CustomerCreation: "if_required", CustomerEmail: optional(form.Get("customer_email")),
			AllowPromotionCodes: optionalBool(form, "allow_promotion_codes"),
			ExpiresAt:           expires.Unix(), Livemode: s.live, Locale: optional(form.Get("locale")),
			Metadata: meta, Mode: "payment", PaymentMethodCollection: "always",
			PaymentMethodTypes: []string{"card"}, PaymentStatus: PaymentUnpaid, Status: SessionOpen,
			SubmitType: optional(form.Get("submit_type")), SuccessURL: &success,
			UIMode: "hosted_page",
			URL:    ptr("https://checkout.stripe.com/c/pay/" + id + "#fidkdWxOYHwnPyd1blpxYHZxWjA0"),
		},
	}
	s.sessions[id] = ss
	s.sessionOrder = append(s.sessionOrder, id)
	out := jsonReply(http.StatusOK, ss.snapshot())
	out.executed = true
	return out
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (s *Stripe) retrieveSession(_ *http.Request, form url.Values, id string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	ss, ok := s.sessions[id]
	if !ok {
		return noSuch("checkout.session", id, "session")
	}
	return jsonReply(http.StatusOK, ss.snapshot())
}

func noSuch(kind, id, param string) reply {
	return errorReply(http.StatusNotFound, "invalid_request_error", "resource_missing",
		"No such "+kind+": '"+id+"'", param)
}

func (s *Stripe) expireSession(_ *http.Request, form url.Values, id string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	ss, ok := s.sessions[id]
	if !ok {
		return noSuch("checkout.session", id, "session")
	}
	if ss.Status != SessionOpen {
		return invalid("", `Only Checkout Sessions with a status in ["open"] can be expired.`, "")
	}
	s.expireLocked(ss, s.clock.Now())
	out := jsonReply(http.StatusOK, ss.snapshot())
	out.executed = true
	return out
}

func (s *Stripe) expireLocked(ss *session, at time.Time) {
	ss.Status = SessionExpired
	ss.URL = nil
	s.emit(EventSessionExpired, ss.snapshot(), at)
}

func (s *Stripe) expireDue() { s.expireDueAt(s.clock.Now()) }

func (s *Stripe) expireDueAt(now time.Time) {
	for _, id := range s.sessionOrder {
		ss := s.sessions[id]
		if ss.Status == SessionOpen && ss.ExpiresAt <= now.Unix() {
			s.expireLocked(ss, time.Unix(ss.ExpiresAt, 0))
		}
	}
}

var sessionListParams = compileAll([]string{`limit`, `starting_after`, `ending_before`, `payment_link`, `status`, `payment_intent`,
	`created`, `created\[(gt|gte|lt|lte)\]`})

func (s *Stripe) listSessions(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, sessionListParams); rp != nil {
		return *rp
	}
	created, rp := createdFilter(form)
	if rp != nil {
		return *rp
	}
	var ids []string
	for i := len(s.sessionOrder) - 1; i >= 0; i-- {
		ss := s.sessions[s.sessionOrder[i]]
		if !created(ss.Created) {
			continue
		}
		if v := form.Get("payment_link"); v != "" && (ss.PaymentLink == nil || *ss.PaymentLink != v) {
			continue
		}
		if v := form.Get("status"); v != "" && ss.Status != v {
			continue
		}
		if v := form.Get("payment_intent"); v != "" && (ss.PaymentIntent == nil || *ss.PaymentIntent != v) {
			continue
		}
		ids = append(ids, ss.ID)
	}
	page, more, rp := paginate(ids, form, "checkout.session")
	if rp != nil {
		return *rp
	}
	data := make([]Session, 0, len(page))
	for _, id := range page {
		data = append(data, s.sessions[id].snapshot())
	}
	return jsonReply(http.StatusOK, listOf("/v1/checkout/sessions", more, data))
}

func createdFilter(form url.Values) (func(int64) bool, *reply) {
	type bound struct {
		param string
		ok    func(created, bound int64) bool
	}
	var checks []func(int64) bool
	bounds := []bound{
		{"created[gt]", func(c, b int64) bool { return c > b }},
		{"created[gte]", func(c, b int64) bool { return c >= b }},
		{"created[lt]", func(c, b int64) bool { return c < b }},
		{"created[lte]", func(c, b int64) bool { return c <= b }},
		{"created", func(c, b int64) bool { return c == b }},
	}
	for _, b := range bounds {
		v := form.Get(b.param)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, badInteger(b.param, v)
		}
		checks = append(checks, func(c int64) bool { return b.ok(c, n) })
	}
	return func(c int64) bool {
		for _, check := range checks {
			if !check(c) {
				return false
			}
		}
		return true
	}, nil
}

func listOf(path string, more bool, data any) map[string]any {
	return map[string]any{"object": "list", "url": path, "has_more": more, "data": data}
}

func paginate(ids []string, form url.Values, kind string) ([]string, bool, *reply) {
	limit := 10
	if v := form.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, false, badInteger("limit", v)
		}
		if n < 1 || n > 100 {
			rp := invalid("", "Invalid limit: it must be between 1 and 100.", "limit")
			return nil, false, &rp
		}
		limit = n
	}
	index := func(param string) (int, *reply) {
		id := form.Get(param)
		for i, x := range ids {
			if x == id {
				return i, nil
			}
		}
		rp := invalid("resource_missing", "No such "+kind+": '"+id+"'", param)
		return 0, &rp
	}
	if form.Get("starting_after") != "" && form.Get("ending_before") != "" {
		rp := invalid("", "You may pass one of `starting_after` and `ending_before`, not both.", "")
		return nil, false, &rp
	}
	if form.Get("starting_after") != "" {
		i, rp := index("starting_after")
		if rp != nil {
			return nil, false, rp
		}
		ids = ids[i+1:]
	}
	if form.Get("ending_before") != "" {
		i, rp := index("ending_before")
		if rp != nil {
			return nil, false, rp
		}
		ids = ids[:i]
		if len(ids) > limit {
			return ids[len(ids)-limit:], true, nil
		}
		return ids, false, nil
	}
	if len(ids) > limit {
		return ids[:limit], true, nil
	}
	return ids, false, nil
}
