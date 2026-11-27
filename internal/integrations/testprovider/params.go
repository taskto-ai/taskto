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

const metaKey = `[^\[\]]{1,200}`

const idx = `(?:0|[1-9]\d*)`

const completedSessionsLimit = `restrictions\[completed_sessions\]\[limit\]`

const limitParam = "restrictions[completed_sessions][limit]"

var (
	lineItemParams = []string{
		`line_items\[` + idx + `\]\[quantity\]`,
		`line_items\[` + idx + `\]\[price\]`,
		`line_items\[` + idx + `\]\[price_data\]\[currency\]`,
		`line_items\[` + idx + `\]\[price_data\]\[unit_amount\]`,
		`line_items\[` + idx + `\]\[price_data\]\[product_data\]\[(name|description)\]`,
		`line_items\[` + idx + `\]\[price_data\]\[product_data\]\[metadata\]\[` + metaKey + `\]`,
		`line_items\[` + idx + `\]\[metadata\]\[` + metaKey + `\]`,
	}

	sessionParamRes = compileAll(append([]string{
		`mode`, `success_url`, `cancel_url`, `client_reference_id`, `customer_email`, `expires_at`,
		`locale`, `submit_type`, `ui_mode`,
		`allow_promotion_codes`,
		`metadata\[` + metaKey + `\]`,
		`payment_intent_data\[description\]`,
		`payment_intent_data\[metadata\]\[` + metaKey + `\]`,
	}, lineItemParams...))

	linkParamRes = compileAll(append([]string{
		`currency`, `allow_promotion_codes`, `submit_type`,
		`after_completion\[type\]`, `after_completion\[redirect\]\[url\]`,
		`after_completion\[hosted_confirmation\]\[custom_message\]`,
		completedSessionsLimit,
		`metadata\[` + metaKey + `\]`,
	}, lineItemParams...))

	linkUpdateParamRes = compileAll([]string{`active`, completedSessionsLimit, `metadata`, `metadata\[` + metaKey + `\]`})

	lineItemIndex = regexp.MustCompile(`^line_items\[(` + idx + `)\]`)
)

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`^` + p + `$`)
	}
	return out
}

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

func boolParam(form url.Values, name string) *reply {
	if v, ok := form[name]; ok && v[len(v)-1] != "true" && v[len(v)-1] != "false" {
		rp := invalid("", "Invalid boolean: "+v[len(v)-1], name)
		return &rp
	}
	return nil
}

func completedSessionsLimitOf(form url.Values) (limit int, ok bool, rp *reply) {
	v, sent := form[limitParam]
	if !sent {
		return 0, false, nil
	}
	text := v[len(v)-1]
	n, err := strconv.Atoi(text)
	if err != nil || strconv.Itoa(n) != text {
		return 0, false, badInteger(limitParam, text)
	}
	if n < 1 {
		r := invalid("", "Invalid "+limitParam+": it must be at least 1.", limitParam)
		return 0, false, &r
	}
	return n, true, nil
}

func missing(param string) *reply {
	rp := invalid("parameter_missing", "Missing required param: "+param+".", param)
	return &rp
}

func badInteger(param, value string) *reply {
	rp := invalid("parameter_invalid_integer", "Invalid integer: "+value, param)
	return &rp
}

func metadataOf(form url.Values, prefix string) (map[string]string, *reply) {
	out := map[string]string{}
	if rp := mergeMetadata(out, form, prefix); rp != nil {
		return nil, rp
	}
	return out, nil
}

func mergeMetadata(into map[string]string, form url.Values, prefix string) *reply {
	p := prefix + "["
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
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

type lineItem struct {
	Name, Description string
	UnitAmount        int64
	Quantity          int64
}

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
		if wholeUnits[cur] && amount%100 != 0 {
			r := invalid("", "Invalid amount for "+strings.ToUpper(cur)+": the currency takes whole units only, so the last two digits of the amount must be 00.",
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

func total(items []lineItem, currency string) (int64, *reply) {
	var sum int64
	for _, it := range items {
		sum += it.UnitAmount * it.Quantity
		if sum > maxAmount {
			r := invalid("amount_too_large", "Amount must be no more than 99999999 in the smallest currency unit.", "line_items")
			return 0, &r
		}
	}
	if min := minimumCharge(currency); sum < min {
		r := invalid("amount_too_small", fmt.Sprintf(
			"The total amount due must add up to at least %d in the smallest unit of %s.", min, currency), "line_items")
		return 0, &r
	}
	return sum, nil
}

const maxAmount = 99999999

var minimumCharges = map[string]int64{
	"usd": 50, "aed": 200, "ars": 50, "aud": 50, "brl": 50, "cad": 50, "chf": 50, "cop": 50,
	"czk": 1500, "dkk": 250, "eur": 50, "gbp": 30, "hkd": 400, "huf": 17500, "idr": 50, "ils": 50,
	"inr": 50, "jpy": 50, "krw": 50, "mxn": 1000, "myr": 200, "nok": 300, "nzd": 50, "php": 50,
	"pln": 200, "ron": 200, "rub": 50, "sek": 300, "sgd": 50, "thb": 1000, "zar": 50,
}

const fallbackMinimum = 50

func minimumCharge(currency string) int64 {
	if min, ok := minimumCharges[currency]; ok {
		return min
	}
	return fallbackMinimum
}

var (
	zeroDecimal  = set("bif clp djf gnf jpy kmf krw mga pyg rwf vnd vuv xaf xof xpf")
	wholeUnits   = set("isk ugx")
	threeDecimal = set("bhd jod kwd omr tnd")
	twoDecimal   = set("usd eur gbp cad aud nzd chf sek nok dkk pln czk huf ron bgn brl mxn ars cop pen " +
		"inr idr myr php rub sgd thb twd hkd cny aed sar ils zar try uah egp ngn kes mad")
)

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

func knownCurrency(c string) bool {
	return zeroDecimal[c] || wholeUnits[c] || threeDecimal[c] || twoDecimal[c]
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= 5000
}
