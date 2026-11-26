package apps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

// The catalog is every component and function an app's screen may use, and the props and
// arguments of each: A2UI v0.9's basic catalog, without the components and functions that
// load or open outside addresses (Image, Video, AudioPlayer, openUrl: an app is shared,
// and a picture from elsewhere would tell that server who opens it, and could carry the
// app's data in its address), plus TaskTo.AI's Stat, Show, PhotoInput and Photos. The last two
// are how photos get in and out without an outside address: they upload to, and draw from, this
// server's authenticated attachment routes (attachments.go). The web app's catalog
// (web/src/apps/catalog.tsx) is the same set; testdata/catalog.json is the contract between
// the two, which both test against.
//
// checkComponents validates a screen against it strictly, as the renderer's
// STRICT_VALIDATION does, without relying on the browser: an unknown component, prop,
// function or argument, a value of the wrong type, an enum value outside its list, a
// reference to a component that doesn't exist and a cycle are all refused.

// kind is the type of a prop or argument.
type kind int

const (
	kString        kind = iota // a literal string
	kNumber                    // a literal number
	kInteger                   // a literal whole number, at least 0
	kBool                      // a literal boolean
	kEnum                      // one of the prop's values
	kDynString                 // a string, a data binding, or a function returning a string
	kDynNumber                 // a number, a binding, or a function returning a number
	kDynBool                   // a boolean, a binding, or a function returning a boolean
	kDynStringList             // a list of strings, a binding, or a function returning a list
	kDynValue                  // any of the above
	kDynBoolList               // a list of at least two dynamic booleans (and, or)
	kComponentID               // the ID of another component
	kChildList                 // a list of component IDs, or a template over a data list
	kAction                    // what a button does: an event the app declares, or a function
	kChecks                    // a list of {condition, message} rules
	kAccessibility             // {label, description}, both dynamic strings
	kTabs                      // a list of {title, child}
	kOptions                   // a list of {label, value}
	kIconName                  // an icon's SVG path, its name (drawn as nothing), or a binding
	kRule                      // a rule (rules.go) over the collection's fields
	kScreenRefs                // a list of {name, child}: the host of the screens' own
)

// prop is one prop of a component, or argument of a function.
type prop struct {
	kind     kind
	required bool
	values   []string // for kEnum
}

func req(k kind) prop            { return prop{kind: k, required: true} }
func opt(k kind) prop            { return prop{kind: k} }
func enum(values ...string) prop { return prop{kind: kEnum, values: values} }
func props(m map[string]prop) map[string]prop {
	// Every component may have these.
	m["accessibility"] = opt(kAccessibility)
	m["weight"] = opt(kNumber)
	return m
}

// checkable adds the checks of an input component.
func checkable(m map[string]prop) map[string]prop {
	m["checks"] = opt(kChecks)
	return props(m)
}

var components = map[string]map[string]prop{
	"Text": props(map[string]prop{
		"text":    req(kDynString),
		"variant": enum("h1", "h2", "h3", "h4", "h5", "caption", "body"),
	}),
	"Icon": props(map[string]prop{"name": req(kIconName)}),
	"Row": props(map[string]prop{
		"children": req(kChildList),
		"justify":  enum("center", "end", "spaceAround", "spaceBetween", "spaceEvenly", "start", "stretch"),
		"align":    enum("start", "center", "end", "stretch"),
	}),
	"Column": props(map[string]prop{
		"children": req(kChildList),
		"justify":  enum("start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly", "stretch"),
		"align":    enum("center", "end", "start", "stretch"),
	}),
	"List": props(map[string]prop{
		"children":  req(kChildList),
		"direction": enum("vertical", "horizontal"),
		"align":     enum("start", "center", "end", "stretch"),
	}),
	"Card":    props(map[string]prop{"child": req(kComponentID)}),
	"Tabs":    props(map[string]prop{"tabs": req(kTabs)}),
	"Modal":   props(map[string]prop{"trigger": req(kComponentID), "content": req(kComponentID)}),
	"Divider": props(map[string]prop{"axis": enum("horizontal", "vertical")}),
	"Button": checkable(map[string]prop{
		"child":   req(kComponentID),
		"variant": enum("default", "primary", "borderless"),
		"action":  req(kAction),
	}),
	"TextField": checkable(map[string]prop{
		"label":   req(kDynString),
		"value":   opt(kDynString),
		"variant": enum("longText", "number", "shortText", "obscured"),
	}),
	"CheckBox": checkable(map[string]prop{"label": req(kDynString), "value": req(kDynBool)}),
	"ChoicePicker": checkable(map[string]prop{
		"label":        opt(kDynString),
		"variant":      enum("multipleSelection", "mutuallyExclusive"),
		"options":      req(kOptions),
		"value":        req(kDynStringList),
		"displayStyle": enum("checkbox", "chips"),
		"filterable":   opt(kBool),
	}),
	"Slider": checkable(map[string]prop{
		"label": opt(kDynString),
		"min":   opt(kNumber),
		"max":   req(kNumber),
		"value": req(kDynNumber),
	}),
	"DateTimeInput": checkable(map[string]prop{
		"value":      req(kDynString),
		"enableDate": opt(kBool),
		"enableTime": opt(kBool),
		"min":        opt(kDynString),
		"max":        opt(kDynString),
		"label":      opt(kDynString),
	}),
	// Ours: a labelled headline value, such as a total. Only these two props.
	"Stat": {"label": req(kDynString), "value": req(kDynString)},
	// Ours: draws its child only while its rule holds of the form (/draft, the default) or of
	// the record a screen shows (/record). It hides an input; it decides nothing the server
	// accepts (rules.go).
	"Show": {"when": req(kRule), "source": enum("draft", "record"), "child": req(kComponentID)},
	// Ours: a photo input for an attachment field. It takes photos from the camera or the
	// library, uploads each to the server (attachments.go) and holds their ids in value, a list
	// at a path such as /draft/photos. field names the attachment field it is for.
	"PhotoInput": {"field": req(kString), "label": opt(kDynString), "value": req(kDynStringList)},
	// Ours: draws the thumbnails of the attachments whose ids are in value, fetched from the
	// server with the viewer's own session, never from an outside address.
	"Photos": {"label": opt(kDynString), "value": req(kDynStringList)},
	// Ours: input for a location field (capture.go). It asks the browser for the position once, on
	// the person's button or, for a field whose capture is "submit", when the form is submitted, and
	// never follows it; where the position can't be had the person types it. value holds the
	// location object at a path such as /draft/<field>; readOnly only shows what is stored.
	"LocationInput": {"field": req(kString), "label": opt(kDynString), "value": req(kDynValue), "readOnly": opt(kBool)},
	// Ours: a pad to sign on, for a signature field: the PNG is uploaded like a photo and its id held
	// in value; readOnly only shows the stored signature. A typed name is the keyboard's way to sign.
	"SignatureInput": {"field": req(kString), "label": opt(kDynString), "value": req(kDynValue), "readOnly": opt(kBool)},
	// Ours: input for a barcode field: scans with the camera where the browser can, and always takes
	// the code typed. value holds its text.
	"BarcodeInput": {"field": req(kString), "label": opt(kDynString), "value": req(kDynString)},
	// Ours, and the server's: the host of an app's screens, which the server adds as the root
	// (screens.go). An app's own components may not use it.
	screensComponent: {"current": req(kDynString), "screens": req(kScreenRefs)},
}

// inputFields is the kind of field each input component is for.
var inputFields = map[string]string{
	"PhotoInput":     TypeAttachment,
	"LocationInput":  TypeLocation,
	"SignatureInput": TypeSignature,
	"BarcodeInput":   TypeBarcode,
}

// function is one function of the catalog: its arguments, what it returns, and whether it
// needs at least one of min and max.
type function struct {
	args     map[string]prop
	returns  string
	minOrMax bool
}

var functions = map[string]function{
	"required": {args: map[string]prop{"value": req(kDynValue)}, returns: "boolean"},
	"length": {args: map[string]prop{"value": req(kDynString), "min": opt(kInteger), "max": opt(kInteger)},
		returns: "boolean", minOrMax: true},
	"numeric": {args: map[string]prop{"value": req(kDynNumber), "min": opt(kNumber), "max": opt(kNumber)},
		returns: "boolean", minOrMax: true},
	"email":        {args: map[string]prop{"value": req(kDynString)}, returns: "boolean"},
	"formatString": {args: map[string]prop{"value": req(kDynString)}, returns: "string"},
	"formatNumber": {args: map[string]prop{"value": req(kDynNumber), "decimals": opt(kDynNumber), "grouping": opt(kDynBool)},
		returns: "string"},
	"formatCurrency": {args: map[string]prop{"value": req(kDynNumber), "currency": req(kDynString),
		"decimals": opt(kDynNumber), "grouping": opt(kDynBool)}, returns: "string"},
	"formatDate": {args: map[string]prop{"value": req(kDynValue), "format": req(kDynString)}, returns: "string"},
	"pluralize": {args: map[string]prop{"value": req(kDynNumber), "zero": opt(kDynString), "one": opt(kDynString),
		"two": opt(kDynString), "few": opt(kDynString), "many": opt(kDynString), "other": req(kDynString)}, returns: "string"},
	"and": {args: map[string]prop{"values": req(kDynBoolList)}, returns: "boolean"},
	"or":  {args: map[string]prop{"values": req(kDynBoolList)}, returns: "boolean"},
	"not": {args: map[string]prop{"value": req(kDynBool)}, returns: "boolean"},
}

// Capabilities are what a client's catalog can draw, named so a release can say which it needs: a
// component is component.<Name>, a function function.<name>, an effect effect.<kind>. A new component or function is a new
// capability, so an app that uses it is not drawn by a client that lacks it. The list is sorted.
func Capabilities() []string {
	var all []string
	for name := range components {
		all = append(all, ComponentCapability(name))
	}
	for name := range functions {
		all = append(all, FunctionCapability(name))
	}
	for _, kind := range EffectKinds {
		all = append(all, EffectCapability(kind))
	}
	for _, capability := range fieldCapabilities { // includes AttachmentCapability
		all = append(all, capability)
	}
	all = append(all, slotCapabilities...)
	all = append(all, SummaryMonthCapability, LabelsCapability, IconPathCapability)
	sort.Strings(all)
	return all
}

// SummaryMonthCapability is what a client needs to total one calendar month: to read a summary's
// month_of. A definition with one requires it, so a client that predates it (and would total every
// month) says it must be updated.
const SummaryMonthCapability = "summary.month_of"

// LabelsCapability is what a client needs to draw a screen that binds to a choice's label
// (/_labels/<field>): to hold it with each item and record, which a client that predates it
// doesn't, and would draw nothing there. A definition whose screens bind one requires it.
const LabelsCapability = "data.labels"

// AttachmentCapability is what a client needs to hold an attachment field: to read its value as a
// list of attachment ids and send them. A collection with one requires it (Definition.Check).
const AttachmentCapability = "field.attachment"

// ComponentCapability is the capability of drawing component name.
func ComponentCapability(name string) string { return "component." + name }

// FunctionCapability is the capability of calling function name.
func FunctionCapability(name string) string { return "function." + name }

// Bounds of a screen.
const (
	MaxComponents = 300
	maxIDLen      = 64
	maxTextLen    = 4000 // a literal string anywhere in a screen
	maxPathLen    = 200
	maxListLen    = 200 // children, tabs, options, checks, and/or values
	maxDepth      = 16  // functions nested in functions
)

// screenCheck validates one screen.
type screenCheck struct {
	*screenScope
	used      map[string]bool // the capabilities the screen needs
	ids       map[string]bool
	pathIcons map[string]bool     // the Icons given as a path
	refs      map[string][]string // component ID → the IDs it refers to
}

// checkComponents validates a screen's components and returns them re-encoded, so what is
// stored is exactly what was checked (no duplicate keys, nothing the decoder skipped). root is
// the id of its root component, "root" for an app with one screen. taken holds the ids of the
// app's other screens' components, which this one's may not reuse, and gets this one's.
func checkComponents(raw []json.RawMessage, root string, taken map[string]bool, scope *screenScope) ([]json.RawMessage, []string, error) {
	if len(raw) == 0 {
		return nil, nil, invalidf("components: a screen needs at least its root component")
	}
	if len(raw) > MaxComponents {
		return nil, nil, invalidf("components: at most %d", MaxComponents)
	}
	c := &screenCheck{screenScope: scope, used: map[string]bool{}, ids: map[string]bool{}, refs: map[string][]string{},
		pathIcons: map[string]bool{}}
	decoded := make([]map[string]any, len(raw))
	for i, r := range raw {
		obj, err := decodeObject(r)
		if err != nil {
			return nil, nil, invalidf("components[%d]: %v", i, err)
		}
		id, _ := obj["id"].(string)
		if id == "" || len(id) > maxIDLen {
			return nil, nil, invalidf("components[%d]: id must be text of 1 to %d characters", i, maxIDLen)
		}
		if root != hostID && id == hostID {
			return nil, nil, invalidf("components[%d]: the id %q is the server's, for the host of the screens: give a screen's root another id", i, hostID)
		}
		if c.ids[id] || (root != hostID && taken[id]) {
			return nil, nil, invalidf("components[%d]: id %q is used twice", i, id)
		}
		c.ids[id] = true
		decoded[i] = obj
	}
	for id := range c.ids {
		taken[id] = true
	}
	if !c.ids[root] {
		return nil, nil, invalidf("components: no component has the id %q", root)
	}
	out := make([]json.RawMessage, len(decoded))
	byID := make(map[string]map[string]any, len(decoded))
	for i, obj := range decoded {
		id := obj["id"].(string)
		byID[id] = obj
		if err := c.component(id, obj); err != nil {
			// An icon's refusal keeps its code, with the component it is in, so the web app can say
			// what is wrong with it.
			var coded *invalidError
			if errors.As(err, &coded) && coded.code == apierr.CodeAppIconPath {
				params := apierr.Params{"component": id}
				for k, v := range coded.params {
					params[k] = v
				}
				return nil, nil, invalidAs(coded.code, params, "component %q: %v", id, err)
			}
			return nil, nil, invalidf("component %q: %v", id, err)
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, nil, err
		}
		out[i] = b
	}
	if err := c.references(); err != nil {
		return nil, nil, err
	}
	if err := c.iconLabels(byID); err != nil {
		return nil, nil, err
	}
	return out, sortedKeys(c.used), nil
}

// decodeObject decodes one JSON object, keeping numbers as written.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	if dec.More() {
		return nil, fmt.Errorf("must be one JSON object")
	}
	return obj, nil
}

func (c *screenCheck) component(id string, obj map[string]any) error {
	name, _ := obj["component"].(string)
	spec, ok := components[name]
	c.used[ComponentCapability(name)] = true
	if !ok || name == screensComponent {
		return fmt.Errorf("%q is not a component of the catalog (it has %s)", obj["component"], strings.Join(authorable(), ", "))
	}
	for key, v := range obj {
		if key == "id" || key == "component" {
			continue
		}
		p, ok := spec[key]
		if !ok {
			return fmt.Errorf("%s has no prop %q", name, key)
		}
		if err := c.value(id, p, v, 0); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	for key, p := range spec {
		if _, given := obj[key]; p.required && !given {
			return fmt.Errorf("%s needs %q", name, key)
		}
	}
	if want, isInput := inputFields[name]; isInput {
		field, _ := obj["field"].(string)
		if f, ok := c.collection.field(field); !ok || f.Type != want {
			return fmt.Errorf("%s's field %q is not a %s field of the collection", name, field, want)
		}
	}
	return nil
}

// value checks v against p; id is the component it is in, for its references.
func (c *screenCheck) value(id string, p prop, v any, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("nested more than %d deep", maxDepth)
	}
	switch p.kind {
	case kString:
		return text(v)
	case kNumber:
		if _, ok := v.(json.Number); !ok {
			return fmt.Errorf("must be a number")
		}
	case kInteger:
		n, ok := v.(json.Number)
		if i, err := n.Int64(); !ok || err != nil || i < 0 {
			return fmt.Errorf("must be a whole number, 0 or more")
		}
	case kBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("must be true or false")
		}
	case kEnum:
		if s, ok := v.(string); !ok || !slices.Contains(p.values, s) {
			return fmt.Errorf("must be one of %s", strings.Join(p.values, ", "))
		}
	case kDynString, kDynNumber, kDynBool, kDynStringList, kDynValue:
		return c.dynamic(id, p.kind, v, depth)
	case kDynBoolList:
		list, ok := v.([]any)
		if !ok || len(list) < 2 || len(list) > maxListLen {
			return fmt.Errorf("must be a list of 2 to %d conditions", maxListLen)
		}
		for i, item := range list {
			if err := c.dynamic(id, kDynBool, item, depth+1); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	case kComponentID:
		s, ok := v.(string)
		if !ok || s == "" {
			return fmt.Errorf("must be a component's id")
		}
		c.refs[id] = append(c.refs[id], s)
	case kChildList:
		return c.childList(id, v)
	case kAction:
		return c.action(id, v, depth)
	case kChecks:
		return eachObject(v, func(i int, o map[string]any) error {
			return c.object(id, o, map[string]prop{"condition": req(kDynBool), "message": req(kString)}, depth+1)
		})
	case kAccessibility:
		o, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("must be an object")
		}
		return c.object(id, o, map[string]prop{"label": opt(kDynString), "description": opt(kDynString)}, depth+1)
	case kTabs:
		if l, ok := v.([]any); ok && len(l) == 0 {
			return fmt.Errorf("needs at least one tab")
		}
		return eachObject(v, func(i int, o map[string]any) error {
			return c.object(id, o, map[string]prop{"title": req(kDynString), "child": req(kComponentID)}, depth+1)
		})
	case kOptions:
		return eachObject(v, func(i int, o map[string]any) error {
			return c.object(id, o, map[string]prop{"label": req(kDynString), "value": req(kString)}, depth+1)
		})
	case kRule:
		rule, err := ruleFromValue(v)
		if err != nil {
			return err
		}
		return rule.check(c.collection)
	case kScreenRefs:
		return fmt.Errorf("only the server adds the host of the screens")
	case kIconName:
		switch n := v.(type) {
		case string:
			if !slices.Contains(iconNames, n) {
				return fmt.Errorf(`%q is not an icon's name: give the icon {"svgPath": "<path data>"} (icons.go)`, n)
			}
		case map[string]any:
			if _, isPath := n["svgPath"]; isPath {
				return c.iconPath(id, n)
			}
			return binding(n)
		default:
			return fmt.Errorf(`must be {"svgPath": "<path data>"}, an icon's name or a data binding`)
		}
	default:
		return fmt.Errorf("unknown kind of prop")
	}
	return nil
}

// object checks an object whose keys are exactly spec's (the required ones at least).
func (c *screenCheck) object(id string, o map[string]any, spec map[string]prop, depth int) error {
	for key, v := range o {
		p, ok := spec[key]
		if !ok {
			return fmt.Errorf("unknown key %q (expected %s)", key, strings.Join(sortedKeys(spec), ", "))
		}
		if err := c.value(id, p, v, depth); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	for key, p := range spec {
		if _, given := o[key]; p.required && !given {
			return fmt.Errorf("needs %q", key)
		}
	}
	return nil
}

// dynamic checks a dynamic value of kind k: a literal of its type, a data binding, or a
// call of a function returning its type.
func (c *screenCheck) dynamic(id string, k kind, v any, depth int) error {
	want := map[kind]string{kDynString: "text", kDynNumber: "a number", kDynBool: "true or false",
		kDynStringList: "a list of texts", kDynValue: "a value"}[k]
	switch x := v.(type) {
	case string:
		if k == kDynString || k == kDynValue {
			return text(x)
		}
	case json.Number:
		if k == kDynNumber || k == kDynValue {
			return nil
		}
	case bool:
		if k == kDynBool || k == kDynValue {
			return nil
		}
	case []any:
		if k == kDynStringList || k == kDynValue {
			if len(x) > maxListLen {
				return fmt.Errorf("at most %d items", maxListLen)
			}
			for i, item := range x {
				if k == kDynStringList {
					if err := text(item); err != nil {
						return fmt.Errorf("[%d]: %w", i, err)
					}
				} else if err := c.dynamic(id, kDynValue, item, depth+1); err != nil {
					return fmt.Errorf("[%d]: %w", i, err)
				}
			}
			return nil
		}
	case map[string]any:
		if _, isCall := x["call"]; isCall {
			return c.call(id, x, k, depth+1)
		}
		return binding(x)
	}
	return fmt.Errorf("must be %s, a data binding {\"path\": …} or a function call", want)
}

// call checks a function call whose result is used as k.
func (c *screenCheck) call(id string, o map[string]any, k kind, depth int) error {
	name, _ := o["call"].(string)
	fn, ok := functions[name]
	c.used[FunctionCapability(name)] = true
	if !ok {
		return fmt.Errorf("%q is not a function of the catalog (it has %s)", o["call"], strings.Join(sortedKeys(functions), ", "))
	}
	if want := map[kind]string{kDynString: "string", kDynNumber: "number", kDynBool: "boolean", kDynStringList: "array"}[k]; want != "" && fn.returns != want {
		return fmt.Errorf("%s returns a %s, not a %s", name, fn.returns, want)
	}
	for key, v := range o {
		switch key {
		case "call":
		case "returnType":
			if v != fn.returns {
				return fmt.Errorf("%s: returnType must be %q", name, fn.returns)
			}
		case "args":
			args, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: args must be an object", name)
			}
			if err := c.object(id, args, fn.args, depth); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if _, hasMin := args["min"]; fn.minOrMax && !hasMin {
				if _, hasMax := args["max"]; !hasMax {
					return fmt.Errorf("%s needs min or max", name)
				}
			}
		default:
			return fmt.Errorf("%s: unknown key %q", name, key)
		}
	}
	if _, given := o["args"]; !given {
		return fmt.Errorf("%s needs args", name)
	}
	return nil
}

// childList is a list of component IDs, or {componentId, path}: one copy of a component
// per item of the data list at path.
func (c *screenCheck) childList(id string, v any) error {
	switch x := v.(type) {
	case []any:
		if len(x) > maxListLen {
			return fmt.Errorf("at most %d children", maxListLen)
		}
		for i, item := range x {
			s, ok := item.(string)
			if !ok || s == "" {
				return fmt.Errorf("[%d]: must be a component's id", i)
			}
			c.refs[id] = append(c.refs[id], s)
		}
		return nil
	case map[string]any:
		return c.object(id, x, map[string]prop{"componentId": req(kComponentID), "path": req(kString)}, 1)
	}
	return fmt.Errorf("must be a list of component ids or {\"componentId\", \"path\"}")
}

// action is what a button does: send an event the app declares, with a context of dynamic
// values, or call a function.
func (c *screenCheck) action(id string, v any, depth int) error {
	o, ok := v.(map[string]any)
	if !ok || len(o) != 1 {
		return fmt.Errorf(`must be {"event": {"name", "context"}} or {"functionCall": …}`)
	}
	if fc, ok := o["functionCall"]; ok {
		call, ok := fc.(map[string]any)
		if !ok {
			return fmt.Errorf("functionCall must be a function call")
		}
		return c.call(id, call, kDynValue, depth+1)
	}
	ev, ok := o["event"].(map[string]any)
	if !ok {
		return fmt.Errorf(`must be {"event": {"name", "context"}} or {"functionCall": …}`)
	}
	var named Action
	for key, val := range ev {
		switch key {
		case "name":
			name, _ := val.(string)
			a, declared := c.actions[name]
			if !declared {
				return fmt.Errorf("event %q is not one of the app's actions", val)
			}
			named = a
		case "context":
			ctx, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("event context must be an object")
			}
			for k, cv := range ctx {
				if err := c.dynamic(id, kDynValue, cv, depth+1); err != nil {
					return fmt.Errorf("event context %s: %w", k, err)
				}
				if err := c.submitCaptured(k, cv); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("event: unknown key %q", key)
		}
	}
	if _, given := ev["name"]; !given {
		return fmt.Errorf("event needs a name")
	}
	if named.Kind == ActionNavigate {
		// What navigating takes is known here: the record's id for a screen that shows one,
		// and nothing else.
		ctx, _ := ev["context"].(map[string]any)
		_, hasID := ctx["id"]
		switch target := c.screens[named.Screen]; {
		case target.Record && !hasID:
			return fmt.Errorf("event %q opens %q, which shows a record: its context needs the record's id", ev["name"], named.Screen)
		case !target.Record && hasID:
			return fmt.Errorf("event %q opens %q, which doesn't show a record: it takes no id", ev["name"], named.Screen)
		case len(ctx) > 1 || (len(ctx) == 1 && !hasID):
			return fmt.Errorf("event %q: navigating takes only the record's id", ev["name"])
		}
	}
	return nil
}

// submitCaptured checks an event context entry that passes a location field taken at submit
// (capture "submit"): the browser takes the position as the button is pressed and puts it in the
// context under the field's own name, so the entry must be named so.
func (c *screenCheck) submitCaptured(key string, v any) error {
	o, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	path, _ := o["path"].(string)
	for _, root := range []string{"/" + DraftKey + "/", "/" + RecordKey + "/"} {
		name, found := strings.CutPrefix(path, root)
		if !found {
			continue
		}
		if f, declared := c.collection.field(name); declared && f.Type == TypeLocation && f.Capture == CaptureSubmit && key != name {
			return fmt.Errorf("event context %s: field %q is taken when the form is submitted, so its entry in the context is named %q", key, name, name)
		}
	}
	return nil
}

// references checks that every component referred to exists, and that no component
// contains itself, however indirectly: the renderer would draw it forever.
func (c *screenCheck) references() error {
	for from, to := range c.refs {
		for _, id := range to {
			if !c.ids[id] {
				return invalidf("component %q: refers to %q, which is not a component of the screen", from, id)
			}
		}
	}
	const (
		unseen = iota
		visiting
		done
	)
	state := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return invalidf("component %q contains itself", id)
		case done:
			return nil
		}
		state[id] = visiting
		for _, next := range c.refs[id] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}
	for _, id := range sortedKeys(c.ids) {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// binding is a data binding: {"path": "/a/b"} or, inside a template, a path relative to
// the item.
func binding(o map[string]any) error {
	path, ok := o["path"].(string)
	if !ok || len(o) != 1 {
		return fmt.Errorf(`must be a data binding {"path": …}`)
	}
	if path == "" || len(path) > maxPathLen {
		return fmt.Errorf("a path is 1 to %d characters", maxPathLen)
	}
	return nil
}

// No regular expressions: A2UI's validationRegexp prop and regex function aren't in the
// catalog. Every viewer's browser would run an app's pattern, with a backtracking engine,
// against text other members can store, and no check of the pattern can tell all the ones
// that take exponential or high polynomial time (such as (a+)+ or .*.*.*.*) from the
// rest. Apps validate with required, length, numeric and email instead.

func text(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("must be text")
	}
	if utf8.RuneCountInString(s) > maxTextLen {
		return fmt.Errorf("at most %d characters", maxTextLen)
	}
	return nil
}

func eachObject(v any, fn func(int, map[string]any) error) error {
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("must be a list")
	}
	if len(list) > maxListLen {
		return fmt.Errorf("at most %d items", maxListLen)
	}
	for i, item := range list {
		o, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("[%d]: must be an object", i)
		}
		if err := fn(i, o); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func componentNames() []string { return sortedKeys(components) }

// authorable are the components an app's own screens may use: all but the host of the screens,
// which the server adds.
func authorable() []string {
	return slices.DeleteFunc(componentNames(), func(n string) bool { return n == screensComponent })
}

// iconNames are A2UI's basic catalog's icon names, still accepted so apps saved with one stay
// valid; the web app draws nothing for them (icons.go).
var iconNames = []string{"accountCircle", "add", "arrowBack", "arrowForward", "attachFile", "calendarToday",
	"call", "camera", "check", "close", "delete", "download", "edit", "event", "error", "fastForward", "favorite",
	"favoriteOff", "folder", "help", "home", "info", "locationOn", "lock", "lockOpen", "mail", "menu", "moreVert",
	"moreHoriz", "notificationsOff", "notifications", "pause", "payment", "person", "phone", "photo", "play",
	"print", "refresh", "rewind", "search", "send", "settings", "share", "shoppingCart", "skipNext",
	"skipPrevious", "star", "starHalf", "starOff", "stop", "upload", "visibility", "visibilityOff", "volumeDown",
	"volumeMute", "volumeOff", "volumeUp", "warning"}
