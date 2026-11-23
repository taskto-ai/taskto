// Package apps holds personal apps: a screen built from a fixed catalog of A2UI components,
// a collection of rows with a schema, and the actions the screen's buttons may run, which
// are a fixed set served by Go. An app is plain JSON, checked strictly when it is saved;
// no code from the app ever runs on the server, and in the browser only the catalog's
// components and functions do.
//
// An app lives in a space, and access to it and its rows is access to the space, as
// package access answers it:
//
//   - a private space's apps are its owner's alone;
//   - a shared space's apps are its members': editors create, change and use them (their
//     actions change rows), viewers launch them and see the rows, read-only;
//   - an app the caller can't read answers ErrNotFound, like a space; the apps of an
//     archived space are read-only;
//   - inside the space, roles in the app (access.AppAccess, set with the roles routes) only
//     narrow that: whoever may write the space may still be allowed to submit records and
//     not to change the app (ErrForbidden), and may read all of its records or only their
//     own. Every way of reading records (screens, summaries, counts, the agent's reads)
//     goes through one filter, visibility. An app with no roles set is as the space alone
//     allows, which is what every app was before roles.
//
// Sharing an app is moving it to a shared space: its rows go with it.
//
// Taking an app offline archives it: it can't be launched and its actions don't run, and
// its rows are kept until it is restored.
//
// Records (the rows) are versioned. Each has a revision, 1 when created and bumped by every
// change; an update or delete may name the revision it was made against, and if the record
// has moved on it fails with ErrConflict and changes nothing. Every revision is kept in the
// record's history, with who made it, when and by which action. A delete is a tombstone, not
// a removal: the record leaves every ordinary read, screen, summary, count and limit, stays
// in the list of deleted records, and can be restored (a new revision) by whoever may
// change it. Reading a record's history, or the deleted ones, takes reading the record.
//
// An app has a draft and releases (release.go). Editing it, by a person or by the agent,
// changes the draft; the app serves its live release until someone who may publish it
// publishes the draft, after the server checks it against the catalog and the rows, or rolls
// back to an earlier release. A release is immutable and numbered.
//
// Changes to an app are optimistic: it carries a version, bumped by every change (the draft's,
// a publish and a rollback included), and an update names the version it was made against; if
// that isn't the current one it fails with ErrStale.
//
// HTTP handlers (http.go) and the agent's tools (tools.go) share the code in store.go;
// the tools take an Actor and check its user's access to the space themselves.
package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

// ProtocolVersion is the A2UI protocol the web renderer (@a2ui/react v0_9) speaks.
const ProtocolVersion = "v0.9"

// CatalogID names the component catalog apps may use (catalog.go). The web app registers
// the same ID; a message naming any other catalog is rejected there.
const CatalogID = "https://taskto.ai/catalogs/apps/v1.json"

// DefinitionVersion is the version of the definition's own format, the one this server writes
// and reads. It is TaskTo.AI's, independent of ProtocolVersion (A2UI's): it grows when the
// definition gains something an older reader would misread (a new kind of action, say), while
// the catalog's growth is told by Definition.Requires. A definition that says a higher version
// than this is refused, and a client that finds a release in a higher one asks to be updated.
//
// Version 1 is one screen, string, number, boolean and date fields, and the actions insert,
// update and delete. Version 2 adds screens with navigation between them, time and choice
// fields, rules (conditionally required fields, the Show component, summaries over some rows),
// the navigate action, effects on boards and documents, attachment fields (photos,
// attachments.go) and contributions (contributions.go: home widgets and a navigation pin, each
// slot a capability the release needs). A definition that says version 1 is read, checked and
// drawn as it always was, and can't use any of version 2.
const DefinitionVersion = 2

// firstVersion is the version of the format before screens and rules.
const firstVersion = 1

// Definition is what an app is made of: its screen or screens, its collection and its actions.
type Definition struct {
	// DefinitionVersion is the format version the definition is written in; DefinitionVersion
	// if left out.
	DefinitionVersion int `json:"definition_version"`
	// Requires lists the catalog capabilities the screen needs (Capabilities): a component
	// it draws is component.<Name>, a function it calls function.<name>. Check works the list
	// out from the screen, replacing whatever was sent, so it can't claim less than it uses.
	// A client whose catalog lacks one of them can't draw the app.
	Requires []string `json:"requires"`
	// Components is the screen of an app with one: the A2UI component list, sent as is in
	// updateComponents. The root component has the ID "root". An app with Screens has none.
	Components []json.RawMessage `json:"components,omitempty"`
	// Screens are the screens of an app with several (screens.go), and Entry the one it opens
	// on. An app has Components or Screens, never both.
	Screens    []Screen   `json:"screens,omitempty"`
	Entry      string     `json:"entry,omitempty"`
	Collection Collection `json:"collection"`
	// Actions maps an A2UI event name to what it does. A button can only send an event
	// named here.
	Actions map[string]Action `json:"actions"`
	// Summaries are values computed from the rows and shown in the data model.
	Summaries map[string]Summary `json:"summaries,omitempty"`
	// Contributions are what the app offers the platform's Home page and navigation
	// (contributions.go): version 2 of the format.
	Contributions *Contributions `json:"contributions,omitempty"`
}

// Collection is the schema of the app's rows, shown in the data model at /items.
type Collection struct {
	Fields []Field `json:"fields"`
}

// The types of field.
const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeDate    = "date"   // YYYY-MM-DD
	TypeTime    = "time"   // HH:MM, on a 24-hour clock (definition version 2)
	TypeChoice  = "choice" // one of the field's Options (definition version 2)
	// TypeAttachment is a list of photos: attachment ids, which the records hold in place of
	// bytes (definition version 2, attachments.go).
	TypeAttachment = "attachment"
)

// Field is one typed field of a row.
type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	// RequiredWhen makes the field required when its rule holds of the row (rules.go): on every
	// insert and update, whichever release's screen sends it. A field that is always required
	// has none.
	RequiredWhen *Rule `json:"required_when,omitempty"`
	// MaxLength bounds a string field, in characters: DefaultMaxLength if unset, at most
	// MaxMaxLength.
	MaxLength int `json:"max_length,omitempty"`
	// Min and Max bound a number field.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Options are the values a choice field may hold: at most MaxOptions, each unique.
	Options []string `json:"options,omitempty"`
	// MaxFiles bounds how many attachments an attachment field holds: DefaultMaxFiles if unset,
	// at most MaxMaxFiles. MaxBytes bounds each one's size: DefaultMaxFileBytes if unset, at most
	// MaxMaxFileBytes.
	MaxFiles int `json:"max_files,omitempty"`
	MaxBytes int `json:"max_bytes,omitempty"`
}

// The kinds of action: add a row built from the event's context and clear the form at
// /draft; change the fields the context gives of the row whose "id" it gives; delete the
// row whose "id" it gives; open another screen (screens.go).
const (
	ActionInsert   = "insert"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionNavigate = "navigate"
)

// Action is what an event does.
type Action struct {
	Kind string `json:"kind"`
	// Screen is the screen a navigate action opens.
	Screen string `json:"screen,omitempty"`
	// Then is the screen an insert, update or delete opens once it has run, if any.
	Then string `json:"then,omitempty"`
	// Effects are what an insert or an update sets going on the rest of TaskTo.AI once the
	// record is changed (effects.go): done by the server's worker, as the user who acted.
	Effects []Effect `json:"effects,omitempty"`
}

// Summary is a value computed over the rows: "count", or "sum" of a number field. Where
// narrows it to the rows its rule holds of.
type Summary struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
	Where *Rule  `json:"where,omitempty"`
}

// Bounds of a definition and of an app's rows.
const (
	MaxFields        = 50
	DefaultMaxLength = 1000
	MaxMaxLength     = 10000
	MaxOptions       = 50
	maxOptionLen     = 100
	maxActions       = 50
	maxSummaries     = 20
	// The bounds of an attachment field, and the defaults when it names none.
	DefaultMaxFiles     = 1
	MaxMaxFiles         = 10
	DefaultMaxFileBytes = 8 << 20
	MaxMaxFileBytes     = 16 << 20
	// MaxDefinitionSize bounds a definition's JSON, so the agent can always read one whole
	// (agenttools.MaxResult).
	MaxDefinitionSize = 48 << 10
	// MaxRows and MaxDataSize bound an app's rows, so a screen with all of them stays a
	// reasonable size.
	MaxRows     = 5000
	MaxDataSize = 4 << 20
)

// names are those of fields, actions and summaries: they appear in data paths and event
// names, so they are plain.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// reserved are the data model keys the platform fills, and a row's own ID; a field or
// summary can't use them.
var reserved = map[string]bool{ItemsKey: true, DraftKey: true, "id": true}

// The data model's keys the platform fills. An app with screens has two more, ScreenKey and
// RecordKey, which its summaries can't use either.
const (
	ItemsKey  = "items"
	DraftKey  = "draft"
	ScreenKey = "screen"
	RecordKey = "record"
	// FilesKey holds what a screen needs to know of the app's attachment fields: at /_files/<field>,
	// their limits (attachments.go). It starts with an underscore, as RevisionKey does, so no field
	// or summary can have its name.
	FilesKey = "_files"
)

// ErrInvalid matches every validation error (errors.Is); their messages say what is wrong.
var ErrInvalid = errors.New("invalid request")

// invalidError is a validation error with a message for the caller. The API answers it with
// its code and params, for the web app to say it in the user's language; the agent's tools
// give the model the message. Errors only a broken client (or a broken app) causes have
// apierr.CodeBadRequest.
type invalidError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *invalidError) Error() string        { return e.msg }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

// invalidf is a validation error only a broken client or app causes.
func invalidf(format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...), code: apierr.CodeBadRequest}
}

// invalidAs is a validation error people can cause, with its code and the params of its
// translated message.
func invalidAs(code apierr.Code, params apierr.Params, format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...), code: code, params: params}
}

// Check validates the definition strictly, the screen against the catalog among it, and
// returns it as it will be stored: the components re-encoded, string limits filled in.
func (d Definition) Check() (Definition, error) {
	if d.DefinitionVersion == 0 {
		d.DefinitionVersion = DefinitionVersion
	}
	if d.DefinitionVersion < 1 || d.DefinitionVersion > DefinitionVersion {
		return Definition{}, invalidAs(apierr.CodeAppDefinitionVersion, apierr.Params{"version": d.DefinitionVersion, "supported": DefinitionVersion},
			"definition_version %d isn't supported: this server reads up to %d", d.DefinitionVersion, DefinitionVersion)
	}
	fields := map[string]Field{}
	// What the definition uses that only version 2 has, for a definition that says version 1.
	var uses2 []string
	if len(d.Collection.Fields) > MaxFields {
		return Definition{}, invalidf("collection: at most %d fields", MaxFields)
	}
	out := d
	out.Collection.Fields = make([]Field, len(d.Collection.Fields))
	for i, f := range d.Collection.Fields {
		if !namePattern.MatchString(f.Name) || reserved[f.Name] {
			return Definition{}, invalidf("collection: field %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _, and not items, draft or id", f.Name)
		}
		if _, dup := fields[f.Name]; dup {
			return Definition{}, invalidf("collection: field %q is declared twice", f.Name)
		}
		switch f.Type {
		case TypeString:
			if f.MaxLength == 0 {
				f.MaxLength = DefaultMaxLength
			}
			if f.MaxLength < 1 || f.MaxLength > MaxMaxLength {
				return Definition{}, invalidf("collection: field %q: max_length is 1 to %d", f.Name, MaxMaxLength)
			}
		case TypeNumber:
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				return Definition{}, invalidf("collection: field %q: min is more than max", f.Name)
			}
		case TypeBoolean, TypeDate:
		case TypeTime:
			uses2 = append(uses2, "a time field")
		case TypeChoice:
			uses2 = append(uses2, "a choice field")
			if len(f.Options) < 1 || len(f.Options) > MaxOptions {
				return Definition{}, invalidf("collection: field %q: a choice has 1 to %d options", f.Name, MaxOptions)
			}
			seen := map[string]bool{}
			for _, o := range f.Options {
				if o == "" || o != strings.TrimSpace(o) || utf8.RuneCountInString(o) > maxOptionLen || seen[o] {
					return Definition{}, invalidf("collection: field %q: options are different texts of 1 to %d characters, with no space at either end", f.Name, maxOptionLen)
				}
				seen[o] = true
			}
		case TypeAttachment:
			uses2 = append(uses2, "an attachment field")
			if f.MaxFiles == 0 {
				f.MaxFiles = DefaultMaxFiles
			}
			if f.MaxBytes == 0 {
				f.MaxBytes = DefaultMaxFileBytes
			}
			if f.MaxFiles < 1 || f.MaxFiles > MaxMaxFiles {
				return Definition{}, invalidf("collection: field %q: max_files is 1 to %d", f.Name, MaxMaxFiles)
			}
			if f.MaxBytes < 1 || f.MaxBytes > MaxMaxFileBytes {
				return Definition{}, invalidf("collection: field %q: max_bytes is 1 to %d", f.Name, MaxMaxFileBytes)
			}
		default:
			return Definition{}, invalidf("collection: field %q: type must be string, number, boolean, date, time, choice or attachment", f.Name)
		}
		if f.Type != TypeAttachment && (f.MaxFiles != 0 || f.MaxBytes != 0) {
			return Definition{}, invalidf("collection: field %q: only an attachment field has max_files and max_bytes", f.Name)
		}
		if f.Type != TypeString && f.MaxLength != 0 {
			return Definition{}, invalidf("collection: field %q: only a string field has max_length", f.Name)
		}
		if f.Type != TypeNumber && (f.Min != nil || f.Max != nil) {
			return Definition{}, invalidf("collection: field %q: only a number field has min and max", f.Name)
		}
		if f.Type != TypeChoice && f.Options != nil {
			return Definition{}, invalidf("collection: field %q: only a choice field has options", f.Name)
		}
		fields[f.Name] = f
		out.Collection.Fields[i] = f
	}
	// Rules read other fields, so they are checked once every field is declared.
	for _, f := range out.Collection.Fields {
		if f.RequiredWhen == nil {
			continue
		}
		uses2 = append(uses2, "required_when")
		if f.Required {
			return Definition{}, invalidf("collection: field %q: it is always required, so required_when means nothing", f.Name)
		}
		if err := f.RequiredWhen.check(out.Collection); err != nil {
			return Definition{}, invalidf("collection: field %q: required_when: %v", f.Name, err)
		}
		if slices.Contains(ruleFields(*f.RequiredWhen), f.Name) {
			return Definition{}, invalidf("collection: field %q: required_when can't read the field itself", f.Name)
		}
	}
	if len(d.Actions) == 0 {
		return Definition{}, invalidf("actions: an app needs at least one")
	}
	if len(d.Actions) > maxActions {
		return Definition{}, invalidf("actions: at most %d", maxActions)
	}
	screens, err := d.screenIndex()
	if err != nil {
		return Definition{}, err
	}
	if len(d.Screens) > 0 {
		uses2 = append(uses2, "screens")
	}
	for name, a := range d.Actions {
		if !namePattern.MatchString(name) {
			return Definition{}, invalidf("actions: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", name)
		}
		switch a.Kind {
		case ActionInsert, ActionUpdate, ActionDelete, ActionNavigate:
		default:
			return Definition{}, invalidf("actions: %q: kind must be insert, update, delete or navigate", name)
		}
		if err := checkActionScreens(name, a, screens); err != nil {
			return Definition{}, err
		}
		if a.Kind == ActionNavigate || a.Then != "" {
			uses2 = append(uses2, "navigation")
		}
		if len(a.Effects) > 0 {
			uses2 = append(uses2, "effects")
		}
	}
	if len(d.Summaries) > maxSummaries {
		return Definition{}, invalidf("summaries: at most %d", maxSummaries)
	}
	for name, s := range d.Summaries {
		if !namePattern.MatchString(name) || reserved[name] {
			return Definition{}, invalidf("summaries: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _, and not items, draft or id", name)
		}
		if _, clash := fields[name]; clash {
			return Definition{}, invalidf("summaries: %q is also a field", name)
		}
		if len(d.Screens) > 0 && (name == ScreenKey || name == RecordKey) {
			return Definition{}, invalidf("summaries: %q: screen and record are the screens' own", name)
		}
		switch s.Kind {
		case "count":
			if s.Field != "" {
				return Definition{}, invalidf("summaries: %q: count has no field", name)
			}
		case "sum":
			if fields[s.Field].Type != TypeNumber {
				return Definition{}, invalidf("summaries: %q sums %q, which is not a number field", name, s.Field)
			}
		default:
			return Definition{}, invalidf("summaries: %q: kind must be count or sum", name)
		}
		if s.Where != nil {
			uses2 = append(uses2, "a summary's where")
			if err := s.Where.check(out.Collection); err != nil {
				return Definition{}, invalidf("summaries: %q: where: %v", name, err)
			}
		}
	}
	scope := &screenScope{actions: d.Actions, collection: out.Collection, screens: screens}
	var requires []string
	if len(d.Screens) > 0 {
		if len(d.Components) > 0 {
			return Definition{}, invalidf("an app has components or screens, not both")
		}
		out.Screens, requires, err = checkScreens(d.Screens, scope)
		if err != nil {
			return Definition{}, err
		}
		out.Components = nil
	} else {
		if d.Entry != "" {
			return Definition{}, invalidf("entry names the screen an app opens on, and this app has no screens")
		}
		out.Components, requires, err = checkComponents(d.Components, "root", map[string]bool{}, scope)
		if err != nil {
			return Definition{}, err
		}
	}
	// A collection with an attachment field needs a client that knows how to hold one, whatever
	// its screens draw.
	if slices.ContainsFunc(out.Collection.Fields, func(f Field) bool { return f.Type == TypeAttachment }) {
		requires = sortedKeys(setOf(append(requires, AttachmentCapability)))
	}
	contributions, slots, err := d.checkContributions(screens)
	if err != nil {
		return Definition{}, err
	}
	out.Contributions = contributions
	if contributions != nil {
		uses2 = append(uses2, "contributions")
		requires = withNeeds(requires, slots)
	}
	effects, err := d.checkEffects(out.Collection)
	if err != nil {
		return Definition{}, err
	}
	out.Requires = mergeSorted(requires, effects)
	if slices.Contains(requires, ComponentCapability("Show")) {
		uses2 = append(uses2, "the Show component")
	}
	if d.DefinitionVersion == firstVersion && len(uses2) > 0 {
		return Definition{}, invalidf("definition_version %d can't use %s: they are for version 2 (leave definition_version out for the current one)",
			d.DefinitionVersion, strings.Join(sortedKeys(setOf(uses2)), ", "))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return Definition{}, err
	}
	if len(raw) > MaxDefinitionSize {
		return Definition{}, invalidf("the definition is over %d KiB", MaxDefinitionSize>>10)
	}
	return out, nil
}

// mergeSorted joins two lists of capabilities, sorted and without repeats.
func mergeSorted(a, b []string) []string {
	all := append(slices.Clone(a), b...)
	slices.Sort(all)
	return slices.Compact(all)
}

// field returns the collection's field name.
func (c Collection) field(name string) (Field, bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Row is one stored row: its ID, its revision and its data.
type Row struct {
	ID       int64
	Revision int64
	Data     map[string]any
}

// ClientAction is what the renderer sends when a component's action fires (A2UI
// A2uiClientAction: the event name and its resolved context).
type ClientAction struct {
	Name              string         `json:"name"`
	SurfaceID         string         `json:"surfaceId"`
	SourceComponentID string         `json:"sourceComponentId"`
	Timestamp         string         `json:"timestamp"`
	Context           map[string]any `json:"context"`
	// Release is the number of the release the screen was drawn from (the one its surface was
	// asked for). Left out, the action is taken to come from the live release. See Service.act
	// for what an action from a release that is not the live one may do.
	Release *int64 `json:"release,omitempty"`
	// ExpectedRevision is, for an update or a delete, the revision of the row the person
	// saw when they acted (the "_revision" of its item). If the row has moved on, the action
	// is refused as a conflict and changes nothing. Without it the action applies to the
	// row as it is now: the web app always sends it; the agent's and scripts' callers may
	// leave it out.
	ExpectedRevision *int64 `json:"expectedRevision,omitempty"`
	// Operation is the idempotency key of the request: 8 to 64 letters, digits, - or _, made by the
	// client for one press of a button and sent again with every retry of it. A request made
	// again under the same operation changes nothing and queues no effects, and is answered
	// as the first one was; a different request under it is refused. An action that has
	// effects and no operation is given one by the server (outbox.go).
	Operation string `json:"operation,omitempty"`
}

// Message is one A2UI server-to-client message.
type Message map[string]any

// SurfaceID is the A2UI surface of the app with ID id.
func SurfaceID(id int64) string { return "app-" + strconv.FormatInt(id, 10) }

// Surface returns the messages that draw an app with its rows.
func Surface(surfaceID string, def Definition, rows []Row) []Message {
	return []Message{
		{"version": ProtocolVersion, "createSurface": map[string]any{"surfaceId": surfaceID, "catalogId": CatalogID}},
		{"version": ProtocolVersion, "updateComponents": map[string]any{"surfaceId": surfaceID, "components": def.surfaceComponents()}},
		DataModel(surfaceID, def, rows),
	}
}

// DataModel replaces the whole data model: the rows at /items (each with its "id" and
// the collection's fields), an empty form at /draft, and the summaries. An app with screens
// has the screen it is on at /screen (its entry) and the record a detail screen shows at
// /record (none).
func DataModel(surfaceID string, def Definition, rows []Row) Message {
	return dataModel(surfaceID, def, rows, def.Entry, 0)
}

// dataModel is DataModel with the screen an app with screens opens on, and, if limit is set, only
// the latest limit rows at /items: the summaries still count every row given.
func dataModel(surfaceID string, def Definition, rows []Row, screen string, limit int) Message {
	shown := rows
	if limit > 0 && len(shown) > limit {
		shown = shown[len(shown)-limit:]
	}
	value := map[string]any{"items": Items(def, shown), "draft": map[string]any{}}
	if files := def.filesModel(); files != nil {
		value[FilesKey] = files
	}
	for name, s := range def.Summaries {
		value[name] = summarize(s, def.Collection, rows)
	}
	if def.hasScreens() {
		value[ScreenKey], value[RecordKey] = screen, map[string]any{}
	}
	return dataMessage(surfaceID, "/", value)
}

func dataMessage(surfaceID, path string, value any) Message {
	return Message{"version": ProtocolVersion, "updateDataModel": map[string]any{"surfaceId": surfaceID, "path": path, "value": value}}
}

// Refreshed is what an action's answer sends once the rows may have changed. An app with one
// screen has its whole data model replaced, as it always was (the form cleared, whichever
// action ran). An app with screens has the rows and the summaries replaced and nothing else,
// so the screen the person is on and the form they are filling stay where they are; only an
// insert clears the form (clearDraft), and the record a detail screen shows is shown again.
func Refreshed(surfaceID string, def Definition, rows []Row, clearDraft bool) []Message {
	if !def.hasScreens() {
		return []Message{DataModel(surfaceID, def, rows)}
	}
	msgs := []Message{dataMessage(surfaceID, "/"+ItemsKey, Items(def, rows))}
	for _, name := range sortedKeys(def.Summaries) {
		msgs = append(msgs, dataMessage(surfaceID, "/"+name, summarize(def.Summaries[name], def.Collection, rows)))
	}
	if clearDraft {
		msgs = append(msgs, dataMessage(surfaceID, "/"+DraftKey, map[string]any{}))
	}
	return msgs
}

// RevisionKey is the key of a row's revision in its item. A field's name can't start with
// an underscore, so it never clashes with one.
const RevisionKey = "_revision"

// Items is the rows as the app shows them: each with its "id", its revision (RevisionKey)
// and the fields the collection declares, and none it no longer does. A value of another type than
// the field's (a row from before the field's type changed, which a publish acknowledged) is left
// out too, as if the row had none, but stays stored; one outside the field's limits is shown.
func Items(def Definition, rows []Row) []map[string]any {
	items := make([]map[string]any, len(rows))
	for i, r := range rows {
		items[i] = item(def, r)
	}
	return items
}

// ofType reports whether v is a value of f's type, whatever the field's limits.
func ofType(f Field, v any) bool {
	_, present, err := f.stored(v)
	return err == nil && present
}

func item(def Definition, r Row) map[string]any {
	it := map[string]any{"id": r.ID, RevisionKey: r.Revision}
	for _, f := range def.Collection.Fields {
		if v, ok := r.Data[f.Name]; ok && ofType(f, v) {
			it[f.Name] = v
		}
	}
	return it
}

// declared is the fields of data that the collection declares, as Items shows them: a value
// of another type than its field's (stored before the field was retyped) is left out.
func declared(def Definition, data map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range def.Collection.Fields {
		if v, ok := data[f.Name]; ok && ofType(f, v) {
			out[f.Name] = v
		}
	}
	return out
}

// summarize formats as text: A2UI binds text components to strings.
func summarize(s Summary, c Collection, rows []Row) string {
	if s.Where != nil {
		var kept []Row
		for _, r := range rows {
			if c.holds(*s.Where, r.Data) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	switch s.Kind {
	case "count":
		return strconv.Itoa(len(rows))
	case "sum":
		var total float64
		for _, r := range rows {
			if n, ok := r.Data[s.Field].(float64); ok {
				total += n
			}
		}
		return strconv.FormatFloat(total, 'f', 2, 64)
	default:
		return ""
	}
}

// Change is what an action does to the rows: insert Data, or update or delete row ID. Name
// is the action's event name, kept with the change; Expected is the revision the row had
// when the person saw it, if they said.
//
// A navigate action changes no row: Screen is the screen it opens and ID, for one that shows a
// record, the record. For the other kinds Then is the screen to open once the change is made.
type Change struct {
	Kind     string
	Name     string
	ID       int64
	Data     map[string]any
	Expected *int64
	Screen   string
	Then     string
}

// Plan works out what action does, from the context alone: which row, and the fields it
// sets, each checked against the collection. An update gives only the fields it changes;
// Merge applies them to the row.
func Plan(surfaceID string, def Definition, action ClientAction) (Change, error) {
	if action.SurfaceID != surfaceID {
		return Change{}, invalidf("the action is for surface %q, not %q", action.SurfaceID, surfaceID)
	}
	a, ok := def.Actions[action.Name]
	if !ok {
		return Change{}, invalidf("unknown action %q", action.Name)
	}
	ch := Change{Kind: a.Kind, Name: action.Name, Then: a.Then}
	if a.Kind == ActionNavigate {
		return planNavigate(def, a, ch, action)
	}
	if action.ExpectedRevision != nil {
		if a.Kind == ActionInsert || *action.ExpectedRevision < 1 {
			return Change{}, invalidf("expectedRevision is the revision of the row an update or a delete was made against, from 1")
		}
		ch.Expected = action.ExpectedRevision
	}
	ctx := action.Context
	if a.Kind != ActionInsert {
		id, ok := rowID(ctx["id"])
		if !ok {
			return Change{}, invalidf("%s needs the row's id", a.Kind)
		}
		ch.ID = id
	}
	if a.Kind == ActionDelete {
		for name := range ctx {
			if name != "id" {
				return Change{}, invalidf("delete takes only the row's id, not %s", name)
			}
		}
		return ch, nil
	}
	data := map[string]any{}
	for name, v := range ctx {
		if name == "id" && a.Kind == ActionUpdate {
			continue
		}
		f, ok := def.Collection.field(name)
		if !ok {
			return Change{}, invalidf("unknown field %s", name)
		}
		value, present, err := f.parse(v)
		if err != nil {
			return Change{}, err
		}
		if present {
			data[name] = value
		} else {
			data[name] = nil // cleared
		}
	}
	ch.Data = data
	if a.Kind == ActionInsert {
		row, err := def.Collection.Merge(nil, data)
		if err != nil {
			return Change{}, err
		}
		ch.Data = row
	}
	return ch, nil
}

// Merge applies changes (nil clears a field) to row and checks the result against the
// collection: required fields present. Fields of the row that the collection no longer
// declares are kept as they are, so declaring them again brings them back.
func (c Collection) Merge(row, changes map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range row {
		out[k] = v
	}
	for k, v := range changes {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	if err := c.Fits(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Fits checks a stored row against the collection: every required field present, every
// declared field's value of its type and within its limits. Undeclared fields are left
// alone.
func (c Collection) Fits(row map[string]any) error {
	for _, f := range c.Fields {
		if err := c.fitsField(f, row); err != nil {
			return err
		}
	}
	return nil
}

// fitsField checks a stored row against one of the collection's fields: what the field alone
// asks (fits), and the value its rule makes required, whatever the screen that sent the row
// showed.
func (c Collection) fitsField(f Field, row map[string]any) error {
	if err := f.fits(row); err != nil {
		return err
	}
	if v, ok := row[f.Name]; f.RequiredWhen != nil && (!ok || v == nil) && c.requires(f, row) {
		return invalidAs(apierr.CodeAppFieldRequiredWhen, apierr.Params{"field": f.Name}, "%s is required here", f.Name)
	}
	return nil
}

// requires reports whether the field is required of the row: always, or when its rule holds.
func (c Collection) requires(f Field, row map[string]any) bool {
	return f.Required || (f.RequiredWhen != nil && c.holds(*f.RequiredWhen, row))
}

// fits checks a stored row against the field alone: present if required, and of its type and
// within its limits.
func (f Field) fits(row map[string]any) error {
	v, ok := row[f.Name]
	if !ok {
		if f.Required {
			return invalidAs(apierr.CodeAppFieldRequired, apierr.Params{"field": f.Name}, "%s is required", f.Name)
		}
		return nil
	}
	_, _, err := f.parse(v)
	return err
}

// parse checks v as a value of f, from an action's context or a stored row. Form inputs
// arrive as strings, so number and boolean fields accept them as text too; blank text is
// no value (present is false).
func (f Field) parse(v any) (value any, present bool, err error) {
	if list, isList := v.([]any); isList && f.Type == TypeChoice {
		// A ChoicePicker's value is a list of what is chosen: one choice, or none.
		switch len(list) {
		case 0:
			return nil, false, nil
		case 1:
			v = list[0]
		default:
			return nil, false, invalidAs(apierr.CodeAppFieldNotOption, apierr.Params{"field": f.Name}, "%s takes one of its options, not several", f.Name)
		}
	}
	if s, isString := v.(string); isString {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, false, nil
		}
		v = s
	}
	if v == nil {
		return nil, false, nil
	}
	switch f.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, false, invalidf("%s must be text", f.Name)
		}
		if utf8.RuneCountInString(s) > f.MaxLength {
			return nil, false, invalidAs(apierr.CodeAppFieldTooLong, apierr.Params{"field": f.Name, "max": f.MaxLength},
				"%s is at most %d characters", f.Name, f.MaxLength)
		}
		return s, true, nil
	case TypeNumber:
		n, ok := number(v)
		if !ok {
			return nil, false, invalidAs(apierr.CodeAppFieldNotNumber, apierr.Params{"field": f.Name}, "%s must be a number", f.Name)
		}
		if f.Min != nil && n < *f.Min {
			return nil, false, invalidAs(apierr.CodeAppFieldTooSmall, apierr.Params{"field": f.Name, "min": *f.Min},
				"%s is at least %v", f.Name, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return nil, false, invalidAs(apierr.CodeAppFieldTooLarge, apierr.Params{"field": f.Name, "max": *f.Max},
				"%s is at most %v", f.Name, *f.Max)
		}
		return n, true, nil
	case TypeBoolean:
		switch b := v.(type) {
		case bool:
			return b, true, nil
		case string:
			if b == "true" || b == "false" {
				return b == "true", true, nil
			}
		}
		return nil, false, invalidf("%s must be true or false", f.Name)
	case TypeDate:
		s, ok := v.(string)
		if _, err := time.Parse(time.DateOnly, s); !ok || err != nil {
			return nil, false, invalidAs(apierr.CodeAppFieldNotDate, apierr.Params{"field": f.Name}, "%s must be a date, YYYY-MM-DD", f.Name)
		}
		return s, true, nil
	case TypeTime:
		s, ok := v.(string)
		if !ok || !timePattern.MatchString(s) {
			return nil, false, invalidAs(apierr.CodeAppFieldNotTime, apierr.Params{"field": f.Name}, "%s must be a time, HH:MM", f.Name)
		}
		return s, true, nil
	case TypeChoice:
		s, ok := v.(string)
		// A field whose options aren't known (Field.stored, which reads a value as a row holds
		// it, whatever the options now are) takes any text.
		if !ok || (f.Options != nil && !slices.Contains(f.Options, s)) {
			return nil, false, invalidAs(apierr.CodeAppFieldNotOption, apierr.Params{"field": f.Name}, "%s must be one of %s", f.Name, strings.Join(f.Options, ", "))
		}
		return s, true, nil
	case TypeAttachment:
		return f.parseAttachments(v)
	}
	return nil, false, invalidf("%s has an unknown type", f.Name)
}

// rowID reads a row's ID from a context: a number, or its text.
func rowID(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x >= 1 && x <= 1<<53 && x == math.Trunc(x) {
			return int64(x), true
		}
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil && n >= 1
	}
	return 0, false
}

// number accepts finite numbers only: ParseFloat also reads "NaN" and "Inf", which JSON
// can't encode, so storing one would break every later response for the app.
func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(n, 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}
