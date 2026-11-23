package apps

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"taskto.ai/taskto/internal/access"
)

// The agent's app tools (docs/agent-tools.md) call the methods below. The agent works for
// one user at a time, in that user's conversation, and has exactly that user's access:
// each method checks a.UserID's access to the space through package access, as
// RequireSpace does for a request, and their roles in the app through Checker.App. A space
// or app the user can't read is ErrNotFound; a change the space doesn't let them make is
// ErrReadOnly, and one their roles in the app don't is ErrForbidden. Changes are recorded as
// a.UserID's, marked as the agent's when a.Agent is set. The agent has no tool to set roles,
// and an app's definition holds none: it gains no authority by what it writes.

// space returns spaceID as a's user sees it, or ErrNotFound.
func (s *Service) space(ctx context.Context, a Actor, spaceID int64) (access.Space, error) {
	sp, err := s.access.Check(ctx, a.UserID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return access.Space{}, ErrNotFound
	}
	return sp, err
}

// ListApps lists a space's apps, the offline ones too if withArchived.
func (s *Service) ListApps(ctx context.Context, a Actor, spaceID int64, withArchived bool) ([]App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listApps(ctx, sp, a, withArchived)
}

// ShowApp returns an app with its draft, for the agent that edits it, and the live
// release's number and needs but not its definition: both would be too much for one reply.
func (s *Service) ShowApp(ctx context.Context, a Actor, spaceID, appID int64) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.getApp(ctx, sp, a, appID)
	return forAgent(ap), err
}

// forAgent leaves out the live release's definition: the agent works on the draft, and a reply
// holds one definition (MaxDefinitionSize), not two.
func forAgent(ap App) App {
	if ap.Live != nil {
		live := *ap.Live
		live.Definition = nil
		ap.Live = &live
	}
	return ap
}

// CreateApp saves a new app in a space.
func (s *Service) CreateApp(ctx context.Context, a Actor, spaceID int64, na NewApp) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.createApp(ctx, sp, a, na)
	return forAgent(ap), err
}

// UpdateApp changes an app: ch.Version must be its current version, or ErrStale. A new
// definition replaces the draft; the live release is not touched, and the agent has no way
// to publish: that is for a person who may publish the app.
func (s *Service) UpdateApp(ctx context.Context, a Actor, spaceID, appID int64, ch Changes) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.updateApp(ctx, sp, a, appID, ch)
	return forAgent(ap), err
}

// ListRows returns an app's rows as the app shows them (Items), oldest first: those the
// user may read, which for a user who reads only their own records are theirs. An offline
// app's rows are kept, so they are read too.
func (s *Service) ListRows(ctx context.Context, a Actor, spaceID, appID int64) ([]map[string]any, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	ap, err := app(ctx, s.db, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	rows, err := loadRows(ctx, s.db, ap.ID, readable(acc, a.UserID))
	if err != nil {
		return nil, err
	}
	// As the app shows them: by the live release (an unpublished app has no rows).
	def := ap.draftDef
	if ap.liveDef != nil {
		def = *ap.liveDef
	}
	return Items(def, rows), nil
}

// QueryRows is a page of an app's rows as the app shows them (Items), filtered, searched and
// sorted, with the totals of everything that matches, for the rows the user may read (query.go).
// An offline app's rows are kept, so they are read too.
func (s *Service) QueryRows(ctx context.Context, a Actor, spaceID, appID int64, q Query) (RowsPage, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return RowsPage{}, err
	}
	res, err := s.queryRecords(ctx, sp, a, appID, q)
	if err != nil {
		return RowsPage{}, err
	}
	page := RowsPage{Rows: make([]map[string]any, len(res.Records)), NextCursor: res.NextCursor, Total: res.Total, Aggregates: res.Aggregates}
	for i, rec := range res.Records {
		it := map[string]any{"id": rec.ID, RevisionKey: rec.Revision}
		for name, v := range rec.Data {
			it[name] = v
		}
		page.Rows[i] = it
	}
	return page, nil
}

// RowsPage is QueryResult with its records as items.
type RowsPage struct {
	Rows       []map[string]any  `json:"rows"`
	NextCursor *string           `json:"next_cursor"`
	Total      int               `json:"total"`
	Aggregates []AggregateResult `json:"aggregates"`
}

//go:embed examples/*.json
var examplesFS embed.FS

// Example is a bundled app, to start from or learn the format.
type Example struct {
	Name       string     `json:"name"`
	Definition Definition `json:"definition"`
}

// Examples returns the bundled example apps, each checked as a saved app is.
func Examples() ([]Example, error) {
	entries, err := examplesFS.ReadDir("examples")
	if err != nil {
		return nil, err
	}
	var all []Example
	for _, e := range entries {
		raw, err := examplesFS.ReadFile("examples/" + e.Name())
		if err != nil {
			return nil, err
		}
		var ex Example
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ex); err != nil {
			return nil, fmt.Errorf("example %s: %w", e.Name(), err)
		}
		if ex.Definition, err = ex.Definition.Check(); err != nil {
			return nil, fmt.Errorf("example %s: %w", e.Name(), err)
		}
		all = append(all, ex)
	}
	return all, nil
}

// CatalogReference describes what an app may be made of, for the agent: the components
// and their props, the functions, the field types and the actions.
type CatalogReference struct {
	Protocol string `json:"protocol"`
	// Capabilities are what a definition may need (Definition.Requires), DefinitionVersion
	// the version of its format.
	DefinitionVersion int                          `json:"definition_version"`
	Capabilities      []string                     `json:"capabilities"`
	Components        map[string]map[string]string `json:"components"`
	Functions         map[string]map[string]string `json:"functions"`
	Rules             []string                     `json:"rules"`
	Examples          []Example                    `json:"examples"`
}

// Reference returns the catalog's reference, with the bundled examples.
func Reference() (CatalogReference, error) {
	ex, err := Examples()
	if err != nil {
		return CatalogReference{}, err
	}
	ref := CatalogReference{
		Protocol:          "A2UI " + ProtocolVersion + ": each component is {\"id\", \"component\", …props}; children are referred to by id",
		DefinitionVersion: DefinitionVersion,
		Capabilities:      Capabilities(),
		Components:        map[string]map[string]string{},
		Functions:         map[string]map[string]string{},
		Rules:             rules,
		Examples:          ex,
	}
	for name, spec := range components {
		ref.Components[name] = describeProps(spec)
	}
	for name, fn := range functions {
		args := describeProps(fn.args)
		args["(returns)"] = fn.returns
		ref.Functions[name] = args
	}
	return ref, nil
}

var rules = []string{
	`An app is {"name", "definition": {"components" or "screens"+"entry", "collection", "actions", "summaries"}}. definition_version is 2 (leave it out); a definition that says 1 can't use screens, time or choice fields, rules or navigation.`,
	`components: the screen of an app with one, at most 300; one has the id "root". Use only the components and props listed; anything else is refused. Screens is the server's own and can't be used.`,
	`screens: an app with several has "screens": [{"name", "root", "record"?, "components"}] and "entry" (the screen it opens on) instead of components, at most 12 screens and 300 components in all. Every component id is unique across the screens (not "root", which is the server's), a screen's "root" is the id of its root component, and a component refers only to its own screen's. The data model's /screen says which is shown.`,
	`Moving between screens: an action {"kind": "navigate", "screen": "<name>"} that a Button's event names, with no context; for a screen with "record": true the event's context is {"id": <the row's id>} (a path such as {"path": "id"} in a List template, or "/record/id" on a detail screen) and the screen shows that row at /record (id, _revision and the fields). Navigating changes no row and keeps the form at /draft, so what was typed survives going back and forth. An insert, update or delete may add "then": "<screen>" to open one after it runs.`,
	`Dynamic values are a literal, a data binding {"path": "/abs/path"} (or a path relative to the item inside a List template), or a function call {"call", "args"}.`,
	`A child list is ["id", …] or a template {"componentId", "path": "/items"} drawing the component once per row; inside it, paths like "amount" or "id" are the row's.`,
	`The data model: /items is the rows, each with its "id" and the collection's fields; /draft is an empty form to bind inputs to; each summary is at /<name>, as text.`,
	`collection.fields: [{"name", "type": "string"|"number"|"boolean"|"date"|"time"|"choice", "required"?, "required_when"? (a rule), "max_length"? (string, default 1000, at most 10000), "min"?/"max"? (number), "options"? (choice: 1 to 50 different texts)}], at most 50; a date is YYYY-MM-DD, a time HH:MM (24-hour); names are lowercase letters, digits and _, not id, items or draft. Bind a time to a DateTimeInput with enableTime true and enableDate false; a choice to a ChoicePicker with variant "mutuallyExclusive" and the same options (a value outside them is refused).`,
	`Rules are tiny and declarative, never code: {"field", "op": "eq"|"ne"|"lt"|"lte"|"gt"|"gte", "value": a literal of the field's type} or {"all": [rules]} or {"any": [rules]}, nested at most 3 deep, at most 16 parts; ordering is for number, date and time fields, and a field with no value equals nothing. A field's "required_when" makes it required when its rule holds of the row: the server enforces it on every insert and update, from any screen or release or from you, whatever is shown. The Show component {"when": rule, "source"?: "draft" (default, the form at /draft) | "record" (the record a detail screen shows), "child": id} draws its child only while the rule holds: it hides an input for people and is never a limit on what the server accepts. A summary may add "where": a rule, to count or sum only some rows.`,
	`actions: {"<event name>": {"kind": "insert"|"update"|"delete"|"navigate", "screen"? (navigate), "then"? (the others)}}. A Button's action {"event": {"name", "context"}} must name one. insert adds a row from the context's fields and clears /draft; update changes the fields the context gives of the row whose "id" it gives; delete removes the row whose "id" it gives.`,
	`summaries: {"<name>": {"kind": "count"} | {"kind": "sum", "field": "<number field>"}}.`,
	`Saving a definition (create_app, update_app) saves the app's DRAFT: it changes nothing people use. They keep using the live release until a person who may publish the app publishes the draft, in the Apps page; you have no tool to publish. A new app is a draft only, and is not launchable until published.`,
	`Publishing checks the draft against the app's rows: removing a field that rows have values for, changing a field's type, making a field required (or conditionally required) that some row lacks, or tightening its limits or a choice's options below a stored value needs the publisher to acknowledge it by name. So prefer adding fields as optional, and keep names and types of fields that have data.`,
	`Image, Video, AudioPlayer and openUrl are not available: an app never loads or opens outside addresses.`,
}

func describeProps(spec map[string]prop) map[string]string {
	out := map[string]string{}
	for name, p := range spec {
		d := kindNames[p.kind]
		if p.kind == kEnum {
			d = fmt.Sprintf("one of %q", p.values)
		}
		if p.required {
			d += " (required)"
		}
		out[name] = d
	}
	return out
}

var kindNames = map[kind]string{
	kString: "string", kNumber: "number", kInteger: "integer ≥ 0", kBool: "boolean",
	kDynString: "dynamic string", kDynNumber: "dynamic number", kDynBool: "dynamic boolean",
	kDynStringList: "dynamic list of strings", kDynValue: "dynamic value", kDynBoolList: "list of ≥ 2 dynamic booleans",
	kComponentID: "component id", kChildList: "child list", kAction: `{"event": {"name", "context"}} or {"functionCall"}`,
	kChecks:        `[{"condition": dynamic boolean, "message"}]`,
	kAccessibility: `{"label"?, "description"?: dynamic string}`,
	kTabs:          `[{"title": dynamic string, "child": component id}], at least one`,
	kOptions:       `[{"label": dynamic string, "value": string}]`,
	kIconName:      `an icon name (` + strings.Join(iconNames, ", ") + `), {"svgPath"} or a data binding`,
	kRule:          `a rule: {"field", "op": eq|ne|lt|lte|gt|gte, "value": literal}, {"all": [rule, …]} or {"any": [rule, …]}`,
	kScreenRefs:    `[{"name", "child": component id}]`,
}
