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
)

// Platform effects.
//
// An insert or an update may declare effects (Action.Effects): operations on the rest of
// TaskTo.AI that the record's change sets going. They are a fixed, small registry, checked
// strictly when the definition is saved, and nothing in a definition can name anything outside
// it: not a URL, not a tool, not a command.
//
//	create_card      a card on a board of a space, from templates over the record's fields
//	update_card      moves a card an earlier create_card effect of the app made for the record
//	create_document  a Markdown document under a path in a space, from templates
//	open_route       a route of the web app to open (a board, a card or a document of the
//	                 app's own effects), from a fixed list, never an address
//
// An effect may name a rule (When, the language of rules.go) that decides whether it applies to
// the record as the change leaves it. Templates are text with {{field}} holes and nothing else:
// a field of the collection, or id, the record's number. There are no expressions, no
// functions and no nesting, and what is put in a hole is never read as a template again.
//
// A definition holds the aims of effects, never authority: when an effect runs it runs as the
// user who acted and does only what that user may do, checked then (outbox.go).

// The kinds of effect.
const (
	EffectCreateCard     = "create_card"
	EffectUpdateCard     = "update_card"
	EffectCreateDocument = "create_document"
	EffectOpenRoute      = "open_route"
	// EffectCreatePaymentLink asks an integration of the payments kind, which the app declares as a
	// slot (integrations.go), for a link to pay an amount of the record. It names the slot, never a
	// provider, an address or a key. Version 3 of the format.
	EffectCreatePaymentLink = "create_payment_link"
)

// EffectKinds lists the kinds of effect, which are what the server supports.
var EffectKinds = []string{EffectCreateCard, EffectUpdateCard, EffectCreateDocument, EffectOpenRoute, EffectCreatePaymentLink}

// The routes an open_route effect may open.
const (
	RouteBoard    = "board"
	RouteCard     = "card"
	RouteDocument = "document"
)

var effectRoutes = []string{RouteBoard, RouteCard, RouteDocument}

// EffectCapability is the capability of using effect kind: a release with such an effect needs a
// client that knows to show its progress (Definition.Requires).
func EffectCapability(kind string) string { return "effect." + kind }

// cardColumns are the columns a card can be put in (package boards).
var cardColumns = []string{"backlog", "todo", "doing", "review", "done"}

// Bounds of effects.
const (
	maxEffectsPerAction = 5
	maxEffectsTotal     = 20
	maxTemplateTitle    = 400
	maxTemplateBody     = 4000
	maxTemplatePath     = 300
	maxTemplateContent  = 8000
	maxEffectDocs       = 5
	// The most a filled-in template holds.
	maxTitleChars   = 200
	maxBodyBytes    = 30 << 10
	maxSlugChars    = 60
	maxContentBytes = 256 << 10
)

// Effect is one effect of an action. Which fields a kind takes is fixed (effectSpecs); a field it
// doesn't take, or a key that isn't one of these, is refused.
type Effect struct {
	Kind string `json:"kind"`
	// Name names a create_card effect, so update_card and open_route can refer to the card it
	// made. Unique in the definition.
	Name string `json:"name,omitempty"`
	// When is the rule the record must satisfy, as the change leaves it, for the effect to apply.
	When *Rule `json:"when,omitempty"`
	// SpaceID and BoardID name the target by ID, in the definition, as the app's builder chose.
	// Whoever acts needs access to both the app and the target when the effect runs.
	SpaceID int64 `json:"space_id,omitempty"`
	BoardID int64 `json:"board_id,omitempty"`
	// Column is the column a card is created in (default backlog) or moved to.
	Column string `json:"column,omitempty"`
	// Title and Body are templates of a card. Docs are templates of paths of documents in the
	// board's space that the card refers to.
	Title string   `json:"title,omitempty"`
	Body  string   `json:"body,omitempty"`
	Docs  []string `json:"docs,omitempty"`
	// Card is the Name of the create_card effect whose card for the record update_card changes
	// and an open_route of kind "card" opens.
	Card string `json:"card,omitempty"`
	// Path and Content are templates of a document: where it goes in the space, and what it says.
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	// Route is the route open_route opens, one of effectRoutes.
	Route string `json:"route,omitempty"`
	// What create_payment_link takes (integrations.go): the slot of the app's integrations it asks, the
	// number field with the amount, the currency (a code, or a choice field of codes), a template of
	// the text the payer reads, and the two string fields that the link and its id are written to.
	Integration   string `json:"integration,omitempty"`
	AmountField   string `json:"amount_field,omitempty"`
	Currency      string `json:"currency,omitempty"`
	CurrencyField string `json:"currency_field,omitempty"`
	Description   string `json:"description,omitempty"`
	URLField      string `json:"url_field,omitempty"`
	IDField       string `json:"id_field,omitempty"`
}

// UnmarshalJSON reads an effect strictly, so a key that isn't one of an effect's (a url, a tool
// name, a command) is refused instead of being dropped.
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

// given lists the JSON names of the fields the effect sets.
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

// targetUnset is whether the effect names a space and board of TaskTo.AI to act on, and none has
// been chosen yet: what a template, or a copy of an app whose boards its copier can't write,
// holds until someone picks one. Such a definition is a draft that can be saved and read but not
// published (unsetTargets, checked when it is), and one half chosen is refused like any other.
func (e Effect) targetUnset() bool {
	return e.SpaceID == 0 && e.BoardID == 0 && targetKinds(e)
}

// targetKinds is whether the effect has a space (and for a card or a board route, a board) to name.
func targetKinds(e Effect) bool {
	switch e.Kind {
	case EffectCreateCard, EffectCreateDocument:
		return true
	case EffectOpenRoute:
		return e.Route == RouteBoard || e.Route == RouteDocument
	}
	return false
}

// unsetTargets lists, as "action name", the effects of the definition that still have no target
// chosen, in the order of the actions' names.
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

// setTargets gives every effect that has no target the board of space, and returns how many it
// changed. A document effect takes only the space.
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

// effectSpec says which fields of a kind of effect may be set and which must be.
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

// routeFields are the fields each route of open_route takes besides route and when.
var routeFields = map[string][]string{
	RouteBoard:    {"space_id", "board_id"},
	RouteCard:     {"card"},
	RouteDocument: {"space_id", "path"},
}

// effectNames collects the names of the definition's create_card effects.
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

// cardMaker finds the create_card effect named name.
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

// checkEffects validates the effects of every action against the collection and returns the
// capabilities they need. Effects belong to an insert or an update (a delete leaves no record to
// act on, a navigate changes nothing).
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

// check validates one effect: its kind, the fields it takes, and their values. creators are the
// names of the definition's create_card effects.
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

// A template is text with holes, {{name}}: name is a field of the collection or id. Nothing else
// is a hole, and a "{{" or "}}" that isn't one is refused, so no one expects more of it than it
// does.
const holeOpen, holeClose = "{{", "}}"

// holes walks a template: each call gets literal text, or, for a hole, its name.
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

// checkTemplate checks a template's holes against the collection and its length.
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

// checkPathTemplate checks a template of a knowledge-base path. Only its literal text is the
// builder's: it can't climb out of a folder, and what is put in its holes is made a safe name
// (render). A document's path ends in .md.
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
	modeLine renderMode = iota // one line: a title
	modeText                   // text: a body, a document
	modeSlug                   // a name in a path
)

// render fills a template with the record's values. id is the record's number (0 for one that
// doesn't exist yet: a preview). What fills a hole is made to fit where it goes and is never read
// as template again.
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

// holeValue is what a hole shows: the field's value as text, nothing if the record has none.
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

// oneLine puts text on one line: control characters, line breaks included, become spaces.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// slug makes text a name fit for a path's segment: letters, digits, - and _, the rest one -.
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

// payload is an effect with its templates filled in from a record: what the worker does, and
// what is stored with the outbox entry. Nothing in it came from the client.
type payload struct {
	SpaceID int64    `json:"space_id,omitempty"`
	BoardID int64    `json:"board_id,omitempty"`
	Column  string   `json:"column,omitempty"`
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body,omitempty"`
	Docs    []string `json:"docs,omitempty"`
	Card    string   `json:"card,omitempty"`
	Path    string   `json:"path,omitempty"`
	Content string   `json:"content,omitempty"`
	Route   string   `json:"route,omitempty"`
	// Uses names the fields of the record whose values the templates were filled with, and their
	// types then: what is shown of the entry is shown only while the live release still declares
	// them so (usesHold), as a field the release removed or retyped is not for people to read.
	Uses map[string]string `json:"uses,omitempty"`
}

// templatesOf lists the templates of the effect that its payload is filled from.
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
	}
	return nil
}

// usedFields is the fields of c the effect's templates have holes for, with their types.
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
	return uses
}

// usesHold reports whether every field the entry was filled from is still declared by c with the
// type it had.
func (p payload) usesHold(c Collection) bool {
	for name, typ := range p.Uses {
		if f, ok := c.field(name); !ok || f.Type != typ {
			return false
		}
	}
	return true
}

// applies reports whether the effect applies to the record as the change leaves it.
func (e Effect) applies(c Collection, data map[string]any) bool {
	return e.When == nil || c.holds(*e.When, data)
}

// payload fills in the effect from the record.
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
	}
	return p
}
