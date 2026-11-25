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
}

// linkHosts is the host a link of a submit_type lives on.
var linkHosts = map[string]string{"auto": "buy", "pay": "buy", "donate": "donate", "book": "book"}

func (l *paymentLink) snapshot() PaymentLink {
	out := l.PaymentLink
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
		Livemode: s.live, Metadata: meta, SubmitType: submit, URL: host + randomID(14),
	}}
	s.links[id] = l
	rp2 := jsonReply(http.StatusOK, l.snapshot())
	rp2.executed = true
	return rp2
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

// updateLink changes a link's active flag (the way to stop a link taking payments) and metadata.
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
	l.Active, l.Metadata = active, meta
	rp := jsonReply(http.StatusOK, l.snapshot())
	rp.executed = true
	return rp
}
