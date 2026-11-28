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
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

const ProtocolVersion = "v0.9"

const CatalogID = "https://taskto.ai/catalogs/apps/v1.json"

const DefinitionVersion = 3

const firstVersion = 1

const integrationsVersion = 3

type Definition struct {
	DefinitionVersion int `json:"definition_version"`
	copied            bool
	Requires          []string                   `json:"requires" apitypes:"nonnull"`
	Components        []json.RawMessage          `json:"components,omitempty" apitypes:"json"`
	Screens           []Screen                   `json:"screens,omitempty"`
	Entry             string                     `json:"entry,omitempty"`
	Collection        Collection                 `json:"collection"`
	Actions           map[string]Action          `json:"actions" apitypes:"nonnull"`
	Summaries         map[string]Summary         `json:"summaries,omitempty"`
	Series            map[string]Series          `json:"series,omitempty"`
	Contributions     *Contributions             `json:"contributions,omitempty"`
	Integrations      map[string]IntegrationSlot `json:"integrations,omitempty"`
	Reminders         []Reminder                 `json:"reminders,omitempty"`
}

type Collection struct {
	Fields []Field `json:"fields" apitypes:"nonnull"`
}

const (
	TypeString     = "string"
	TypeNumber     = "number"
	TypeBoolean    = "boolean"
	TypeDate       = "date"
	TypeTime       = "time"
	TypeChoice     = "choice"
	TypeAttachment = "attachment"
)

type Field struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Required     bool              `json:"required,omitempty"`
	RequiredWhen *Rule             `json:"required_when,omitempty"`
	MaxLength    int               `json:"max_length,omitempty"`
	Min          *float64          `json:"min,omitempty"`
	Max          *float64          `json:"max,omitempty"`
	Options      []string          `json:"options,omitempty"`
	OptionLabels map[string]string `json:"option_labels,omitempty"`
	MaxFiles     int               `json:"max_files,omitempty"`
	MaxBytes     int               `json:"max_bytes,omitempty"`
	Capture      string            `json:"capture,omitempty"`
	Format       string            `json:"format,omitempty"`
	Editable     bool              `json:"editable,omitempty"`
	asStored     bool
}

const (
	ActionInsert   = "insert"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionNavigate = "navigate"
)

type Action struct {
	Kind    string   `json:"kind"`
	Screen  string   `json:"screen,omitempty"`
	Then    string   `json:"then,omitempty"`
	Effects []Effect `json:"effects,omitempty"`
}

type Summary struct {
	Kind    string `json:"kind"`
	Field   string `json:"field,omitempty"`
	Where   *Rule  `json:"where,omitempty"`
	MonthOf string `json:"month_of,omitempty"`
}

const (
	MaxFields           = 50
	DefaultMaxLength    = 1000
	MaxMaxLength        = 10000
	MaxOptions          = 50
	maxOptionLen        = 100
	maxActions          = 50
	maxSummaries        = 20
	maxSeries           = 10
	DefaultMaxFiles     = 1
	MaxMaxFiles         = 10
	DefaultMaxFileBytes = 8 << 20
	MaxMaxFileBytes     = 16 << 20
	MaxDefinitionSize   = 48 << 10
	MaxRows             = 5000
	MaxDataSize         = 4 << 20
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

var reserved = map[string]bool{ItemsKey: true, DraftKey: true, "id": true}

const (
	ItemsKey  = "items"
	DraftKey  = "draft"
	ScreenKey = "screen"
	RecordKey = "record"
	FilesKey  = "_files"
	FieldsKey = "_fields"
	LabelsKey = "_labels"
)

var ErrInvalid = errors.New("invalid request")

type invalidError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *invalidError) Error() string        { return e.msg }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

func InvalidCode(err error) (code apierr.Code, params apierr.Params, ok bool) {
	var bad *invalidError
	if !errors.As(err, &bad) || bad.code == "" || bad.code == apierr.CodeBadRequest {
		return "", nil, false
	}
	return bad.code, bad.params, true
}

func invalidf(format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...), code: apierr.CodeBadRequest}
}

func invalidAs(code apierr.Code, params apierr.Params, format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...), code: code, params: params}
}

func (d Definition) Check() (Definition, error) {
	if d.DefinitionVersion == 0 {
		d.DefinitionVersion = DefinitionVersion
	}
	if d.DefinitionVersion < 1 || d.DefinitionVersion > DefinitionVersion {
		return Definition{}, invalidAs(apierr.CodeAppDefinitionVersion, apierr.Params{"version": d.DefinitionVersion, "supported": DefinitionVersion},
			"definition_version %d isn't supported: this server reads up to %d", d.DefinitionVersion, DefinitionVersion)
	}
	fields := map[string]Field{}
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
			for o, label := range f.OptionLabels {
				if !seen[o] || label == "" || label != strings.TrimSpace(label) || utf8.RuneCountInString(label) > maxOptionLen {
					return Definition{}, invalidf("collection: field %q: option_labels name an option each, with a label of 1 to %d characters, with no space at either end", f.Name, maxOptionLen)
				}
			}
		case TypeLocation:
			uses2 = append(uses2, "a location field")
			if f.Capture == "" {
				f.Capture = CaptureButton
			}
			if f.Capture != CaptureButton && f.Capture != CaptureSubmit {
				return Definition{}, invalidf("collection: field %q: capture is %q or %q", f.Name, CaptureButton, CaptureSubmit)
			}
		case TypeSignature:
			uses2 = append(uses2, "a signature field")
			if f.MaxBytes == 0 {
				f.MaxBytes = DefaultSignatureBytes
			}
			if f.MaxBytes < 1 || f.MaxBytes > MaxMaxFileBytes {
				return Definition{}, invalidf("collection: field %q: max_bytes is 1 to %d", f.Name, MaxMaxFileBytes)
			}
		case TypeBarcode:
			uses2 = append(uses2, "a barcode field")
			if f.MaxLength == 0 {
				f.MaxLength = DefaultMaxLength
			}
			if f.MaxLength < 1 || f.MaxLength > MaxMaxLength {
				return Definition{}, invalidf("collection: field %q: max_length is 1 to %d", f.Name, MaxMaxLength)
			}
			if f.Format != "" && !slices.Contains(BarcodeFormats, f.Format) {
				return Definition{}, invalidf("collection: field %q: format is one of %s", f.Name, strings.Join(BarcodeFormats, ", "))
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
			return Definition{}, invalidf("collection: field %q: type must be string, number, boolean, date, time, choice, attachment, location, signature or barcode", f.Name)
		}
		if f.Type != TypeAttachment && f.MaxFiles != 0 {
			return Definition{}, invalidf("collection: field %q: only an attachment field has max_files", f.Name)
		}
		if f.Type != TypeAttachment && f.Type != TypeSignature && f.MaxBytes != 0 {
			return Definition{}, invalidf("collection: field %q: only an attachment or a signature field has max_bytes", f.Name)
		}
		if f.Type != TypeString && f.Type != TypeBarcode && f.MaxLength != 0 {
			return Definition{}, invalidf("collection: field %q: only a string field or a barcode field has max_length", f.Name)
		}
		if f.Type != TypeLocation && f.Capture != "" {
			return Definition{}, invalidf("collection: field %q: only a location field has capture", f.Name)
		}
		if f.Type != TypeBarcode && f.Format != "" {
			return Definition{}, invalidf("collection: field %q: only a barcode field has format", f.Name)
		}
		if f.Type != TypeLocation && f.Type != TypeSignature && f.Editable {
			return Definition{}, invalidf("collection: field %q: only a location or a signature field has editable", f.Name)
		}
		if f.Type != TypeNumber && (f.Min != nil || f.Max != nil) {
			return Definition{}, invalidf("collection: field %q: only a number field has min and max", f.Name)
		}
		if f.Type != TypeChoice && (f.Options != nil || f.OptionLabels != nil) {
			return Definition{}, invalidf("collection: field %q: only a choice field has options and option_labels", f.Name)
		}
		fields[f.Name] = f
		out.Collection.Fields[i] = f
	}
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
	monthly := false
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
		if s.MonthOf != "" {
			uses2 = append(uses2, "a summary's month_of")
			monthly = true
			if fields[s.MonthOf].Type != TypeDate {
				return Definition{}, invalidf("summaries: %q: month_of names %q, which is not a date field", name, s.MonthOf)
			}
		}
		if s.Where != nil {
			uses2 = append(uses2, "a summary's where")
			if err := s.Where.check(out.Collection); err != nil {
				return Definition{}, invalidf("summaries: %q: where: %v", name, err)
			}
		}
	}
	if err := d.checkSeries(out.Collection); err != nil {
		return Definition{}, err
	}
	if len(d.Series) > 0 {
		uses2 = append(uses2, "series")
	}
	scope := &screenScope{actions: d.Actions, collection: out.Collection, screens: screens, copied: d.copied}
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
	for _, f := range out.Collection.Fields {
		if capability, ok := fieldCapabilities[f.Type]; ok {
			requires = sortedKeys(setOf(append(requires, capability)))
		}
	}
	if usesLabels(out.Screens, out.Components) {
		requires = sortedKeys(setOf(append(requires, LabelsCapability)))
	}
	if monthly {
		requires = sortedKeys(setOf(append(requires, SummaryMonthCapability)))
	}
	if len(d.Series) > 0 {
		requires = sortedKeys(setOf(append(requires, SeriesCapability)))
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
	usesReminders, err := d.checkReminders(out.Collection)
	if err != nil {
		return Definition{}, err
	}
	if usesReminders {
		effects = mergeSorted(effects, []string{RemindersCapability})
	}
	out.Requires = mergeSorted(requires, effects)
	if slices.Contains(requires, ComponentCapability("Show")) {
		uses2 = append(uses2, "the Show component")
	}
	if d.DefinitionVersion == firstVersion && len(uses2) > 0 {
		return Definition{}, invalidf("definition_version %d can't use %s: they are for version 2 (leave definition_version out for the current one)",
			d.DefinitionVersion, strings.Join(sortedKeys(setOf(uses2)), ", "))
	}
	usesIntegrations, err := d.checkIntegrations(out.Collection)
	if err != nil {
		return Definition{}, err
	}
	if usesIntegrations && d.DefinitionVersion < integrationsVersion {
		return Definition{}, invalidf("definition_version %d can't use integrations: they are for version %d (leave definition_version out for the current one)",
			d.DefinitionVersion, integrationsVersion)
	}
	if usesReminders && d.DefinitionVersion < remindersVersion {
		return Definition{}, invalidf("definition_version %d can't use reminders: they are for version %d (leave definition_version out for the current one)",
			d.DefinitionVersion, remindersVersion)
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

func usesLabels(screens []Screen, components []json.RawMessage) bool {
	var found func(v any) bool
	found = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if path, ok := x["path"].(string); ok && slices.Contains(strings.Split(path, "/"), LabelsKey) {
				return true
			}
			for _, child := range x {
				if found(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if found(child) {
					return true
				}
			}
		}
		return false
	}
	raw, err := json.Marshal(struct {
		Screens    []Screen          `json:"screens"`
		Components []json.RawMessage `json:"components"`
	}{screens, components})
	if err != nil {
		return false
	}
	var tree any
	return json.Unmarshal(raw, &tree) == nil && found(tree)
}

func mergeSorted(a, b []string) []string {
	all := append(slices.Clone(a), b...)
	slices.Sort(all)
	return slices.Compact(all)
}

func (c Collection) field(name string) (Field, bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

type Row struct {
	ID       int64
	Revision int64
	Data     map[string]any
}

type ClientAction struct {
	Name              string         `json:"name"`
	SurfaceID         string         `json:"surfaceId"`
	SourceComponentID string         `json:"sourceComponentId"`
	Timestamp         string         `json:"timestamp"`
	Context           map[string]any `json:"context"`
	Release           *int64         `json:"release,omitempty"`
	ExpectedRevision  *int64         `json:"expectedRevision,omitempty"`
	Operation         string         `json:"operation,omitempty"`
	Exact             bool           `json:"-"`
}

type Message map[string]any

func SurfaceID(id int64) string { return "app-" + strconv.FormatInt(id, 10) }

func Surface(surfaceID string, def Definition, rows []Row) []Message {
	return []Message{
		{"version": ProtocolVersion, "createSurface": map[string]any{"surfaceId": surfaceID, "catalogId": CatalogID}},
		{"version": ProtocolVersion, "updateComponents": map[string]any{"surfaceId": surfaceID, "components": def.surfaceComponents()}},
		DataModel(surfaceID, def, rows),
	}
}

func DataModel(surfaceID string, def Definition, rows []Row) Message {
	return dataModel(surfaceID, def, rows, def.Entry, 0)
}

func dataModel(surfaceID string, def Definition, rows []Row, screen string, limit int) Message {
	shown := rows
	if limit > 0 && len(shown) > limit {
		shown = shown[len(shown)-limit:]
	}
	value := map[string]any{"items": Items(def, shown), "draft": map[string]any{}}
	now := seriesNow()
	if files := def.filesModel(); files != nil {
		value[FilesKey] = files
	}
	if fields := def.fieldsModel(); fields != nil {
		value[FieldsKey] = fields
	}
	for name, s := range def.Summaries {
		value[name] = summarize(s, def.Collection, rows)
	}
	for name, s := range def.Series {
		value[name] = computeSeries(s, def.Collection, rows, now)
	}
	if def.hasScreens() {
		value[ScreenKey], value[RecordKey] = screen, map[string]any{}
	}
	return dataMessage(surfaceID, "/", value)
}

func dataMessage(surfaceID, path string, value any) Message {
	return Message{"version": ProtocolVersion, "updateDataModel": map[string]any{"surfaceId": surfaceID, "path": path, "value": value}}
}

func Refreshed(surfaceID string, def Definition, rows []Row, clearDraft bool) []Message {
	if !def.hasScreens() {
		return []Message{DataModel(surfaceID, def, rows)}
	}
	msgs := []Message{dataMessage(surfaceID, "/"+ItemsKey, Items(def, rows))}
	now := seriesNow()
	for _, name := range sortedKeys(def.Summaries) {
		msgs = append(msgs, dataMessage(surfaceID, "/"+name, summarize(def.Summaries[name], def.Collection, rows)))
	}
	for _, name := range sortedKeys(def.Series) {
		msgs = append(msgs, dataMessage(surfaceID, "/"+name, computeSeries(def.Series[name], def.Collection, rows, now)))
	}
	if clearDraft {
		msgs = append(msgs, dataMessage(surfaceID, "/"+DraftKey, map[string]any{}))
	}
	return msgs
}

const RevisionKey = "_revision"

func Items(def Definition, rows []Row) []map[string]any {
	items := make([]map[string]any, len(rows))
	for i, r := range rows {
		items[i] = item(def, r)
	}
	return items
}

func ofType(f Field, v any) bool {
	_, present, err := f.stored(v)
	return err == nil && present
}

func item(def Definition, r Row) map[string]any {
	it := map[string]any{"id": r.ID, RevisionKey: r.Revision}
	labels := map[string]any{}
	for _, f := range def.Collection.Fields {
		if v, ok := r.Data[f.Name]; ok && ofType(f, v) {
			it[f.Name] = v
			if s, isText := v.(string); isText && f.Type == TypeChoice {
				labels[f.Name] = f.label(s)
			}
		}
	}
	if len(labels) > 0 {
		it[LabelsKey] = labels
	}
	return it
}

func (f Field) label(value string) string {
	if l, ok := f.OptionLabels[value]; ok {
		return l
	}
	return value
}

func declared(def Definition, data map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range def.Collection.Fields {
		if v, ok := data[f.Name]; ok && ofType(f, v) {
			out[f.Name] = v
		}
	}
	return out
}

var currentMonth = func() string { return time.Now().UTC().Format("2006-01") }

func inMonth(c Collection, name string, row map[string]any, month string) bool {
	f, ok := c.field(name)
	if !ok {
		return false
	}
	v, present := row[name]
	d, _ := v.(string)
	return present && ofType(f, v) && strings.HasPrefix(d, month+"-")
}

func summarize(s Summary, c Collection, rows []Row) string {
	if s.Where != nil || s.MonthOf != "" {
		month := currentMonth()
		var kept []Row
		for _, r := range rows {
			if s.Where != nil && !c.holds(*s.Where, r.Data) {
				continue
			}
			if s.MonthOf != "" && !inMonth(c, s.MonthOf, r.Data, month) {
				continue
			}
			kept = append(kept, r)
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

type Change struct {
	Kind     string
	Name     string
	ID       int64
	Data     map[string]any
	Expected *int64
	Screen   string
	Then     string
}

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
			data[name] = nil
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

func (c Collection) Merge(row, changes map[string]any) (map[string]any, error) {
	out := merged(row, changes)
	if err := c.Fits(out); err != nil {
		return nil, err
	}
	return out, nil
}

func merged(row, changes map[string]any) map[string]any {
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
	return out
}

func (c Collection) Fits(row map[string]any) error {
	for _, f := range c.Fields {
		if err := c.fitsField(f, row); err != nil {
			return err
		}
	}
	return nil
}

func (c Collection) fitsField(f Field, row map[string]any) error {
	if err := f.fits(row); err != nil {
		return err
	}
	if v, ok := row[f.Name]; f.RequiredWhen != nil && (!ok || v == nil) && c.requires(f, row) {
		return invalidAs(apierr.CodeAppFieldRequiredWhen, apierr.Params{"field": f.Name}, "%s is required here", f.Name)
	}
	return nil
}

func (c Collection) requires(f Field, row map[string]any) bool {
	return f.Required || (f.RequiredWhen != nil && c.holds(*f.RequiredWhen, row))
}

func (f Field) fits(row map[string]any) error {
	v, ok := row[f.Name]
	if !ok {
		if f.Required {
			return invalidAs(apierr.CodeAppFieldRequired, apierr.Params{"field": f.Name}, "%s is required", f.Name)
		}
		return nil
	}
	f.asStored = true
	_, _, err := f.parse(v)
	return err
}

func (f Field) parse(v any) (value any, present bool, err error) {
	if list, isList := v.([]any); isList && f.Type == TypeChoice {
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
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return nil, false, nil
		}
		if f.Type == TypeBarcode && strings.IndexFunc(s, unicode.IsControl) >= 0 {
			return nil, false, f.badBarcode()
		}
		v = trimmed
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
		if !ok || (f.Options != nil && !slices.Contains(f.Options, s)) {
			return nil, false, invalidAs(apierr.CodeAppFieldNotOption, apierr.Params{"field": f.Name}, "%s must be one of %s", f.Name, strings.Join(f.Options, ", "))
		}
		return s, true, nil
	case TypeAttachment:
		return f.parseAttachments(v)
	case TypeLocation:
		return f.parseLocation(v)
	case TypeSignature:
		return f.parseSignature(v)
	case TypeBarcode:
		s, ok := v.(string)
		if !ok {
			return nil, false, invalidAs(apierr.CodeAppFieldNotBarcode, apierr.Params{"field": f.Name, "format": f.Format}, "%s must be text", f.Name)
		}
		return f.parseBarcode(s)
	}
	return nil, false, invalidf("%s has an unknown type", f.Name)
}

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
