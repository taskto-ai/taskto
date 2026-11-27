package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/currency"
)

const (
	EffectCreateCard        = "create_card"
	EffectUpdateCard        = "update_card"
	EffectCreateDocument    = "create_document"
	EffectOpenRoute         = "open_route"
	EffectCreatePaymentLink = "create_payment_link"
)

var EffectKinds = []string{EffectCreateCard, EffectUpdateCard, EffectCreateDocument, EffectOpenRoute, EffectCreatePaymentLink}

const (
	RouteBoard    = "board"
	RouteCard     = "card"
	RouteDocument = "document"
)

var effectRoutes = []string{RouteBoard, RouteCard, RouteDocument}

func EffectCapability(kind string) string { return "effect." + kind }

var cardColumns = []string{"backlog", "todo", "doing", "review", "done"}

const (
	maxEffectsPerAction = 5
	maxEffectsTotal     = 20
	maxTemplateTitle    = 400
	maxTemplateBody     = 4000
	maxTemplatePath     = 300
	maxTemplateContent  = 8000
	maxEffectDocs       = 5
	maxTitleChars       = 200
	maxBodyBytes        = 30 << 10
	maxSlugChars        = 60
	maxContentBytes     = 256 << 10
)

type Effect struct {
	Kind          string   `json:"kind"`
	Name          string   `json:"name,omitempty"`
	When          *Rule    `json:"when,omitempty"`
	SpaceID       int64    `json:"space_id,omitempty"`
	BoardID       int64    `json:"board_id,omitempty"`
	Column        string   `json:"column,omitempty"`
	Title         string   `json:"title,omitempty"`
	Body          string   `json:"body,omitempty"`
	Docs          []string `json:"docs,omitempty"`
	Card          string   `json:"card,omitempty"`
	Path          string   `json:"path,omitempty"`
	Content       string   `json:"content,omitempty"`
	Route         string   `json:"route,omitempty"`
	Integration   string   `json:"integration,omitempty"`
	AmountField   string   `json:"amount_field,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	CurrencyField string   `json:"currency_field,omitempty"`
	Description   string   `json:"description,omitempty"`
	URLField      string   `json:"url_field,omitempty"`
	IDField       string   `json:"id_field,omitempty"`
}

func (e *Effect) UnmarshalJSON(b []byte) error {
	type plain Effect
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("an effect is one JSON object")
	}
	*e = Effect(p)
	return nil
}

func (e Effect) given() []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(e.Name != "", "name")
	add(e.When != nil, "when")
	add(e.SpaceID != 0, "space_id")
	add(e.BoardID != 0, "board_id")
	add(e.Column != "", "column")
	add(e.Title != "", "title")
	add(e.Body != "", "body")
	add(e.Docs != nil, "docs")
	add(e.Card != "", "card")
	add(e.Path != "", "path")
	add(e.Content != "", "content")
	add(e.Route != "", "route")
	add(e.Integration != "", "integration")
	add(e.AmountField != "", "amount_field")
	add(e.Currency != "", "currency")
	add(e.CurrencyField != "", "currency_field")
	add(e.Description != "", "description")
	add(e.URLField != "", "url_field")
	add(e.IDField != "", "id_field")
	return out
}

func (e Effect) targetUnset() bool {
	return e.SpaceID == 0 && e.BoardID == 0 && targetKinds(e)
}

func targetKinds(e Effect) bool {
	switch e.Kind {
	case EffectCreateCard, EffectCreateDocument:
		return true
	case EffectOpenRoute:
		return e.Route == RouteBoard || e.Route == RouteDocument
	}
	return false
}

func (d Definition) unsetTargets() []string {
	var out []string
	for _, name := range sortedKeys(d.Actions) {
		for _, e := range d.Actions[name].Effects {
			if e.targetUnset() {
				out = append(out, name)
			}
		}
	}
	return out
}

func (d *Definition) setTargets(spaceID, boardID int64) int {
	n := 0
	for name, a := range d.Actions {
		effects := slices.Clone(a.Effects)
		for i, e := range effects {
			if !e.targetUnset() {
				continue
			}
			effects[i].SpaceID = spaceID
			if e.Kind == EffectCreateCard || (e.Kind == EffectOpenRoute && e.Route == RouteBoard) {
				effects[i].BoardID = boardID
			}
			n++
		}
		a.Effects = effects
		d.Actions[name] = a
	}
	return n
}

type effectSpec struct{ allowed, required []string }

var effectSpecs = map[string]effectSpec{
	EffectCreateCard:     {allowed: []string{"name", "when", "space_id", "board_id", "column", "title", "body", "docs"}, required: []string{"space_id", "board_id", "title"}},
	EffectUpdateCard:     {allowed: []string{"when", "card", "column"}, required: []string{"card", "column"}},
	EffectCreateDocument: {allowed: []string{"when", "space_id", "path", "content"}, required: []string{"space_id", "path"}},
	EffectOpenRoute:      {allowed: []string{"when", "route", "space_id", "board_id", "card", "path"}, required: []string{"route"}},
	EffectCreatePaymentLink: {
		allowed:  []string{"when", "integration", "amount_field", "currency", "currency_field", "description", "url_field", "id_field"},
		required: []string{"integration", "amount_field", "url_field", "id_field"},
	},
}

var routeFields = map[string][]string{
	RouteBoard:    {"space_id", "board_id"},
	RouteCard:     {"card"},
	RouteDocument: {"space_id", "path"},
}

func (d Definition) effectNames() map[string]bool {
	names := map[string]bool{}
	for _, a := range d.Actions {
		for _, e := range a.Effects {
			if e.Kind == EffectCreateCard && e.Name != "" {
				names[e.Name] = true
			}
		}
	}
	return names
}

func (d Definition) cardMaker(name string) (Effect, bool) {
	for _, key := range sortedKeys(d.Actions) {
		for _, e := range d.Actions[key].Effects {
			if e.Kind == EffectCreateCard && e.Name == name {
				return e, true
			}
		}
	}
	return Effect{}, false
}

func (d Definition) checkEffects(c Collection) ([]string, error) {
	used := map[string]bool{}
	total := 0
	named := map[string]bool{}
	for _, name := range sortedKeys(d.Actions) {
		for _, e := range d.Actions[name].Effects {
			if e.Name == "" {
				continue
			}
			if !namePattern.MatchString(e.Name) {
				return nil, invalidf("actions: %q: effect name %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", name, e.Name)
			}
			if named[e.Name] {
				return nil, invalidf("actions: effect name %q is used twice", e.Name)
			}
			named[e.Name] = true
		}
	}
	creators := d.effectNames()
	for _, name := range sortedKeys(d.Actions) {
		a := d.Actions[name]
		if len(a.Effects) == 0 {
			continue
		}
		if a.Kind != ActionInsert && a.Kind != ActionUpdate {
			return nil, invalidf("actions: %q: only an insert or an update has effects", name)
		}
		if len(a.Effects) > maxEffectsPerAction {
			return nil, invalidf("actions: %q: at most %d effects", name, maxEffectsPerAction)
		}
		total += len(a.Effects)
		for i, e := range a.Effects {
			if err := e.check(c, creators); err != nil {
				return nil, invalidf("actions: %q: effect %d: %v", name, i, err)
			}
			used[EffectCapability(e.Kind)] = true
		}
	}
	if total > maxEffectsTotal {
		return nil, invalidf("actions: at most %d effects in all", maxEffectsTotal)
	}
	return sortedKeys(used), nil
}

func (e Effect) check(c Collection, creators map[string]bool) error {
	spec, ok := effectSpecs[e.Kind]
	if !ok {
		return fmt.Errorf("kind must be one of %s", strings.Join(EffectKinds, ", "))
	}
	given := e.given()
	allowed := spec.allowed
	if e.Kind == EffectOpenRoute {
		if !slices.Contains(effectRoutes, e.Route) {
			return fmt.Errorf("route must be one of %s", strings.Join(effectRoutes, ", "))
		}
		allowed = append([]string{"route", "when"}, routeFields[e.Route]...)
	}
	for _, f := range given {
		if !slices.Contains(allowed, f) {
			return fmt.Errorf("%s doesn't take %s", e.Kind, f)
		}
	}
	required := spec.required
	if e.Kind == EffectOpenRoute {
		required = append(slices.Clone(required), routeFields[e.Route]...)
	}
	for _, f := range required {
		unchosen := e.targetUnset() && (f == "space_id" || f == "board_id")
		if !slices.Contains(given, f) && !unchosen {
			return fmt.Errorf("%s needs %s", e.Kind, f)
		}
	}
	if e.When != nil {
		if err := e.When.check(c); err != nil {
			return fmt.Errorf("when: %w", err)
		}
	}
	if e.SpaceID < 0 || e.BoardID < 0 {
		return fmt.Errorf("space_id and board_id are IDs, from 1")
	}
	switch e.Kind {
	case EffectCreateCard:
		if e.Column != "" && !slices.Contains(cardColumns, e.Column) {
			return fmt.Errorf("column must be one of %s", strings.Join(cardColumns, ", "))
		}
		if err := checkTemplate("title", e.Title, c, maxTemplateTitle); err != nil {
			return err
		}
		if err := checkTemplate("body", e.Body, c, maxTemplateBody); err != nil {
			return err
		}
		if len(e.Docs) > maxEffectDocs {
			return fmt.Errorf("docs: at most %d", maxEffectDocs)
		}
		for _, p := range e.Docs {
			if err := checkPathTemplate("docs", p, c, false); err != nil {
				return err
			}
		}
	case EffectUpdateCard:
		if !slices.Contains(cardColumns, e.Column) {
			return fmt.Errorf("column must be one of %s", strings.Join(cardColumns, ", "))
		}
		if !creators[e.Card] {
			return fmt.Errorf("card names %q, which is not a create_card effect of this app (name one with \"name\")", e.Card)
		}
	case EffectCreateDocument:
		if err := checkPathTemplate("path", e.Path, c, true); err != nil {
			return err
		}
		if err := checkTemplate("content", e.Content, c, maxTemplateContent); err != nil {
			return err
		}
	case EffectCreatePaymentLink:
		return e.checkPaymentLink(c)
	case EffectOpenRoute:
		if e.Route == RouteCard && !creators[e.Card] {
			return fmt.Errorf("card names %q, which is not a create_card effect of this app (name one with \"name\")", e.Card)
		}
		if e.Route == RouteDocument {
			if err := checkPathTemplate("path", e.Path, c, true); err != nil {
				return err
			}
		}
	}
	return nil
}

const holeOpen, holeClose = "{{", "}}"

func holes(tpl string, fn func(text string, isHole bool) error) error {
	for tpl != "" {
		i := strings.Index(tpl, holeOpen)
		j := strings.Index(tpl, holeClose)
		switch {
		case i < 0 && j < 0:
			return fn(tpl, false)
		case i < 0 || (j >= 0 && j < i):
			return fmt.Errorf("a template has }} without {{")
		}
		if err := fn(tpl[:i], false); err != nil {
			return err
		}
		rest := tpl[i+len(holeOpen):]
		end := strings.Index(rest, holeClose)
		if end < 0 || strings.Contains(rest[:end], holeOpen) {
			return fmt.Errorf("a template has {{ without }}")
		}
		if err := fn(strings.TrimSpace(rest[:end]), true); err != nil {
			return err
		}
		tpl = rest[end+len(holeClose):]
	}
	return nil
}

func checkTemplate(what, tpl string, c Collection, max int) error {
	if utf8.RuneCountInString(tpl) > max {
		return fmt.Errorf("%s is at most %d characters", what, max)
	}
	if !utf8.ValidString(tpl) {
		return fmt.Errorf("%s must be UTF-8", what)
	}
	err := holes(tpl, func(hole string, isHole bool) error {
		if !isHole {
			return nil
		}
		if _, ok := c.field(hole); !ok && hole != "id" {
			return fmt.Errorf("%s: {{%s}} is not a field of the collection (the holes are the fields, and id)", what, hole)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func checkPathTemplate(what, tpl string, c Collection, document bool) error {
	if err := checkTemplate(what, tpl, c, maxTemplatePath); err != nil {
		return err
	}
	if tpl == "" || strings.HasPrefix(tpl, "/") || strings.Contains(tpl, "..") || strings.Contains(tpl, `\`) || strings.Contains(tpl, "//") {
		return fmt.Errorf("%s is a path inside the space, like inspections/{{id}}.md: no leading /, //, .. or \\", what)
	}
	if strings.ContainsFunc(tpl, unicode.IsControl) {
		return fmt.Errorf("%s has a control character", what)
	}
	if document && !strings.HasSuffix(tpl, ".md") {
		return fmt.Errorf("%s of a document ends in .md", what)
	}
	return nil
}

type renderMode int

const (
	modeLine renderMode = iota
	modeText
	modeSlug
)

func render(tpl string, c Collection, data map[string]any, id int64, mode renderMode) string {
	var b strings.Builder
	_ = holes(tpl, func(text string, isHole bool) error {
		if !isHole {
			b.WriteString(text)
			return nil
		}
		v := holeValue(c, data, id, text)
		switch mode {
		case modeLine:
			v = oneLine(v)
		case modeSlug:
			v = slug(v)
		default:
			v = strings.ReplaceAll(v, "\x00", "")
		}
		b.WriteString(v)
		return nil
	})
	out := b.String()
	switch mode {
	case modeLine:
		out = truncate(strings.TrimSpace(oneLine(out)), maxTitleChars)
	case modeText:
		out = truncateBytes(out, maxBodyBytes)
	}
	return out
}

func holeValue(c Collection, data map[string]any, id int64, name string) string {
	if name == "id" {
		if id == 0 {
			return "new"
		}
		return strconv.FormatInt(id, 10)
	}
	f, ok := c.field(name)
	if !ok {
		return ""
	}
	v, set := data[name]
	if !set || !ofType(f, v) {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func slug(s string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := truncate(strings.Trim(b.String(), "-"), maxSlugChars)
	if out == "" {
		return "untitled"
	}
	return out
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

type payload struct {
	SpaceID       int64             `json:"space_id,omitempty"`
	BoardID       int64             `json:"board_id,omitempty"`
	Column        string            `json:"column,omitempty"`
	Title         string            `json:"title,omitempty"`
	Body          string            `json:"body,omitempty"`
	Docs          []string          `json:"docs,omitempty"`
	Card          string            `json:"card,omitempty"`
	Path          string            `json:"path,omitempty"`
	Content       string            `json:"content,omitempty"`
	Route         string            `json:"route,omitempty"`
	Slot          string            `json:"slot,omitempty"`
	AmountField   string            `json:"amount_field,omitempty"`
	Amount        string            `json:"amount,omitempty"`
	Currency      string            `json:"currency,omitempty"`
	CurrencyField string            `json:"currency_field,omitempty"`
	Description   string            `json:"description,omitempty"`
	URLField      string            `json:"url_field,omitempty"`
	IDField       string            `json:"id_field,omitempty"`
	Describes     []string          `json:"describes,omitempty"`
	DescribesID   bool              `json:"describes_id,omitempty"`
	Uses          map[string]string `json:"uses,omitempty"`
}

func (e Effect) templatesOf() []string {
	switch e.Kind {
	case EffectCreateCard:
		return append([]string{e.Title, e.Body}, e.Docs...)
	case EffectCreateDocument:
		return []string{e.Path, e.Content}
	case EffectOpenRoute:
		if e.Route == RouteDocument {
			return []string{e.Path}
		}
	case EffectCreatePaymentLink:
		return []string{e.Description}
	}
	return nil
}

func (e Effect) usedFields(c Collection) map[string]string {
	var uses map[string]string
	for _, tpl := range e.templatesOf() {
		_ = holes(tpl, func(text string, isHole bool) error {
			if !isHole || text == "id" {
				return nil
			}
			if f, ok := c.field(text); ok {
				if uses == nil {
					uses = map[string]string{}
				}
				uses[text] = f.Type
			}
			return nil
		})
	}
	if e.Kind == EffectCreatePaymentLink {
		for _, name := range []string{e.AmountField, e.CurrencyField} {
			if f, ok := c.field(name); ok && name != "" {
				if uses == nil {
					uses = map[string]string{}
				}
				uses[name] = f.Type
			}
		}
	}
	return uses
}

func (p payload) usesHold(c Collection) bool {
	for name, typ := range p.Uses {
		if f, ok := c.field(name); !ok || f.Type != typ {
			return false
		}
	}
	return true
}

func (e Effect) applies(c Collection, data map[string]any) bool {
	return e.When == nil || c.holds(*e.When, data)
}

func (e Effect) payload(c Collection, data map[string]any, id int64) payload {
	p := payload{SpaceID: e.SpaceID, BoardID: e.BoardID, Card: e.Card, Route: e.Route, Uses: e.usedFields(c)}
	switch e.Kind {
	case EffectCreateCard:
		p.Column = e.Column
		if p.Column == "" {
			p.Column = cardColumns[0]
		}
		p.Title = render(e.Title, c, data, id, modeLine)
		p.Body = render(e.Body, c, data, id, modeText)
		for _, d := range e.Docs {
			p.Docs = append(p.Docs, render(d, c, data, id, modeSlug))
		}
	case EffectUpdateCard:
		p.Column = e.Column
	case EffectCreateDocument:
		p.Path = render(e.Path, c, data, id, modeSlug)
		p.Content = truncateBytes(render(e.Content, c, data, id, modeText), maxContentBytes)
	case EffectOpenRoute:
		if e.Route == RouteDocument {
			p.Path = render(e.Path, c, data, id, modeSlug)
		}
	case EffectCreatePaymentLink:
		p.Slot, p.AmountField, p.CurrencyField, p.URLField, p.IDField = e.Integration, e.AmountField, e.CurrencyField, e.URLField, e.IDField
		p.Description = render(e.Description, c, data, id, modeLine)
		p.Describes, p.DescribesID = e.describes()
		p.Amount, p.Currency = e.moneyOf(c, data)
	}
	return p
}

func (e Effect) moneyOf(c Collection, data map[string]any) (amount, code string) {
	if f, ok := c.field(e.AmountField); ok {
		if v, set := data[e.AmountField]; set && ofType(f, v) {
			switch n := v.(type) {
			case float64:
				amount, _ = currency.Decimal(n)
			case json.Number:
				if f, err := n.Float64(); err == nil {
					amount, _ = currency.Decimal(f)
				}
			}
		}
	}
	code = e.Currency
	if e.CurrencyField != "" {
		if f, ok := c.field(e.CurrencyField); ok {
			if v, set := data[e.CurrencyField]; set && ofType(f, v) {
				code, _ = v.(string)
			}
		}
	}
	return amount, strings.ToUpper(code)
}

func (e Effect) describes() (fields []string, id bool) {
	fields = []string{}
	_ = holes(e.Description, func(text string, isHole bool) error {
		switch {
		case isHole && text == "id":
			id = true
		case isHole:
			fields = mergeSorted(fields, []string{text})
		}
		return nil
	})
	return fields, id
}
