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

func (s *Service) space(ctx context.Context, a Actor, spaceID int64) (access.Space, error) {
	sp, err := s.access.Check(ctx, a.UserID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return access.Space{}, ErrNotFound
	}
	return sp, err
}

func (s *Service) ListApps(ctx context.Context, a Actor, spaceID int64, withArchived bool) ([]App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listApps(ctx, sp, a, withArchived)
}

func (s *Service) ShowApp(ctx context.Context, a Actor, spaceID, appID int64) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.getApp(ctx, sp, a, appID)
	return forAgent(ap), err
}

func (s *Service) AppIntegrations(ctx context.Context, a Actor, spaceID, appID int64) (IntegrationStatus, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return IntegrationStatus{}, err
	}
	return s.integrationStatus(ctx, sp, a, appID)
}

func forAgent(ap App) App {
	if ap.Live != nil {
		live := *ap.Live
		live.Definition = nil
		ap.Live = &live
	}
	return ap
}

func (s *Service) CreateApp(ctx context.Context, a Actor, spaceID int64, na NewApp) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.createApp(ctx, sp, a, na)
	return forAgent(ap), err
}

func (s *Service) UpdateApp(ctx context.Context, a Actor, spaceID, appID int64, ch Changes) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.updateApp(ctx, sp, a, appID, ch)
	return forAgent(ap), err
}

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

func (s *Service) ListEffects(ctx context.Context, a Actor, spaceID, appID int64, f EffectFilter) (EffectList, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return EffectList{}, err
	}
	return s.listEffects(ctx, sp, a, appID, f)
}

func (s *Service) ListReleases(ctx context.Context, a Actor, spaceID, appID int64) ([]Release, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listReleases(ctx, sp, a, appID)
}

func (s *Service) AppRoles(ctx context.Context, a Actor, spaceID, appID int64) ([]access.AppGrant, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.appRoles(ctx, sp, a, appID)
}

func (s *Service) ListRows(ctx context.Context, a Actor, spaceID, appID int64) ([]map[string]any, error) {
	_, _, items, err := s.snapshotRows(ctx, a, spaceID, appID, nil)
	return items, err
}

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
		if ap, err = app(ctx, tx, sp.ID, appID); err != nil {
			return err
		}
		rows, err := loadSpaceRows(ctx, tx, sp.ID, ap.ID, readable(acc, a.UserID))
		if err != nil {
			return err
		}
		def := ap.draftDef
		if ap.liveDef != nil {
			def = *ap.liveDef
		}
		items = Items(def, rows)
		return describeAttachments(ctx, tx, sp.ID, ap.ID, def, items)
	})
	if err != nil {
		return App{}, access.AppAccess{}, nil, err
	}
	return ap, acc, items, nil
}

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

type ActionInfo struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Allowed bool     `json:"allowed"`
	Effects []string `json:"effects,omitempty"`
}

type ActionList struct {
	Release    int64            `json:"release"`
	Access     access.AppAccess `json:"access"`
	Collection Collection       `json:"collection"`
	Actions    []ActionInfo     `json:"actions"`
}

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

const actionsReads = 3

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

type ActionRun struct {
	Action           string
	Context          map[string]any
	ExpectedRevision *int64
	ExpectedRelease  int64
	Operation        string
}

type ActionResult struct {
	Action        string `json:"action"`
	Kind          string `json:"kind"`
	ID            int64  `json:"id,omitempty"`
	Revision      int64  `json:"revision,omitempty"`
	Changed       bool   `json:"changed"`
	Replayed      bool   `json:"replayed,omitempty"`
	EffectsQueued int    `json:"effects_queued"`
}

func (s *Service) RunAction(ctx context.Context, a Actor, spaceID, appID int64, run ActionRun) (ActionResult, error) {
	res, err := s.runAction(ctx, a, spaceID, appID, run)
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

type RowsPage struct {
	Rows       []map[string]any  `json:"rows"`
	NextCursor *string           `json:"next_cursor"`
	Total      int               `json:"total"`
	Aggregates []AggregateResult `json:"aggregates"`
}

//go:embed examples/*.json
var examplesFS embed.FS

type Example struct {
	Name       string     `json:"name"`
	Definition Definition `json:"definition"`
}

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

type CatalogReference struct {
	Protocol          string                       `json:"protocol"`
	DefinitionVersion int                          `json:"definition_version"`
	Capabilities      []string                     `json:"capabilities"`
	Components        map[string]map[string]string `json:"components"`
	Functions         map[string]map[string]string `json:"functions"`
	Rules             []string                     `json:"rules"`
	Examples          []Example                    `json:"examples"`
}

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
	`An app is {"name", "definition": {"components" or "screens"+"entry", "collection", "actions", "summaries", "series"?, "contributions"?}}. definition_version is 3 (leave it out); a definition that says 1 can't use screens, time or choice fields, rules, navigation, series or contributions, and version 2 can't use integrations.`,
	`components: the screen of an app with one, at most 300; one has the id "root". Use only the components and props listed; anything else is refused. Screens is the server's own and can't be used.`,
	`screens: an app with several has "screens": [{"name", "root", "record"?, "components"}] and "entry" (the screen it opens on) instead of components, at most 12 screens and 300 components in all. Every component id is unique across the screens (not "root", which is the server's), a screen's "root" is the id of its root component, and a component refers only to its own screen's. The data model's /screen says which is shown.`,
	`Moving between screens: an action {"kind": "navigate", "screen": "<name>"} that a Button's event names, with no context; for a screen with "record": true the event's context is {"id": <the row's id>} (a path such as {"path": "id"} in a List template, or "/record/id" on a detail screen) and the screen shows that row at /record (id, _revision and the fields). Navigating changes no row and keeps the form at /draft, so what was typed survives going back and forth. An insert, update or delete may add "then": "<screen>" to open one after it runs.`,
	`Dynamic values are a literal, a data binding {"path": "/abs/path"} (or a path relative to the item inside a List template), or a function call {"call", "args"}.`,
	`A child list is ["id", …] or a template {"componentId", "path": "/items"} drawing the component once per row; inside it, paths like "amount" or "id" are the row's.`,
	`The data model: /items is the rows, each with its "id" and the collection's fields; /draft is an empty form to bind inputs to; each summary is at /<name>, as text; each series is at /<name> as {"x":[{"key":"2026-11","label":"2026-11"}],"series":[{"key":"","label":"","values":[3]}]}, computed over every row the viewer may read, not just the rows shown in /items.`,
	`collection.fields: [{"name", "type": "string"|"number"|"boolean"|"date"|"time"|"choice"|"attachment"|"location"|"signature"|"barcode", "required"?, "required_when"? (a rule), "max_length"? (string, default 1000, at most 10000), "min"?/"max"? (number), "options"? (choice: 1 to 50 different texts), "option_labels"? (choice: {"<option>": "<label>"}, how a stored value reads to people)}], at most 50; a date is YYYY-MM-DD, a time HH:MM (24-hour); names are lowercase letters, digits and _, not id, items or draft. Bind a time to a DateTimeInput with enableTime true and enableDate false; a choice to a ChoicePicker with variant "mutuallyExclusive" and the same options (a value outside them is refused). A row's choice holds the stored option text ("food"), which is what a Text bound to the field shows: to show the label people read ("Food"), give the field option_labels and bind to the label instead, at "_labels/<field>" in a List template or "/record/_labels/<field>" on a detail screen (its label, or the stored text where none is given; the label never changes what is stored or what a rule compares). Use the same labels in the ChoicePicker.`,
	`Rules are tiny and declarative, never code: {"field", "op": "eq"|"ne"|"lt"|"lte"|"gt"|"gte", "value": a literal of the field's type} or {"all": [rules]} or {"any": [rules]}, nested at most 3 deep, at most 16 parts; ordering is for number, date and time fields, and a field with no value equals nothing. A field's "required_when" makes it required when its rule holds of the row: the server enforces it on every insert and update, from any screen or release or from you, whatever is shown. The Show component {"when": rule, "source"?: "draft" (default, the form at /draft) | "record" (the record a detail screen shows), "child": id} draws its child only while the rule holds: it hides an input for people and is never a limit on what the server accepts. A summary may add "where": a rule, to count or sum only some rows.`,
	`actions: {"<event name>": {"kind": "insert"|"update"|"delete"|"navigate", "screen"? (navigate), "then"? (the others)}}. A Button's action {"event": {"name", "context"}} must name one. insert adds a row from the context's fields and clears /draft; update changes the fields the context gives of the row whose "id" it gives; delete removes the row whose "id" it gives.`,
	`summaries: {"<name>": {"kind": "count"} | {"kind": "sum", "field": "<number field>"}}.`,
	`series (definition version 2, up to 10): {"<name>": {"kind":"count"|"sum"|"avg", "field":"<number field>" (sum/avg only), "group_by":"<choice|boolean|string|date field>", "bucket":"day"|"week"|"month" (required for dates), "last": 12 (date buckets, 1 to 90, ending at current bucket), "split_by":"<choice or boolean field>"?, "where":<rule>?}}. Choice groups follow option order and labels; boolean groups are false then true; text groups keep the 20 most frequent values and combine the rest as other. Date buckets use UTC and weeks start Monday. A split creates one line per choice option or boolean value. Counts and sums are numbers; an average with no matching rows is null. Offline values are recomputed from downloaded rows and queued changes.`,
	`effects: an insert or update may add "effects": [...] (at most 5), what the change sets going on boards and documents once the record is saved. The kinds are fixed and nothing else is accepted, never a URL or a tool name: {"kind": "create_card", "name"?, "when"?, "space_id", "board_id", "column"?, "title", "body"?, "docs"?}, {"kind": "update_card", "card": <the name of a create_card effect>, "column"}, {"kind": "create_document", "space_id", "path", "content"?} (path ends in .md) and {"kind": "open_route", "route": "board"|"card"|"document", …}. title, body, content and path are templates: {{field}} is a field of the collection and {{id}} the record's number, and that is all (no expressions). "when" is a rule, the same language as required_when, over the record as the change leaves it: e.g. only when "result" is "fail". The server does them afterwards, once each however often a request is repeated, as the user who acted and only if they may then: the app gives no one any access. A failed effect is shown on the app, and the user who acted retries it. Space and board are IDs from list_spaces and list_boards, which you and the person who publishes must be able to write. Effects need definition_version 2.`,
	`contributions (optional, definition version 2): what the app offers the Home page and the navigation, never markup or addresses: {"widgets"?: [{"id", "title", "kind": "summaries"|"screen"|"shortcut", "size"?: "small"|"medium"|"large", "summaries"? (kind summaries: 1 to 4 of {"summary": a summary's name, "label"}), "screen"? (kind screen: a screen that shows no record, so the app needs "screens")}], at most 6, "pin"?: {"label", "screen"?}}. Titles and labels are plain text on one line (60, 40 and 30 characters at most). Each person chooses which widgets sit on their Home and which apps they pin, and the people who may publish an app of a shared space may set defaults for its members; a widget shows only what its viewer may read and runs only what they may do in the app, so a contribution grants nothing.`,
	`Saving a definition (create_app, update_app) saves the app's DRAFT: it changes nothing people use. They keep using the live release until a person who may publish the app publishes the draft, in the Apps page; you have no tool to publish. A new app is a draft only, and is not launchable until published.`,
	`Publishing checks the draft against the app's rows: removing a field that rows have values for, changing a field's type, making a field required (or conditionally required) that some row lacks, or tightening its limits or a choice's options below a stored value needs the publisher to acknowledge it by name. So prefer adding fields as optional, and keep names and types of fields that have data.`,
	`Records: list_app_actions says which of the LIVE release's actions change records and what the user's roles allow, and run_app_action runs one as the user, exactly as the app's own button would, if the release you listed (expected_release) is still the live one: an insert with the fields, an update with the row's id and the fields to change, a delete with the row's id. The user's roles in the app, the collection's rules, the locks of captured values and the action's effects all apply; the user must ask for it, and each call changes data people use. You can't set a photo, a signature or a location. To let the agent change something the app doesn't offer, add an action to the draft; it is used once a person publishes it.`,
	`Image, Video, AudioPlayer and openUrl are not available: an app never loads or opens outside addresses.`,
	`Icons: give an icon as a path. An Icon's name is {"svgPath": "<path data>"}, one SVG path you write on a 24 by 24 view box (0 0 24 24), filled in the text's colour (nonzero fill; cut a hole by drawing it the other way round). Only path data is accepted: it starts with M or m and holds only the commands MLHVCSQTAZ (either case), numbers, commas and spaces, at most 2000 characters; no quotes, tags, URLs or styles. Examples: a plus {"svgPath": "M10 4h4v6h6v4h-6v6h-4v-6H4v-4h6z"}, a back arrow {"svgPath": "M3 12l8-8v5h10v6H11v5z"}. Beside a label, put the icon and a Text in a Row: the icon is decoration. A Button (or a Modal's trigger) that shows only icons needs "accessibility": {"label": "<what it does>"} on its icon. An icon name (add, delete, …) has no drawing: where it is all a control shows it appears as the plain word, elsewhere as nothing; don't use names in new apps`,
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
	kIconName:      `{"svgPath": "<SVG path data on a 24 by 24 view box>"}: see the rule on icons (a name such as "add" is only shown as its word, and only where it is all a control shows)`,
	kRule:          `a rule: {"field", "op": eq|ne|lt|lte|gt|gte, "value": literal}, {"all": [rule, …]} or {"any": [rule, …]}`,
	kScreenRefs:    `[{"name", "child": component id}]`,
}
