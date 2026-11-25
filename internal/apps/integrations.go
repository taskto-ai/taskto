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

// Integrations (docs/integrations.md).
//
// An app may use a service outside TaskTo.AI, starting with payments, without ever naming one. It
// declares a slot of a kind in Definition.Integrations ("payments", of the kind "payments"), and
// effects and event handlers refer to the slot by name. What the slot is connected to (a provider
// account, its keys, its limits) is a workspace admin's, bound when they approve the app's contract
// (integration_approvals.go), so a definition holds no URL, no redirect, no header, no secret, no
// provider and no integration id, and a template or a copy of an app works with whichever the
// admin binds. The shapes here refuse any key that is not one of theirs, and a test pins that.
//
//	"integrations": {"payments": {"kind": "payments", "on": {
//	    "payment.succeeded": {"only_if": {…a rule…}, "set": {"status": {"value": "paid"}, "paid_at": {"from": "occurred_at"}}},
//	    "payment.expired":   {"set": {"status": {"value": "sent"}}}}}}
//
// and, in an action of an insert or update, an effect that asks for a payment link:
//
//	{"kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency": "EUR",
//	 "description": "Invoice {{number}}", "url_field": "pay_url", "id_field": "payment_id"}
//
// The slot's handlers are what the integration may write to a record when the outside service
// reports an event: only the fields named, only the values or sources named, and only when the
// rule holds. That, and what the app sends out, is the contract (contract.go) an admin approves.

// KindPayments is the kind of integration an app asks for payments with.
const KindPayments = "payments"

// IntegrationKinds lists the kinds of integration, which are what the server supports.
var IntegrationKinds = []string{KindPayments}

// The events of the payments kind, which a slot's handlers are named for.
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentExpired   = "payment.expired"
	EventPaymentFailed    = "payment.failed"
	EventPaymentRefunded  = "payment.refunded"
)

// kindEvents are the events of each kind a slot may handle.
var kindEvents = map[string][]string{
	KindPayments: {EventPaymentSucceeded, EventPaymentExpired, EventPaymentFailed, EventPaymentRefunded},
}

// The sources a handler may write into a field with from: values the server takes from the event
// it verified, never from anything the outside service sends as text.
const (
	SourceOccurredAt = "occurred_at"
	SourcePaidAt     = "paid_at"
	SourceAmount     = "amount"
	SourceCurrency   = "currency"
	SourceReceiptURL = "receipt_url"
	SourcePaymentID  = "payment_id"
)

// sourceSpec says which events a source exists for and what a field must be to hold it.
type sourceSpec struct {
	events []string
	// fits says whether the field can hold the source's value, and what is wrong if not.
	fits func(Field) string
}

// The least a text field that holds a link or an id may hold, so a write after a payment can't be
// refused by a limit the app chose too small: a checkout link is long.
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

// Sources lists the sources a handler may write into a field with from, sorted.
func Sources() []string { return sortedKeys(sources) }

func allCurrencies(options []string) bool {
	return len(options) > 0 && !slices.ContainsFunc(options, func(o string) bool { return !currency.Known(o) })
}

// Bounds of the integrations a definition declares.
const (
	maxIntegrationSlots = 4
	maxHandlerSets      = 10
	maxDescription      = 400
)

// IntegrationSlot is a use of an outside service that an app declares: a kind, and what the
// service's events do to a record. It is never an integration: which provider account stands
// behind the slot is an admin's to say when they approve the app.
type IntegrationSlot struct {
	// Kind is one of IntegrationKinds.
	Kind string `json:"kind"`
	// On maps an event of the kind to what it does.
	On map[string]IntegrationHandler `json:"on,omitempty"`
}

// IntegrationHandler is what an event does to the record it is about: the fields it sets, when the
// rule of OnlyIf, if any, holds of the record as it is then.
type IntegrationHandler struct {
	// OnlyIf is a rule in the language of rules.go, over the record before the event is applied.
	OnlyIf *Rule `json:"only_if,omitempty"`
	// Set maps a field of the collection to what it is set to.
	Set map[string]IntegrationSet `json:"set" apitypes:"nonnull"`
}

// IntegrationSet is what one field is set to: a literal of the field's type (Value), or one of the
// sources (From).
type IntegrationSet struct {
	Value any    `json:"value,omitempty" apitypes:"json"`
	From  string `json:"from,omitempty"`
}

// strictObject decodes b into v refusing a key that isn't one of v's fields, so a url, a header, a
// secret or an id in a slot is an error, never silently dropped.
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

// UnmarshalJSON reads a slot strictly.
func (s *IntegrationSlot) UnmarshalJSON(b []byte) error {
	type plain IntegrationSlot
	var p plain
	if err := strictObject(b, &p, "an integration slot"); err != nil {
		return err
	}
	*s = IntegrationSlot(p)
	return nil
}

// UnmarshalJSON reads a handler strictly.
func (h *IntegrationHandler) UnmarshalJSON(b []byte) error {
	type plain IntegrationHandler
	var p plain
	if err := strictObject(b, &p, "an integration handler"); err != nil {
		return err
	}
	*h = IntegrationHandler(p)
	return nil
}

// UnmarshalJSON reads what a field is set to strictly: it is exactly one of a value and a source.
func (s *IntegrationSet) UnmarshalJSON(b []byte) error {
	// Both are pointers or raw: a key that is present is told from one that is absent, whatever it
	// holds, so {"value": null, "from": …} and {"value": "x", "from": ""} are two keys, refused.
	var p struct {
		Value json.RawMessage `json:"value"`
		From  *string         `json:"from"`
	}
	if err := strictObject(b, &p, "a set"); err != nil {
		return err
	}
	hasValue, hasFrom := p.Value != nil, p.From != nil
	switch {
	case hasValue == hasFrom:
		return fmt.Errorf(`a set is {"value": …} or {"from": …}, one of them`)
	case hasFrom:
		if *p.From == "" {
			return fmt.Errorf(`a set's from names a source`)
		}
		*s = IntegrationSet{From: *p.From}
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

// writable says whether an integration may write the field: the plain types only. A capture field
// (location, signature, barcode) or an attachment is locked or held by the person who captured it.
func writable(f Field) bool {
	switch f.Type {
	case TypeString, TypeNumber, TypeBoolean, TypeDate, TypeTime, TypeChoice:
		return true
	}
	return false
}

// checkIntegrations validates the integrations the definition declares, and the effects that use
// them, against the collection. It returns whether the definition uses any (they are for version 3
// of the format).
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

// slotCurrencies are the currencies the app's payment links ask in through the slot, sorted:
// none when no effect uses it.
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

// check validates one handler of a slot of kind, for event.
//
// currencies are the ones the slot asks in (see slotCurrencies): a choice that receives the
// currency must offer each of them, or a write for one the choice lacks would be refused.
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

// checkPaymentLink validates a create_payment_link effect's own fields against the collection. That its
// integration is a declared slot of the right kind is checkIntegrations'.
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

// ----- the contract -----

// contractVersion is the format of Contract, in its hash, so a change of format is a new hash.
const contractVersion = 2

// Contract is what the server derives from a draft for one slot: what the app may send out and
// what the integration may write back, in field names and literals, never in the app's wording.
// Computed in Go, from the definition, and never taken from a client: an admin approves its hash.
type Contract struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// Operations are the operations the app asks the service for (create_payment_link).
	Operations []string `json:"operations" apitypes:"nonnull"`
	// Sends are the fields of the record the app sends out: names only.
	Sends []string `json:"sends" apitypes:"nonnull"`
	// Currencies are the currencies the app asks in.
	Currencies []string `json:"currencies" apitypes:"nonnull"`
	// Events are the events the app handles.
	Events []string `json:"events" apitypes:"nonnull"`
	// Writes are the fields the integration may write, by the field's name, with the literals and
	// the sources it may write there.
	Writes []ContractWrite `json:"writes" apitypes:"nonnull"`
	// OnlyIf are the rules that gate each event's writes.
	OnlyIf []ContractRule `json:"only_if" apitypes:"nonnull"`
	// Links are the payment links the app asks for, one per effect: its action, the amount field,
	// the currency (a literal, or a choice field and its options), where the link and its id are
	// written, and the rule the record must meet. It binds a currency to the amount it is asked
	// for, and keeps a restriction (when) from being dropped without a new approval.
	Links []ContractLink `json:"links" apitypes:"nonnull"`
	// Handlers are each event's writes, field by field. Writes above says what may be written to a
	// field whatever the event; this binds each write to its event, so moving a value from one
	// event to another is another contract.
	Handlers []ContractHandler `json:"handlers" apitypes:"nonnull"`
}

// ContractLink is one kind of payment link request an app makes. It holds no wording.
type ContractLink struct {
	// Action is the action whose effect asks. It is an identifier (a client runs an action by it),
	// not wording: moving a request to another action is another contract.
	Action      string `json:"action"`
	AmountField string `json:"amount_field"`
	Currency    string `json:"currency,omitempty"`
	// CurrencyField and CurrencyOptions are the choice field the currency is read from, and the
	// currencies it offers, sorted.
	CurrencyField   string   `json:"currency_field,omitempty"`
	CurrencyOptions []string `json:"currency_options,omitempty"`
	URLField        string   `json:"url_field"`
	IDField         string   `json:"id_field"`
	// When is the rule the record must meet, as the change leaves it, for the link to be asked for.
	When *Rule `json:"when,omitempty"`
}

// ContractHandler is what one event writes.
type ContractHandler struct {
	Event string        `json:"event"`
	Sets  []ContractSet `json:"sets" apitypes:"nonnull"`
}

// ContractSet is one field an event writes: a literal, as JSON, or a source.
type ContractSet struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value,omitempty" apitypes:"json"`
	From  string          `json:"from,omitempty"`
}

// ContractWrite is what the integration may write to one field.
type ContractWrite struct {
	Field string `json:"field"`
	// Values are the literals it may write, as JSON, sorted.
	Values []json.RawMessage `json:"values" apitypes:"json,nonnull"`
	// From are the sources it may write, sorted.
	From []string `json:"from" apitypes:"nonnull"`
}

// ContractRule is the rule that gates an event's writes.
type ContractRule struct {
	Event string `json:"event"`
	Rule  Rule   `json:"rule"`
}

// linkSources are what the write-back of a payment link puts in the effect's two fields.
const (
	linkURL = "link.url"
	linkID  = "link.id"
)

// contract works out the contract of the slot, which the definition declares and Check has
// accepted.
func (d Definition) contract(slot string) Contract {
	s := d.Integrations[slot]
	c := Contract{Version: contractVersion, Kind: s.Kind,
		Operations: []string{}, Sends: []string{}, Currencies: []string{}, Events: []string{}, Writes: []ContractWrite{}, OnlyIf: []ContractRule{}, Links: []ContractLink{}, Handlers: []ContractHandler{}}
	sends, currencies := map[string]bool{}, map[string]bool{}
	writes := map[string]*ContractWrite{}
	var links []json.RawMessage // the encoded links already in c.Links
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
			link := ContractLink{Action: name, AmountField: e.AmountField, Currency: e.Currency, CurrencyField: e.CurrencyField,
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
				continue // a checked literal always encodes
			}
			handler.Sets = append(handler.Sets, ContractSet{Field: field, Value: raw})
			if !slices.ContainsFunc(w.Values, func(v json.RawMessage) bool { return bytes.Equal(v, raw) }) {
				w.Values = append(w.Values, raw)
			}
		}
		c.Handlers = append(c.Handlers, handler)
	}
	// Links in a fixed order, whatever the order of the actions and their effects.
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
	return c
}

// sortedCopy returns a sorted copy of list.
func sortedCopy(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return out
}

// Hash is the contract's canonical hash: the SHA-256, in hex, of its JSON, whose fields come in a
// fixed order and whose lists are sorted.
func (c Contract) Hash() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return "" // the contract holds only strings, numbers and rules
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Contracts returns the contract of each slot the definition declares, by the slot's name.
func (d Definition) Contracts() map[string]Contract {
	out := make(map[string]Contract, len(d.Integrations))
	for name := range d.Integrations {
		out[name] = d.contract(name)
	}
	return out
}
