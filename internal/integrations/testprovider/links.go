package testprovider

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type paymentLink struct {
	PaymentLink
	items []lineItem
	// completed is how many Checkout Sessions made through the link have completed, whether or
	// not it has a limit (yet): the count a limit is measured against.
	completed int
}

// linkHosts is the host a link of a submit_type lives on.
var linkHosts = map[string]string{"auto": "buy", "pay": "buy", "donate": "donate", "book": "book"}

// limitReached is true when the link has taken as many completed sessions as its limit allows.
func (l *paymentLink) limitReached() bool {
	r := l.Restrictions
	return r != nil && l.completed >= r.CompletedSessions.Limit
}

func (l *paymentLink) snapshot() PaymentLink {
	out := l.PaymentLink
	out.Restrictions = copyOf(l.Restrictions)
	if out.Restrictions != nil {
		out.Restrictions.CompletedSessions.Count = l.completed
	}
	out.Metadata = maps.Clone(l.Metadata)
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	return out
}

func (s *Stripe) createLink(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, linkParamRes); rp != nil {
		return *rp
	}
	if v := form.Get("submit_type"); v != "" && !slices.Contains(submitTypes, v) {
		return invalid("", "Invalid submit_type: must be one of "+strings.Join(submitTypes, ", ")+".", "submit_type")
	}
	if rp := boolParam(form, "allow_promotion_codes"); rp != nil {
		return *rp
	}
	items, currency, rp := parseLineItems(form, maxLinkLines)
	if rp != nil {
		return *rp
	}
	if _, rp := total(items, currency); rp != nil {
		return *rp
	}
	if c := form.Get("currency"); c != "" && c != currency {
		return invalid("", "The currency of the link must be the currency of its line items.", "currency")
	}
	meta, rp := metadataOf(form, "metadata")
	if rp != nil {
		return *rp
	}
	after := map[string]any{"type": "hosted_confirmation", "hosted_confirmation": map[string]any{"custom_message": nil}}
	switch t := form.Get("after_completion[type]"); t {
	case "", "hosted_confirmation":
		if msg := form.Get("after_completion[hosted_confirmation][custom_message]"); msg != "" {
			after = map[string]any{"type": "hosted_confirmation", "hosted_confirmation": map[string]any{"custom_message": msg}}
		}
	case "redirect":
		u := form.Get("after_completion[redirect][url]")
		if u == "" {
			return *missing("after_completion[redirect][url]")
		}
		if !validURL(u) {
			return invalid("url_invalid", "Not a valid URL", "after_completion[redirect][url]")
		}
		after = map[string]any{"type": "redirect", "redirect": map[string]any{"url": u}}
	default:
		return invalid("", "Invalid after_completion[type]: "+t, "after_completion[type]")
	}
	var restrictions *LinkRestrictions
	limit, hasLimit, rp := completedSessionsLimitOf(form)
	if rp != nil {
		return *rp
	}
	if hasLimit {
		restrictions = &LinkRestrictions{CompletedSessions: CompletedSessions{Limit: limit}}
	}
	submit := form.Get("submit_type")
	if submit == "" {
		submit = "auto"
	}
	id := "plink_" + randomID(24)
	host := "https://" + linkHosts[submit] + ".stripe.com/" // buy, donate or book
	if !s.live {
		host += "test_"
	}
	l := &paymentLink{items: items, PaymentLink: PaymentLink{
		ID: id, Object: "payment_link", Active: true, AfterCompletion: after, Currency: currency,
		Livemode: s.live, Metadata: meta, Restrictions: restrictions, SubmitType: submit, URL: host + randomID(14),
	}}
	s.links[id] = l
	s.linkOrder = append(s.linkOrder, id)
	rp2 := jsonReply(http.StatusOK, l.snapshot())
	rp2.executed = true
	return rp2
}

var linkListParams = compileAll([]string{`limit`, `starting_after`, `ending_before`, `active`})

// listLinks lists the payment links, newest first. Like Stripe's, it has no filter on metadata:
// a caller looking for a link's metadata reads the pages.
func (s *Stripe) listLinks(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, linkListParams); rp != nil {
		return *rp
	}
	active := ""
	if v, ok := form["active"]; ok {
		if v[len(v)-1] != "true" && v[len(v)-1] != "false" {
			return invalid("", "Invalid boolean: "+v[len(v)-1], "active")
		}
		active = v[len(v)-1]
	}
	var ids []string
	for i := len(s.linkOrder) - 1; i >= 0; i-- { // newest first
		l := s.links[s.linkOrder[i]]
		if active != "" && strconv.FormatBool(l.Active) != active {
			continue
		}
		ids = append(ids, l.ID)
	}
	page, more, rp := paginate(ids, form, "payment link")
	if rp != nil {
		return *rp
	}
	data := make([]PaymentLink, 0, len(page))
	for _, id := range page {
		data = append(data, s.links[id].snapshot())
	}
	return jsonReply(http.StatusOK, listOf("/v1/payment_links", more, data))
}

func (s *Stripe) retrieveLink(_ *http.Request, form url.Values, id string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	l, ok := s.links[id]
	if !ok {
		return noSuch("payment link", id, "payment_link")
	}
	return jsonReply(http.StatusOK, l.snapshot())
}

// updateLink changes a link's active flag (the way to stop a link taking payments), its limit of
// completed sessions and its metadata. A new limit that the link has already reached
// deactivates it, as reaching the limit does.
func (s *Stripe) updateLink(_ *http.Request, form url.Values, id string) reply {
	if rp := unknownParam(form, linkUpdateParamRes); rp != nil {
		return *rp
	}
	l, ok := s.links[id]
	if !ok {
		return noSuch("payment link", id, "payment_link")
	}
	// Everything is checked before anything changes: a refused update leaves the link as it was.
	active := l.Active
	if v := form.Get("active"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil || (v != "true" && v != "false") {
			return invalid("", "Invalid boolean: "+v, "active")
		}
		active = b
	}
	limit, hasLimit, rp := completedSessionsLimitOf(form)
	if rp != nil {
		return *rp
	}
	meta := maps.Clone(l.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	if _, ok := form["metadata"]; ok {
		if form.Get("metadata") != "" {
			return invalid("", "Invalid metadata: pass an empty string to clear it, or set its keys one by one.", "metadata")
		}
		clear(meta) // `metadata=` empties it; keys passed beside it are then set
	}
	if rp := mergeMetadata(meta, form, "metadata"); rp != nil {
		return *rp
	}
	restrictions := copyOf(l.Restrictions)
	if hasLimit {
		if restrictions == nil {
			restrictions = &LinkRestrictions{}
		}
		restrictions.CompletedSessions.Limit = limit
		if l.completed >= limit {
			active = false
		}
	}
	l.Active, l.Metadata, l.Restrictions = active, meta, restrictions
	out := jsonReply(http.StatusOK, l.snapshot())
	out.executed = true
	return out
}

// Link returns the payment link as the API would show it now.
func (s *Stripe) Link(id string) (PaymentLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[id]
	if !ok {
		return PaymentLink{}, false
	}
	return l.snapshot(), true
}

// Links returns every payment link, sorted by id.
func (s *Stripe) Links() []PaymentLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PaymentLink, 0, len(s.links))
	for _, id := range slices.Sorted(maps.Keys(s.links)) {
		out = append(out, s.links[id].snapshot())
	}
	return out
}
