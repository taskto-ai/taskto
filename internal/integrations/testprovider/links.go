package testprovider

import (
	"maps"
	"net/http"
	"net/url"
	"strconv"
)

type paymentLink struct {
	PaymentLink
	items []lineItem
}

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
	items, currency, rp := parseLineItems(form)
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
	host := "https://buy.stripe.com/"
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
	if v := form.Get("active"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil || (v != "true" && v != "false") {
			return invalid("", "Invalid boolean: "+v, "active")
		}
		l.Active = b
	}
	for k, v := range form {
		if len(k) > len("metadata[]") && k[:9] == "metadata[" {
			key := k[9 : len(k)-1]
			if v[len(v)-1] == "" {
				delete(l.Metadata, key)
			} else {
				if l.Metadata == nil {
					l.Metadata = map[string]string{}
				}
				l.Metadata[key] = v[len(v)-1]
			}
		}
	}
	rp := jsonReply(http.StatusOK, l.snapshot())
	rp.executed = true
	return rp
}
