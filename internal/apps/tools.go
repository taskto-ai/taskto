package apps

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
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

// InstallTemplate makes a new app in a space from a bundled template: a draft, nothing
// published, with the template's own role assignments for the user as installTemplate makes
// them (a template's roles are its definition's, not something the agent chooses).
func (s *Service) InstallTemplate(ctx context.Context, a Actor, spaceID int64, templateID string, in Install) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.installTemplate(ctx, sp, a, templateID, in)
	var status *statusError
	if errors.As(err, &status) && status.code == apierr.CodeAppTemplateNotFound {
		return App{}, invalidf("no template %q: list_app_templates has the ids", templateID)
	}
	return forAgent(ap), err
}

// DuplicateApp copies an app's definition into a new draft app, in the same space or another
// of the workspace the user may write: no rows, roles, releases or secrets come with it.
func (s *Service) DuplicateApp(ctx context.Context, a Actor, spaceID, appID int64, req DuplicateRequest) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.duplicateApp(ctx, sp, a, appID, req)
	if errors.Is(err, ErrNotPublished) {
		return App{}, invalidf(`the app has no live release to copy: ask for source "draft" if you may design it`)
	}
	return forAgent(ap), err
}

// ListEffects lists an app's effects (newest first) that the user may see: those of records
// they may read, with the same view of what they made or why they failed as the API gives.
func (s *Service) ListEffects(ctx context.Context, a Actor, spaceID, appID int64, f EffectFilter) (EffectList, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return EffectList{}, err
	}
	return s.listEffects(ctx, sp, a, appID, f)
}

// ListReleases lists an app's releases, newest first, without their definitions, for a user
// who may design or publish the app.
func (s *Service) ListReleases(ctx context.Context, a Actor, spaceID, appID int64) ([]Release, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listReleases(ctx, sp, a, appID)
}

// AppRoles returns an app's role assignments, for a user who manages them. The agent only
// reads them: there is no method here that sets them.
func (s *Service) AppRoles(ctx context.Context, a Actor, spaceID, appID int64) ([]access.AppGrant, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.appRoles(ctx, sp, a, appID)
}

// ListRows returns an app's rows as the app shows them (Items), oldest first: those the
// user may read, which for a user who reads only their own records are theirs. An offline
// app's rows are kept, so they are read too. An attachment field holds each attachment's
// metadata, not its bytes.
func (s *Service) ListRows(ctx context.Context, a Actor, spaceID, appID int64) ([]map[string]any, error) {
	_, _, items, err := s.snapshotRows(ctx, a, spaceID, appID, nil)
	return items, err
}

// snapshotRows reads an app and its rows as a's user may read them, all of one snapshot: the
// space's access, the app's roles (and so the scope of the rows), the app, its live or draft
// definition, the rows and their attachments' metadata. A demotion, a removal or a publish
// that happens meanwhile is therefore either all in what comes back or not at all: never rows
// of a scope the person no longer has, nor rows shaped by another release than the definition
// that is returned. check, if set, is asked inside the snapshot, once the access is known, and
// an error from it ends the read.
func (s *Service) snapshotRows(ctx context.Context, a Actor, spaceID, appID int64, check func(ctx context.Context, tx *sql.Tx, sp access.Space, acc access.AppAccess) error) (App, access.AppAccess, []map[string]any, error) {
	var ap App
	var acc access.AppAccess
	var items []map[string]any
	err := s.access.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
		sp, err := snap.Check(ctx, a.UserID, spaceID)
		if errors.Is(err, access.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if acc, err = appAccessBy(ctx, snap, a, sp.ID, appID); err != nil {
			return err
		}
		if check != nil {
			if err := check(ctx, tx, sp, acc); err != nil {
				return err
			}
		}
		// The app and its rows are read asking for the app to be in this space: an app that
		// moved before the snapshot began is ErrNotFound, never its rows read from the space it left.
		if ap, err = app(ctx, tx, sp.ID, appID); err != nil {
			return err
		}
		rows, err := loadSpaceRows(ctx, tx, sp.ID, ap.ID, readable(acc, a.UserID))
		if err != nil {
			return err
		}
		// As the app shows them: by the live release (an unpublished app has no rows).
		def := ap.draftDef
		if ap.liveDef != nil {
			def = *ap.liveDef
		}
		items = Items(def, rows)
		// An attachment field shows what each attachment is (id, type, size, pixels), never its bytes:
		// the agent has no tool to read or to upload a file.
		return describeAttachments(ctx, tx, sp.ID, ap.ID, def, items)
	})
	if err != nil {
		return App{}, access.AppAccess{}, nil, err
	}
	return ap, acc, items, nil
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

// ActionInfo is one of an app's actions that change a record, as the agent is told of it.
type ActionInfo struct {
	Name string `json:"name"`
	// Kind is "insert", "update" or "delete".
	Kind string `json:"kind"`
	// Allowed is whether the user's roles in the app let them run it: an insert is the right to
	// submit records, an update or a delete the right to modify them (their own, or all).
	Allowed bool `json:"allowed"`
	// Effects are the kinds of what the action sets going once it has changed a record.
	Effects []string `json:"effects,omitempty"`
}

// ActionList is what an app's live release offers the agent to run, and what the user may do.
type ActionList struct {
	// Release is the live release's number.
	Release int64 `json:"release"`
	// Access is what the user may do in the app: their roles and capabilities.
	Access access.AppAccess `json:"access"`
	// Collection is the live release's: the fields an action's context may set.
	Collection Collection   `json:"collection"`
	Actions    []ActionInfo `json:"actions"`
}

// Actions lists the actions of the app's live release that change a record (navigating changes
// nothing), with whether the user may run each. Anyone who may open the app is told: its screens
// say as much to them.
//
// What the user may do and the release are read apart, and a role revoked or a release published
// between the two reads could pair one with the other: the access is read again after the
// release, and the list is made only if it is the same (else all is read again), so what is
// shown is what the user held while the release was read.
func (s *Service) Actions(ctx context.Context, a Actor, spaceID, appID int64) (ActionList, error) {
	for range actionsReads {
		sp, err := s.space(ctx, a, spaceID)
		if err != nil {
			return ActionList{}, err
		}
		acc, err := s.appAccess(ctx, a, sp.ID, appID)
		if err != nil {
			return ActionList{}, err
		}
		if s.hooks.afterActionsAccess != nil {
			s.hooks.afterActionsAccess()
		}
		ap, err := launchable(ctx, s.db, sp.ID, appID)
		if err != nil {
			return ActionList{}, err
		}
		if ap.liveDef == nil {
			return ActionList{}, ErrNotPublished
		}
		again, err := s.space(ctx, a, spaceID)
		if err != nil {
			return ActionList{}, err
		}
		accAgain, err := s.appAccess(ctx, a, sp.ID, appID)
		if err != nil {
			return ActionList{}, err
		}
		if reflect.DeepEqual(sp, again) && reflect.DeepEqual(acc, accAgain) {
			return actionList(sp, acc, ap), nil
		}
	}
	return ActionList{}, ErrStale
}

// actionsReads is how many times Actions reads again when what the user may do changes under it.
const actionsReads = 3

// actionList is what the live release of ap offers a user with access acc in space sp.
func actionList(sp access.Space, acc access.AppAccess, ap App) ActionList {
	def := *ap.liveDef
	out := ActionList{Release: ap.Live.Number, Access: acc, Collection: def.Collection, Actions: []ActionInfo{}}
	for _, name := range sortedKeys(def.Actions) {
		act := def.Actions[name]
		info := ActionInfo{Name: name, Kind: act.Kind}
		switch act.Kind {
		case ActionInsert:
			info.Allowed = acc.Submit
		case ActionUpdate, ActionDelete:
			info.Allowed = acc.ModifyRecords != access.ScopeNone
		default:
			continue
		}
		info.Allowed = info.Allowed && may(sp, access.Write) == nil
		for _, e := range act.Effects {
			info.Effects = append(info.Effects, e.Kind)
		}
		out.Actions = append(out.Actions, info)
	}
	return out
}

// ActionRun is a request to run one of an app's actions that changes a record.
type ActionRun struct {
	Action string
	// Context is what the action's button would send: the fields to set and, for an update or a
	// delete, the row's "id".
	Context map[string]any
	// ExpectedRevision is the revision of the row the caller saw, for an update or a delete; if the
	// row has moved on, nothing changes (ErrConflict).
	ExpectedRevision *int64
	// ExpectedRelease is the number of the live release the caller decided from (ActionList.Release),
	// required: the action runs only if it is the live release when it runs (ErrReleaseOutdated
	// otherwise), so what it does, its effects included, is what the caller was told. A retry under
	// the same Operation names the release of the request it repeats.
	ExpectedRelease int64
	// Operation is the request's idempotency key (ClientAction.Operation).
	Operation string
}

// ActionResult is what an action did. It names the record and its revision but carries no data:
// whether the user may read the record is for the read tools to say.
type ActionResult struct {
	Action string `json:"action"`
	Kind   string `json:"kind"`
	// ID and Revision are the record's, after the change: a deleted record's revision is that of
	// its deletion. Both are absent for a request already made under the same operation.
	ID       int64 `json:"id,omitempty"`
	Revision int64 `json:"revision,omitempty"`
	// Changed is false for an update that left the record as it was: no new revision, no effects.
	Changed bool `json:"changed"`
	// Replayed is a request made before under the same operation, which changed nothing again.
	Replayed bool `json:"replayed,omitempty"`
	// EffectsQueued is how many effects the change set going, for the server to do afterwards.
	EffectsQueued int `json:"effects_queued"`
}

// RunAction runs one of the live release's actions that change a record, as the user: the same
// code as the action route (store.go, runChange), so the user's roles, the records they may
// modify, the collection's rules, the locks of captured values, the record's history and the
// action's effects are those of the web app. The agent sets no photo, signature or location (it
// can't capture one): agentMaySet.
func (s *Service) RunAction(ctx context.Context, a Actor, spaceID, appID int64, run ActionRun) (ActionResult, error) {
	res, err := s.runAction(ctx, a, spaceID, appID, run)
	// A request this user made before under the same operation and release, and that committed, is
	// answered with its receipt even if their access has changed or the app has gone offline since
	// (as the action route does, act): an agent that lost the answer must learn that it is in.
	// The receipt names no record and carries no data; a request that did not commit is refused as
	// it would be.
	if err != nil && run.ExpectedRelease >= 1 &&
		(errors.Is(err, ErrNotFound) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrOffline)) {
		action := ClientAction{
			Name: run.Action, SurfaceID: SurfaceID(appID), Context: run.Context,
			Release: &run.ExpectedRelease, ExpectedRevision: run.ExpectedRevision, Operation: run.Operation,
		}
		if s.committed(ctx, spaceID, a, appID, action) {
			return s.actionReceipt(ctx, appID, run), nil
		}
	}
	return res, err
}

// actionReceipt is the answer to a request that committed before: it names the action and its kind
// (from the release the request was made against) and says it was replayed, and no record.
func (s *Service) actionReceipt(ctx context.Context, appID int64, run ActionRun) ActionResult {
	receipt := ActionResult{Action: run.Action, Replayed: true}
	if rel, err := loadRelease(ctx, s.db, appID, run.ExpectedRelease, true); err == nil && rel.Definition != nil {
		receipt.Kind = rel.Definition.Actions[run.Action].Kind
	}
	return receipt
}

func (s *Service) runAction(ctx context.Context, a Actor, spaceID, appID int64, run ActionRun) (ActionResult, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := may(sp, access.Read); err != nil {
		return ActionResult{}, err
	}
	if run.ExpectedRelease < 1 {
		return ActionResult{}, invalidf("expected_release is the live release's number, from list_app_actions")
	}
	action := ClientAction{
		Name: run.Action, SurfaceID: SurfaceID(appID), Context: run.Context,
		Release: &run.ExpectedRelease, Exact: true,
		ExpectedRevision: run.ExpectedRevision, Operation: run.Operation,
	}
	if s.navigates(ctx, sp, a, appID, action) {
		return ActionResult{}, invalidf("%s only opens a screen: it changes no record, so there is nothing to run", run.Action)
	}
	if err := may(sp, access.Write); err != nil {
		return ActionResult{}, err
	}
	if err := s.spendTokenAction(a, sp.WorkspaceID); err != nil {
		// A request that committed before is answered over the budget too, as a replay, for an
		// agent that lost the answer; only a new operation is refused.
		if IsRateLimited(err) && s.committed(ctx, sp.ID, a, appID, action) {
			return s.actionReceipt(ctx, appID, run), nil
		}
		return ActionResult{}, err
	}
	done, err := s.runChange(ctx, sp, a, appID, action)
	if err != nil {
		return ActionResult{}, err
	}
	res := ActionResult{Action: run.Action, Kind: done.change.Kind, EffectsQueued: done.effects, Replayed: done.replayed}
	if !done.replayed {
		res.ID, res.Revision, res.Changed = done.ap.id, done.ap.revision, done.ap.changed
	}
	return res, nil
}

// agentMaySet refuses an action of the agent that sets a photo, a signature or a location: it
// can read them, never capture one, so what it stores of them would be made up. Leaving them out
// of an update keeps them as they were.
func agentMaySet(def Definition, ch Change) error {
	for name := range ch.Data {
		f, _ := def.Collection.field(name)
		switch f.Type {
		case TypeAttachment, TypeSignature, TypeLocation:
			return invalidf("%s is a %s: only a person can capture one, leave it out", name, f.Type)
		}
	}
	return nil
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
	`An app is {"name", "definition": {"components" or "screens"+"entry", "collection", "actions", "summaries", "contributions"?}}. definition_version is 2 (leave it out); a definition that says 1 can't use screens, time or choice fields, rules, navigation or contributions.`,
	`components: the screen of an app with one, at most 300; one has the id "root". Use only the components and props listed; anything else is refused. Screens is the server's own and can't be used.`,
	`screens: an app with several has "screens": [{"name", "root", "record"?, "components"}] and "entry" (the screen it opens on) instead of components, at most 12 screens and 300 components in all. Every component id is unique across the screens (not "root", which is the server's), a screen's "root" is the id of its root component, and a component refers only to its own screen's. The data model's /screen says which is shown.`,
	`Moving between screens: an action {"kind": "navigate", "screen": "<name>"} that a Button's event names, with no context; for a screen with "record": true the event's context is {"id": <the row's id>} (a path such as {"path": "id"} in a List template, or "/record/id" on a detail screen) and the screen shows that row at /record (id, _revision and the fields). Navigating changes no row and keeps the form at /draft, so what was typed survives going back and forth. An insert, update or delete may add "then": "<screen>" to open one after it runs.`,
	`Dynamic values are a literal, a data binding {"path": "/abs/path"} (or a path relative to the item inside a List template), or a function call {"call", "args"}.`,
	`A child list is ["id", …] or a template {"componentId", "path": "/items"} drawing the component once per row; inside it, paths like "amount" or "id" are the row's.`,
	`The data model: /items is the rows, each with its "id" and the collection's fields; /draft is an empty form to bind inputs to; each summary is at /<name>, as text.`,
	`collection.fields: [{"name", "type": "string"|"number"|"boolean"|"date"|"time"|"choice"|"attachment"|"location"|"signature"|"barcode", "required"?, "required_when"? (a rule), "max_length"? (string, default 1000, at most 10000), "min"?/"max"? (number), "options"? (choice: 1 to 50 different texts), "option_labels"? (choice: {"<option>": "<label>"}, how a stored value reads to people)}], at most 50; a date is YYYY-MM-DD, a time HH:MM (24-hour); names are lowercase letters, digits and _, not id, items or draft. Bind a time to a DateTimeInput with enableTime true and enableDate false; a choice to a ChoicePicker with variant "mutuallyExclusive" and the same options (a value outside them is refused). A row's choice holds the stored option text ("food"), which is what a Text bound to the field shows: to show the label people read ("Food"), give the field option_labels and bind to the label instead, at "_labels/<field>" in a List template or "/record/_labels/<field>" on a detail screen (its label, or the stored text where none is given; the label never changes what is stored or what a rule compares). Use the same labels in the ChoicePicker.`,
	`Rules are tiny and declarative, never code: {"field", "op": "eq"|"ne"|"lt"|"lte"|"gt"|"gte", "value": a literal of the field's type} or {"all": [rules]} or {"any": [rules]}, nested at most 3 deep, at most 16 parts; ordering is for number, date and time fields, and a field with no value equals nothing. A field's "required_when" makes it required when its rule holds of the row: the server enforces it on every insert and update, from any screen or release or from you, whatever is shown. The Show component {"when": rule, "source"?: "draft" (default, the form at /draft) | "record" (the record a detail screen shows), "child": id} draws its child only while the rule holds: it hides an input for people and is never a limit on what the server accepts. A summary may add "where": a rule, to count or sum only some rows.`,
	`actions: {"<event name>": {"kind": "insert"|"update"|"delete"|"navigate", "screen"? (navigate), "then"? (the others)}}. A Button's action {"event": {"name", "context"}} must name one. insert adds a row from the context's fields and clears /draft; update changes the fields the context gives of the row whose "id" it gives; delete removes the row whose "id" it gives.`,
	`summaries: {"<name>": {"kind": "count"} | {"kind": "sum", "field": "<number field>"}}.`,
	`effects: an insert or update may add "effects": [...] (at most 5), what the change sets going on boards and documents once the record is saved. The kinds are fixed and nothing else is accepted, never a URL or a tool name: {"kind": "create_card", "name"?, "when"?, "space_id", "board_id", "column"?, "title", "body"?, "docs"?}, {"kind": "update_card", "card": <the name of a create_card effect>, "column"}, {"kind": "create_document", "space_id", "path", "content"?} (path ends in .md) and {"kind": "open_route", "route": "board"|"card"|"document", …}. title, body, content and path are templates: {{field}} is a field of the collection and {{id}} the record's number, and that is all (no expressions). "when" is a rule, the same language as required_when, over the record as the change leaves it: e.g. only when "result" is "fail". The server does them afterwards, once each however often a request is repeated, as the user who acted and only if they may then: the app gives no one any access. A failed effect is shown on the app, and the user who acted retries it. Space and board are IDs from list_spaces and list_boards, which you and the person who publishes must be able to write. Effects need definition_version 2.`,
	`contributions (optional, definition version 2): what the app offers the Home page and the navigation, never markup or addresses: {"widgets"?: [{"id", "title", "kind": "summaries"|"screen"|"shortcut", "size"?: "small"|"medium"|"large", "summaries"? (kind summaries: 1 to 4 of {"summary": a summary's name, "label"}), "screen"? (kind screen: a screen that shows no record, so the app needs "screens")}], at most 6, "pin"?: {"label", "screen"?}}. Titles and labels are plain text on one line (60, 40 and 30 characters at most). Each person chooses which widgets sit on their Home and which apps they pin, and the people who may publish an app of a shared space may set defaults for its members; a widget shows only what its viewer may read and runs only what they may do in the app, so a contribution grants nothing.`,
	`Saving a definition (create_app, update_app) saves the app's DRAFT: it changes nothing people use. They keep using the live release until a person who may publish the app publishes the draft, in the Apps page; you have no tool to publish. A new app is a draft only, and is not launchable until published.`,
	`Publishing checks the draft against the app's rows: removing a field that rows have values for, changing a field's type, making a field required (or conditionally required) that some row lacks, or tightening its limits or a choice's options below a stored value needs the publisher to acknowledge it by name. So prefer adding fields as optional, and keep names and types of fields that have data.`,
	`Records: list_app_actions says which of the LIVE release's actions change records and what the user's roles allow, and run_app_action runs one as the user, exactly as the app's own button would, if the release you listed (expected_release) is still the live one: an insert with the fields, an update with the row's id and the fields to change, a delete with the row's id. The user's roles in the app, the collection's rules, the locks of captured values and the action's effects all apply; the user must ask for it, and each call changes data people use. You can't set a photo, a signature or a location. To let the agent change something the app doesn't offer, add an action to the draft; it is used once a person publishes it.`,
	`Image, Video, AudioPlayer and openUrl are not available: an app never loads or opens outside addresses.`,
	`Icons: an Icon's name is {"svgPath": "<path data>"}, one SVG path you write on a 24 by 24 view box (0 0 24 24), filled in the text's colour (nonzero fill; cut a hole by drawing it the other way round). Only path data is accepted: it starts with M or m and holds only the commands MLHVCSQTAZ (either case), numbers, commas and spaces, at most 2000 characters; no quotes, tags, URLs or styles. Examples: a plus {"svgPath": "M11 5h2v6h6v2h-6v6h-2v-6H5v-2h6z"}, a back arrow {"svgPath": "M20 11H7.8l5.6-5.6L12 4l-8 8 8 8 1.4-1.4L7.8 13H20z"}. Beside a label, put the icon and a Text in a Row: the icon is decoration. A Button (or a Modal's trigger) that shows only icons needs "accessibility": {"label": "<what it does>"} on its icon. Icon names (add, star, …) are accepted for old apps but draw nothing.`,
	`Photos: a collection field {"name", "type": "attachment", "max_files"? (1 to 10, default 1), "max_bytes"? (per file, at most 16 MiB, default 8 MiB), "required"?} holds JPEG, PNG or GIF pictures, which people upload with the PhotoInput component {"field": "<that field>", "label"?, "value": {"path": "/draft/<field>"}} (camera or library; on a detail screen with "source": "record" use "/record/<field>" and an update action whose context gives the field) and see with Photos {"value": {"path": "<field>"} inside a List template, or "/record/<field>", "label"?}, which draws thumbnails from this server, never from an outside address. The action's context passes the field's binding ({"path": "/draft/<field>"}) like any other. An attachment field needs definition_version 2, can't be used in a rule, and is stored as a list of attachment ids; the pictures themselves are separate resources that only people who may read the record can fetch. You can read their ids and metadata (list_app_rows) but you cannot upload, read or attach a picture yourself.`,
	`Capture fields (definition version 2): a "location" field {"name", "type": "location", "capture"? ("button", the default, or "submit"), "editable"?, "required"?} holds {lat, lon, accuracy_m, captured_at, source: "device"|"manual"}, which people take with the LocationInput component {"field", "label"?, "value": {"path": "/draft/<field>"}, "readOnly"? (only shows what is stored)}: the browser asks for the position once, on the person's button or (capture "submit") when they press the form's submit button, never in the background; where it can't (denied, no support) the person types the coordinates, stored as source "manual". For capture "submit" the Button's event context must pass the field under its own name ({"<field>": {"path": "/draft/<field>"}}). A "signature" field {"name", "type": "signature", "max_bytes"?, "editable"?} holds one PNG, drawn (or confirmed by typing a name) with SignatureInput {"field", "label"?, "value": {"path": "/draft/<field>"}, "readOnly"?} and stored like a photo; show it with Photos. A stored location or signature is evidence: the server refuses every later change or clearing of it, from any screen, release or the records explorer, unless the field says "editable": true. A "barcode" field {"name", "type": "barcode", "format"? (aztec, codabar, code_39, code_93, code_128, data_matrix, ean_8, ean_13, itf, pdf417, qr_code, upc_a, upc_e), "max_length"?} holds the text of a code, scanned with the camera where the browser can and always typed otherwise, with BarcodeInput {"field", "label"?, "value": {"path": "/draft/<field>"}}; ean_8, ean_13 and upc_a are checked as digits with their check digit. None of the three can be used in a rule except a barcode (eq, ne); a location or a signature can't be filtered or sorted by. Text fields get a dictation button of their own, needing a connection; you do nothing for it. You can read these values (list_app_rows) but cannot capture them yourself.`,
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
	kIconName:      `{"svgPath": "<SVG path data on a 24 by 24 view box>"}: see the rule on icons (a name such as "add" is accepted but draws nothing)`,
	kRule:          `a rule: {"field", "op": eq|ne|lt|lte|gt|gte, "value": literal}, {"all": [rule, …]} or {"any": [rule, …]}`,
	kScreenRefs:    `[{"name", "child": component id}]`,
}
