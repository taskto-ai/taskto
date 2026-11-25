package testprovider

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Stripe's v1 API takes form-encoded bodies with bracket notation (line_items[0][quantity]=1)
// and refuses a parameter it doesn't know (parameter_unknown). The fake does the same, with the
// lists below: the parameters the integration may use. A real parameter that is missing from the
// lists is refused here, so add it (and a test) when a card starts to send one.
const metaKey = `[^\[\]]{1,200}`

var (
	lineItemParams = []string{
		`line_items\[\d+\]\[quantity\]`,
		`line_items\[\d+\]\[price\]`,
		`line_items\[\d+\]\[price_data\]\[currency\]`,
		`line_items\[\d+\]\[price_data\]\[unit_amount\]`,
		`line_items\[\d+\]\[price_data\]\[product_data\]\[(name|description)\]`,
		`line_items\[\d+\]\[price_data\]\[product_data\]\[metadata\]\[` + metaKey + `\]`,
		`line_items\[\d+\]\[metadata\]\[` + metaKey + `\]`,
	}

	sessionParamRes = compileAll(append([]string{
		`mode`, `success_url`, `cancel_url`, `client_reference_id`, `customer_email`, `expires_at`,
		`locale`, `submit_type`, `ui_mode`, `customer_creation`, `billing_address_collection`,
		`allow_promotion_codes`,
		`metadata\[` + metaKey + `\]`,
		`payment_intent_data\[(description|statement_descriptor|receipt_email)\]`,
		`payment_intent_data\[metadata\]\[` + metaKey + `\]`,
	}, lineItemParams...))

	linkParamRes = compileAll(append([]string{
		`currency`, `allow_promotion_codes`, `submit_type`, `inactive_message`,
		`after_completion\[type\]`, `after_completion\[redirect\]\[url\]`,
		`after_completion\[hosted_confirmation\]\[custom_message\]`,
		`metadata\[` + metaKey + `\]`,
		`payment_intent_data\[(description|statement_descriptor)\]`,
		`payment_intent_data\[metadata\]\[` + metaKey + `\]`,
	}, lineItemParams...))

	// `metadata` on its own, with an empty value, clears all of a link's metadata.
	linkUpdateParamRes = compileAll([]string{`active`, `inactive_message`, `metadata`, `metadata\[` + metaKey + `\]`})

	lineItemIndex = regexp.MustCompile(`^line_items\[(\d+)\]`)
)

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`^` + p + `$`)
	}
	return out
}

// unknownParam answers Stripe's parameter_unknown for the first (in sorted order) parameter of
// form that no pattern allows.
func unknownParam(form url.Values, allowed []*regexp.Regexp) *reply {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
next:
	for _, k := range keys {
		for _, re := range allowed {
			if re.MatchString(k) {
				continue next
			}
		}
		rp := invalid("parameter_unknown", "Received unknown parameter: "+k, k)
		return &rp
	}
	return nil
}

func missing(param string) *reply {
	rp := invalid("parameter_missing", "Missing required param: "+param+".", param)
	return &rp
}

func badInteger(param, value string) *reply {
	rp := invalid("parameter_invalid_integer", "Invalid integer: "+value, param)
	return &rp
}

// metadataOf collects the key-value pairs under prefix (such as "metadata"), within Stripe's
// limits: 50 keys, keys of at most 40 characters, values of at most 500.
func metadataOf(form url.Values, prefix string) (map[string]string, *reply) {
	out := map[string]string{}
	if rp := mergeMetadata(out, form, prefix); rp != nil {
		return nil, rp
	}
	return out, nil
}

// mergeMetadata applies the pairs under prefix to into, which a caller passes as a copy of what
// it keeps: an empty value unsets the key, and the limits (see metadataOf) are checked on the
// result. On a refusal into may be half changed.
func mergeMetadata(into map[string]string, form url.Values, prefix string) *reply {
	p := prefix + "["
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first refusal is the same on every run
	for _, k := range keys {
		if !strings.HasPrefix(k, p) || !strings.HasSuffix(k, "]") {
			continue
		}
		key := k[len(p) : len(k)-1]
		v := form[k]
		val := v[len(v)-1]
		switch {
		case utf8.RuneCountInString(key) > 40:
			rp := invalid("", "Metadata key names can be at most 40 characters.", k)
			return &rp
		case utf8.RuneCountInString(val) > 500:
			rp := invalid("", "Metadata values can be at most 500 characters.", k)
			return &rp
		}
		if val == "" {
			delete(into, key)
		} else {
			into[key] = val
		}
	}
	if len(into) > 50 {
		rp := invalid("", "You can have at most 50 keys in metadata.", prefix)
		return &rp
	}
	return nil
}

// lineItem is one line of a session or a link, from price_data (the integration's way: the
// amount isn't known ahead of time). A price id is refused: the fake has no price catalogue.
type lineItem struct {
	Name, Description string
	UnitAmount        int64
	Quantity          int64
}

// Line limits: a Checkout Session in payment mode takes 100 lines, a Payment Link 20.
const (
	maxSessionLines = 100
	maxLinkLines    = 20
)

func parseLineItems(form url.Values, maxLines int) (items []lineItem, currency string, rp *reply) {
	seen := map[int]bool{}
	for k := range form {
		if m := lineItemIndex.FindStringSubmatch(k); m != nil {
			i, _ := strconv.Atoi(m[1])
			seen[i] = true
		}
	}
	if len(seen) == 0 {
		return nil, "", missing("line_items")
	}
	if len(seen) > maxLines {
		r := invalid("", fmt.Sprintf("You can have at most %d line items.", maxLines), "line_items")
		return nil, "", &r
	}
	for i := range len(seen) {
		if !seen[i] {
			r := invalid("", "Invalid array: line_items indexes must be consecutive from 0.", "line_items")
			return nil, "", &r
		}
	}
	for i := range len(seen) {
		base := fmt.Sprintf("line_items[%d]", i)
		if v := form.Get(base + "[price]"); v != "" {
			r := invalid("resource_missing", "No such price: '"+v+"'", base+"[price]")
			return nil, "", &r
		}
		cur := form.Get(base + "[price_data][currency]")
		if cur == "" {
			return nil, "", missing(base + "[price_data][currency]")
		}
		if !knownCurrency(cur) {
			r := invalid("", "Invalid currency: "+cur+". Currencies are three lowercase letters, and Stripe must support them.",
				base+"[price_data][currency]")
			return nil, "", &r
		}
		if currency != "" && currency != cur {
			r := invalid("", "You cannot combine currencies on a single order.", base+"[price_data][currency]")
			return nil, "", &r
		}
		currency = cur
		amountText := form.Get(base + "[price_data][unit_amount]")
		if amountText == "" {
			return nil, "", missing(base + "[price_data][unit_amount]")
		}
		amount, err := strconv.ParseInt(amountText, 10, 64)
		if err != nil || amount < 0 {
			return nil, "", badInteger(base+"[price_data][unit_amount]", amountText)
		}
		if amount > maxAmount {
			r := invalid("amount_too_large", "Amount must be no more than 99999999 in the smallest currency unit.",
				base+"[price_data][unit_amount]")
			return nil, "", &r
		}
		if threeDecimal[cur] && amount%10 != 0 {
			r := invalid("", "For a three-decimal currency the last digit of an amount must be 0.",
				base+"[price_data][unit_amount]")
			return nil, "", &r
		}
		qtyText := form.Get(base + "[quantity]")
		if qtyText == "" {
			return nil, "", missing(base + "[quantity]")
		}
		qty, err := strconv.ParseInt(qtyText, 10, 64)
		if err != nil {
			return nil, "", badInteger(base+"[quantity]", qtyText)
		}
		if qty < 1 || qty > 999999 {
			r := invalid("", "Quantity must be between 1 and 999999.", base+"[quantity]")
			return nil, "", &r
		}
		name := form.Get(base + "[price_data][product_data][name]")
		if name == "" {
			return nil, "", missing(base + "[price_data][product_data][name]")
		}
		items = append(items, lineItem{
			Name: name, Description: form.Get(base + "[price_data][product_data][description]"),
			UnitAmount: amount, Quantity: qty,
		})
	}
	return items, currency, nil
}

// total adds up the lines and applies the amount limits of a payment.
func total(items []lineItem, currency string) (int64, *reply) {
	var sum int64
	for _, it := range items {
		sum += it.UnitAmount * it.Quantity
		if sum > maxAmount {
			r := invalid("amount_too_large", "Amount must be no more than 99999999 in the smallest currency unit.", "line_items")
			return 0, &r
		}
	}
	if sum < minAmount {
		r := invalid("amount_too_small", fmt.Sprintf(
			"The total amount due must add up to at least 50 in the smallest unit of %s.", currency), "line_items")
		return 0, &r
	}
	return sum, nil
}

// Amount limits, in the smallest unit of the currency. Stripe's minimum varies by currency
// (50 cents or 50 yen for the common ones); the fake applies 50 to all, which is the common case.
const (
	minAmount = 50
	maxAmount = 99999999
)

var (
	zeroDecimal = set("bif clp djf gnf jpy kmf krw mga pyg rwf ugx vnd vuv xaf xof xpf")
	// threeDecimal currencies take amounts whose last digit is 0 at Stripe.
	threeDecimal = set("bhd jod kwd omr tnd")
	twoDecimal   = set("usd eur gbp cad aud nzd chf sek nok dkk pln czk huf ron bgn brl mxn ars cop pen " +
		"inr idr myr php sgd thb twd hkd cny aed sar ils zar try uah egp ngn kes mad")
)

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

func knownCurrency(c string) bool { return zeroDecimal[c] || threeDecimal[c] || twoDecimal[c] }

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= 5000
}
