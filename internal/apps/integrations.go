package apps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/currency"
)

const KindPayments = "payments"

var IntegrationKinds = []string{KindPayments}

const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentExpired   = "payment.expired"
	EventPaymentFailed    = "payment.failed"
	EventPaymentRefunded  = "payment.refunded"
)

var kindEvents = map[string][]string{
	KindPayments: {EventPaymentSucceeded, EventPaymentExpired, EventPaymentFailed, EventPaymentRefunded},
}

const (
	SourceOccurredAt = "occurred_at"
	SourcePaidAt     = "paid_at"
	SourceAmount     = "amount"
	SourceCurrency   = "currency"
	SourceReceiptURL = "receipt_url"
	SourcePaymentID  = "payment_id"
)

type sourceSpec struct {
	events []string
	fits   func(Field) string
}

const (
	minLinkLength      = 2048
	minPaymentIDLength = 255
	minTimestampLength = 40
)

func textOf(min int) func(Field) string {
	return func(f Field) string {
		if f.Type != TypeString || f.MaxLength < min {
			return fmt.Sprintf("a string field with max_length of at least %d", min)
		}
		return ""
	}
}

var sources = map[string]sourceSpec{
	SourceOccurredAt: {events: kindEvents[KindPayments], fits: func(f Field) string {
		if f.Type == TypeDate || (f.Type == TypeString && f.MaxLength >= minTimestampLength) {
			return ""
		}
		return fmt.Sprintf("a date field, or a string field with max_length of at least %d", minTimestampLength)
	}},
	SourcePaidAt: {events: []string{EventPaymentSucceeded, EventPaymentRefunded}, fits: func(f Field) string {
		if f.Type == TypeDate || (f.Type == TypeString && f.MaxLength >= minTimestampLength) {
			return ""
		}
		return fmt.Sprintf("a date field, or a string field with max_length of at least %d", minTimestampLength)
	}},
	SourceAmount: {events: kindEvents[KindPayments], fits: func(f Field) string {
		if f.Type != TypeNumber {
			return "a number field"
		}
		return ""
	}},
	SourceCurrency: {events: kindEvents[KindPayments], fits: func(f Field) string {
		switch {
		case f.Type == TypeString && f.MaxLength >= 3:
			return ""
		case f.Type == TypeChoice && allCurrencies(f.Options):
			return ""
		}
		return "a string field, or a choice whose options are all currency codes"
	}},
	SourceReceiptURL: {events: []string{EventPaymentSucceeded, EventPaymentRefunded}, fits: textOf(minLinkLength)},
	SourcePaymentID:  {events: kindEvents[KindPayments], fits: textOf(minPaymentIDLength)},
}

func Sources() []string { return sortedKeys(sources) }

func allCurrencies(options []string) bool {
	return len(options) > 0 && !slices.ContainsFunc(options, func(o string) bool { return !currency.Known(o) })
}

const (
	maxIntegrationSlots = 4
	maxHandlerSets      = 10
	maxDescription      = 400
)

type IntegrationSlot struct {
	Kind string                        `json:"kind"`
	On   map[string]IntegrationHandler `json:"on,omitempty"`
}

type IntegrationHandler struct {
	OnlyIf *Rule                     `json:"only_if,omitempty"`
	Set    map[string]IntegrationSet `json:"set" apitypes:"nonnull"`
}

type IntegrationSet struct {
	Value any    `json:"value,omitempty" apitypes:"json"`
	From  string `json:"from,omitempty"`
}

func strictObject(b []byte, v any, what string) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("%s is one JSON object", what)
	}
	return nil
}

func (s *IntegrationSlot) UnmarshalJSON(b []byte) error {
	type plain IntegrationSlot
	var p plain
	if err := strictObject(b, &p, "an integration slot"); err != nil {
		return err
	}
	*s = IntegrationSlot(p)
	return nil
}

func (h *IntegrationHandler) UnmarshalJSON(b []byte) error {
	type plain IntegrationHandler
	var p plain
	if err := strictObject(b, &p, "an integration handler"); err != nil {
		return err
	}
	*h = IntegrationHandler(p)
	return nil
}

func (s *IntegrationSet) UnmarshalJSON(b []byte) error {
	var p struct {
		Value json.RawMessage `json:"value"`
		From  json.RawMessage `json:"from"`
	}
	if err := strictObject(b, &p, "a set"); err != nil {
		return err
	}
	hasValue, hasFrom := p.Value != nil, p.From != nil
	switch {
	case hasValue == hasFrom:
		return fmt.Errorf(`a set is {"value": …} or {"from": …}, one of them`)
	case hasFrom:
		var from string
		if err := json.Unmarshal(p.From, &from); err != nil || from == "" {
			return fmt.Errorf(`a set's from names a source`)
		}
		*s = IntegrationSet{From: from}
	default:
		var v any
		if err := json.Unmarshal(p.Value, &v); err != nil {
			return err
		}
		if v == nil {
			return fmt.Errorf(`a set's value is not null`)
		}
		*s = IntegrationSet{Value: v}
	}
	return nil
}

func writable(f Field) bool {
	switch f.Type {
	case TypeString, TypeNumber, TypeBoolean, TypeDate, TypeTime, TypeChoice:
		return true
	}
	return false
}

func (d Definition) checkIntegrations(c Collection) (bool, error) {
	uses := len(d.Integrations) > 0
	for _, name := range sortedKeys(d.Actions) {
		for _, e := range d.Actions[name].Effects {
			if e.Kind == EffectCreatePaymentLink {
				uses = true
			}
		}
	}
	if !uses {
		return false, nil
	}
	if len(d.Integrations) > maxIntegrationSlots {
		return true, invalidf("integrations: at most %d", maxIntegrationSlots)
	}
	used := map[string]bool{}
	for _, name := range sortedKeys(d.Actions) {
		a := d.Actions[name]
		links := 0
		for i, e := range a.Effects {
			if e.Kind != EffectCreatePaymentLink {
				continue
			}
			if links++; links > 1 {
				return true, invalidf("actions: %q: effect %d: an action asks for one payment link at most", name, i)
			}
			slot, ok := d.Integrations[e.Integration]
			switch {
			case !ok:
				return true, invalidf("actions: %q: effect %d: integration names %q, which the app doesn't declare under integrations (%s)",
					name, i, e.Integration, strings.Join(sortedKeys(d.Integrations), ", "))
			case slot.Kind != KindPayments:
				return true, invalidf("actions: %q: effect %d: create_payment_link needs a slot of kind %q, and %q is of kind %q",
					name, i, KindPayments, e.Integration, slot.Kind)
			}
			used[e.Integration] = true
		}
	}
	for _, name := range sortedKeys(d.Integrations) {
		slot := d.Integrations[name]
		if !namePattern.MatchString(name) {
			return true, invalidf("integrations: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", name)
		}
		if !slices.Contains(IntegrationKinds, slot.Kind) {
			return true, invalidf("integrations: %q: kind must be one of %s", name, strings.Join(IntegrationKinds, ", "))
		}
		for _, event := range sortedKeys(slot.On) {
			if err := slot.On[event].check(c, slot.Kind, event, d.slotCurrencies(name)); err != nil {
				return true, invalidf("integrations: %q: on %q: %v", name, event, err)
			}
		}
		if len(slot.On) > 0 {
			used[name] = true
		}
		if !used[name] {
			return true, invalidf("integrations: %q does nothing: an effect uses it or an event is handled", name)
		}
	}
	return true, nil
}

func (d Definition) slotCurrencies(slot string) []string {
	set := map[string]bool{}
	for _, a := range d.Actions {
		for _, e := range a.Effects {
			if e.Kind != EffectCreatePaymentLink || e.Integration != slot {
				continue
			}
			if e.CurrencyField == "" {
				set[e.Currency] = true
			} else if f, ok := d.Collection.field(e.CurrencyField); ok {
				for _, o := range f.Options {
					set[o] = true
				}
			}
		}
	}
	return sortedKeys(set)
}

func (h IntegrationHandler) check(c Collection, kind, event string, currencies []string) error {
	if !slices.Contains(kindEvents[kind], event) {
		return fmt.Errorf("an event of kind %s is one of %s", kind, strings.Join(kindEvents[kind], ", "))
	}
	if len(h.Set) < 1 || len(h.Set) > maxHandlerSets {
		return fmt.Errorf("set takes 1 to %d fields", maxHandlerSets)
	}
	if h.OnlyIf != nil {
		if err := h.OnlyIf.check(c); err != nil {
			return fmt.Errorf("only_if: %w", err)
		}
	}
	for _, name := range sortedKeys(h.Set) {
		f, ok := c.field(name)
		if !ok {
			return fmt.Errorf("set: %q is not a field of the collection", name)
		}
		if !writable(f) {
			return fmt.Errorf("set: %q is a %s field, which an integration can't write", name, f.Type)
		}
		s := h.Set[name]
		if s.From != "" {
			spec, known := sources[s.From]
			if !known {
				return fmt.Errorf("set: %q: from is one of %s", name, strings.Join(Sources(), ", "))
			}
			if !slices.Contains(spec.events, event) {
				return fmt.Errorf("set: %q: %s is not known for %s", name, s.From, event)
			}
			if why := spec.fits(f); why != "" {
				return fmt.Errorf("set: %q: %s goes in %s", name, s.From, why)
			}
			if s.From == SourceCurrency && f.Type == TypeChoice {
				for _, cur := range currencies {
					if !slices.Contains(f.Options, cur) {
						return fmt.Errorf("set: %q: the app asks in %s, which the choice's options lack", name, cur)
					}
				}
			}
			continue
		}
		if _, err := f.literal(s.Value); err != nil {
			return fmt.Errorf("set: %q: the value %w", name, err)
		}
		if _, present, err := f.parse(s.Value); err != nil {
			return fmt.Errorf("set: %q: the value is outside the field's limits", name)
		} else if !present {
			return fmt.Errorf("set: %q: the value is blank, which is no value: clear a field with an action, not an integration", name)
		}
	}
	return nil
}

func (e Effect) checkPaymentLink(c Collection) error {
	if f, ok := c.field(e.AmountField); !ok || f.Type != TypeNumber {
		return fmt.Errorf("amount_field names %q, which is not a number field of the collection", e.AmountField)
	}
	if (e.Currency == "") == (e.CurrencyField == "") {
		return fmt.Errorf("currency (a code) or currency_field (a choice field of codes), one of them")
	}
	if e.Currency != "" && !currency.Known(e.Currency) {
		return fmt.Errorf("currency is one of the codes %s", strings.Join(currency.Codes(), ", "))
	}
	if e.CurrencyField != "" {
		f, ok := c.field(e.CurrencyField)
		if !ok || f.Type != TypeChoice || !allCurrencies(f.Options) {
			return fmt.Errorf("currency_field names %q, which is not a choice field whose options are all currency codes", e.CurrencyField)
		}
	}
	if err := checkTemplate("description", e.Description, c, maxDescription); err != nil {
		return err
	}
	if !namePattern.MatchString(e.Integration) {
		return fmt.Errorf("integration names a slot of the app's integrations")
	}
	for _, p := range []struct{ what, name string }{{"url_field", e.URLField}, {"id_field", e.IDField}} {
		f, ok := c.field(p.name)
		min := minLinkLength
		if p.what == "id_field" {
			min = minPaymentIDLength
		}
		if !ok || f.Type != TypeString || f.MaxLength < min {
			return fmt.Errorf("%s names %q, which is not a string field with max_length of at least %d (a link or an id is long: set max_length)", p.what, p.name, min)
		}
	}
	if e.URLField == e.IDField {
		return fmt.Errorf("url_field and id_field are different fields")
	}
	if e.AmountField == e.URLField || e.AmountField == e.IDField {
		return fmt.Errorf("amount_field is not a field the link is written to")
	}
	return nil
}

const contractVersion = 5

type Contract struct {
	Version    int               `json:"version"`
	Kind       string            `json:"kind"`
	Operations []string          `json:"operations" apitypes:"nonnull"`
	Sends      []string          `json:"sends" apitypes:"nonnull"`
	Currencies []string          `json:"currencies" apitypes:"nonnull"`
	Events     []string          `json:"events" apitypes:"nonnull"`
	Writes     []ContractWrite   `json:"writes" apitypes:"nonnull"`
	OnlyIf     []ContractRule    `json:"only_if" apitypes:"nonnull"`
	Links      []ContractLink    `json:"links" apitypes:"nonnull"`
	Handlers   []ContractHandler `json:"handlers" apitypes:"nonnull"`
	Fields     []ContractField   `json:"fields" apitypes:"nonnull"`
}

type ContractField struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Required     bool     `json:"required,omitempty"`
	RequiredWhen *Rule    `json:"required_when,omitempty"`
	MaxLength    int      `json:"max_length,omitempty"`
	Min          *float64 `json:"min,omitempty"`
	Max          *float64 `json:"max,omitempty"`
	Options      []string `json:"options,omitempty"`
	Format       string   `json:"format,omitempty"`
	Editable     bool     `json:"editable,omitempty"`
	MaxFiles     int      `json:"max_files,omitempty"`
	MaxBytes     int      `json:"max_bytes,omitempty"`
}

type ContractLink struct {
	Action          string   `json:"action"`
	ActionKind      string   `json:"action_kind"`
	AmountField     string   `json:"amount_field"`
	Describes       []string `json:"describes" apitypes:"nonnull"`
	DescribesID     bool     `json:"describes_id"`
	Currency        string   `json:"currency,omitempty"`
	CurrencyField   string   `json:"currency_field,omitempty"`
	CurrencyOptions []string `json:"currency_options,omitempty"`
	URLField        string   `json:"url_field"`
	IDField         string   `json:"id_field"`
	When            *Rule    `json:"when,omitempty"`
}

type ContractHandler struct {
	Event string        `json:"event"`
	Sets  []ContractSet `json:"sets" apitypes:"nonnull"`
}

type ContractSet struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value,omitempty" apitypes:"json"`
	From  string          `json:"from,omitempty"`
}

type ContractWrite struct {
	Field  string            `json:"field"`
	Values []json.RawMessage `json:"values" apitypes:"json,nonnull"`
	From   []string          `json:"from" apitypes:"nonnull"`
}

type ContractRule struct {
	Event string `json:"event"`
	Rule  Rule   `json:"rule"`
}

const (
	linkURL = "link.url"
	linkID  = "link.id"
)

func (d Definition) contract(slot string) Contract {
	s := d.Integrations[slot]
	c := Contract{Version: contractVersion, Kind: s.Kind,
		Operations: []string{}, Sends: []string{}, Currencies: []string{}, Events: []string{}, Writes: []ContractWrite{}, Fields: []ContractField{}, OnlyIf: []ContractRule{}, Links: []ContractLink{}, Handlers: []ContractHandler{}}
	sends, currencies := map[string]bool{}, map[string]bool{}
	writes := map[string]*ContractWrite{}
	var links []json.RawMessage
	write := func(field string) *ContractWrite {
		w := writes[field]
		if w == nil {
			w = &ContractWrite{Field: field, Values: []json.RawMessage{}, From: []string{}}
			writes[field] = w
		}
		return w
	}
	for _, name := range sortedKeys(d.Actions) {
		for _, e := range d.Actions[name].Effects {
			if e.Kind != EffectCreatePaymentLink || e.Integration != slot {
				continue
			}
			c.Operations = mergeSorted(c.Operations, []string{e.Kind})
			describes, describesID := e.describes()
			link := ContractLink{Action: name, ActionKind: d.Actions[name].Kind, AmountField: e.AmountField, Describes: describes, DescribesID: describesID, Currency: e.Currency, CurrencyField: e.CurrencyField,
				URLField: e.URLField, IDField: e.IDField, When: e.When}
			sends[e.AmountField] = true
			if e.CurrencyField != "" {
				sends[e.CurrencyField] = true
				if f, ok := d.Collection.field(e.CurrencyField); ok {
					link.CurrencyOptions = sortedCopy(f.Options)
					for _, o := range f.Options {
						currencies[o] = true
					}
				}
			} else {
				currencies[e.Currency] = true
			}
			if raw, err := json.Marshal(link); err == nil && !slices.ContainsFunc(links, func(l json.RawMessage) bool { return bytes.Equal(l, raw) }) {
				links = append(links, raw)
				c.Links = append(c.Links, link)
			}
			_ = holes(e.Description, func(text string, isHole bool) error {
				if isHole && text != "id" {
					sends[text] = true
				}
				return nil
			})
			w := write(e.URLField)
			w.From = mergeSorted(w.From, []string{linkURL})
			w = write(e.IDField)
			w.From = mergeSorted(w.From, []string{linkID})
		}
	}
	for _, event := range sortedKeys(s.On) {
		h := s.On[event]
		c.Events = append(c.Events, event)
		if h.OnlyIf != nil {
			c.OnlyIf = append(c.OnlyIf, ContractRule{Event: event, Rule: *h.OnlyIf})
		}
		handler := ContractHandler{Event: event, Sets: []ContractSet{}}
		for _, field := range sortedKeys(h.Set) {
			set := h.Set[field]
			w := write(field)
			if set.From != "" {
				w.From = mergeSorted(w.From, []string{set.From})
				handler.Sets = append(handler.Sets, ContractSet{Field: field, From: set.From})
				continue
			}
			raw, err := json.Marshal(set.Value)
			if err != nil {
				continue
			}
			handler.Sets = append(handler.Sets, ContractSet{Field: field, Value: raw})
			if !slices.ContainsFunc(w.Values, func(v json.RawMessage) bool { return bytes.Equal(v, raw) }) {
				w.Values = append(w.Values, raw)
			}
		}
		c.Handlers = append(c.Handlers, handler)
	}
	slices.SortFunc(c.Links, func(a, b ContractLink) int {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return bytes.Compare(x, y)
	})
	c.Sends = sortedKeys(sends)
	c.Currencies = sortedKeys(currencies)
	for _, field := range sortedKeys(writes) {
		w := writes[field]
		slices.SortFunc(w.Values, func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
		c.Writes = append(c.Writes, *w)
	}
	named := map[string]bool{}
	for f := range sends {
		named[f] = true
	}
	for f := range writes {
		named[f] = true
	}
	for _, l := range c.Links {
		for _, f := range []string{l.AmountField, l.CurrencyField, l.URLField, l.IDField} {
			if f != "" {
				named[f] = true
			}
		}
		if l.When != nil {
			for _, f := range ruleFields(*l.When) {
				named[f] = true
			}
		}
	}
	for _, r := range c.OnlyIf {
		for _, f := range ruleFields(r.Rule) {
			named[f] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, f := range d.Collection.Fields {
			if f.RequiredWhen == nil {
				continue
			}
			deps := ruleFields(*f.RequiredWhen)
			if named[f.Name] {
				for _, dep := range deps {
					if !named[dep] {
						named[dep], grew = true, true
					}
				}
			} else if slices.ContainsFunc(deps, func(dep string) bool { return named[dep] }) {
				named[f.Name], grew = true, true
			}
		}
	}
	for _, name := range sortedKeys(named) {
		f, ok := d.Collection.field(name)
		if !ok {
			continue
		}
		c.Fields = append(c.Fields, ContractField{Name: f.Name, Type: f.Type, Required: f.Required, RequiredWhen: f.RequiredWhen,
			MaxLength: f.MaxLength, Min: f.Min, Max: f.Max, Options: sortedCopy(f.Options),
			Format: f.Format, Editable: f.Editable, MaxFiles: f.MaxFiles, MaxBytes: f.MaxBytes})
	}
	return c
}

func sortedCopy(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return out
}

func (c Contract) Hash() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (d Definition) Contracts() map[string]Contract {
	out := make(map[string]Contract, len(d.Integrations))
	for name := range d.Integrations {
		out[name] = d.contract(name)
	}
	return out
}
