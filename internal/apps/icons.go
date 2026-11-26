package apps

import (
	"fmt"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

// An app's icon is SVG path data the agent (or whoever edits the app) writes into the
// definition: A2UI's own form for the Icon component, {"name": {"svgPath": "…"}}, drawn on a
// fixed 24 by 24 view box and filled in the text's colour. It is stored and drawn in other
// people's browsers, shared spaces too, so the server accepts nothing but path data: the path
// commands, numbers, signs, decimal points, exponents, commas and spaces, starting with a
// moveto. No quote, bracket, colon, slash or letter outside the commands can get through, so a
// path can't close the attribute it is put in, open markup, or name a URL, a style or a script.
// The web app checks the same grammar again before it draws one (web/src/apps/iconPath.ts),
// creates the <svg> and <path> itself, and puts the string only in the path's d attribute.
//
// An icon given by a name, the A2UI basic catalog's other form ("delete", "menu"), is still
// accepted, so apps saved with one stay valid, but the web app has no drawing for it: no icon
// font and no set of icons, by choice. Where a name is all a control (a Button, a Modal's trigger)
// shows, the web app shows the name as text, as before, so the control keeps its size and its
// name; beside other content it draws nothing. A path is the way to give an icon.

// MaxIconPathLen is the longest path an icon may have, in characters. A detailed 24px glyph is
// well under 1,500; the definition's own limit (MaxDefinitionSize) already caps a whole app at
// a couple of dozen paths this long, so there is no separate count of icons.
const MaxIconPathLen = 2000

// IconPathCapability is what a client needs to draw an icon given as a path: a renderer that
// checks the path before drawing it. A definition with one requires it, so a client that
// predates the check says it must be updated instead of drawing the path unchecked.
const IconPathCapability = "icon.svg_path"

// iconPathChars are the characters path data may hold after its first moveto.
const iconPathChars = "MmLlHhVvCcSsQqTtAaZz0123456789+-.eE, "

// checkIconPath reports why s is not an icon's path data, or nil.
func checkIconPath(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("is empty")
	case len(s) > MaxIconPathLen:
		return fmt.Errorf("is over %d characters", MaxIconPathLen)
	case s[0] != 'M' && s[0] != 'm':
		return fmt.Errorf("must start with a moveto, M or m")
	}
	for i, r := range s {
		if !strings.ContainsRune(iconPathChars, r) {
			return fmt.Errorf("has %q at byte %d: only path commands (%s), numbers, commas and spaces are allowed",
				r, i, iconPathChars[:20])
		}
	}
	return nil
}

// iconPath checks an Icon's {"svgPath": …} and notes that the screen needs the capability. Every
// refusal has the code the web app translates.
func (c *screenCheck) iconPath(id string, o map[string]any) error {
	refuse := func(format string, args ...any) error {
		return invalidAs(apierr.CodeAppIconPath, apierr.Params{"max": MaxIconPathLen}, "svgPath "+format, args...)
	}
	if len(o) != 1 {
		return refuse(`takes no other key beside it`)
	}
	s, isText := o["svgPath"].(string)
	if !isText {
		return refuse("must be text, not %s", jsonKind(o["svgPath"]))
	}
	if err := checkIconPath(s); err != nil {
		return refuse("%v", err)
	}
	c.used[IconPathCapability] = true
	c.pathIcons[id] = true
	return nil
}

// jsonKind names the kind of a decoded JSON value, for a message.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "true or false"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	case string:
		return "text"
	}
	return "a number"
}

// iconLabels checks that a control drawn only with path icons has a name. A Button's content,
// or a Modal's trigger, followed through the containers that lay it out (Row, Column, Card), up to
// maxIconContentDepth deep: if nothing in it names the control (contentNames) and it holds path
// icons, one of them needs an accessibility label of its own, which the web app gives the drawn
// icon and so the control (the renderer's Button puts its own label on an element outside the
// <button>, which names nothing). Beside content that names the control, an icon is decoration,
// hidden from assistive technology unless it has a label. The web app runs the same walk
// (web/src/apps/iconPath.ts) to decide where an icon given by name shows its name.
func (c *screenCheck) iconLabels(byID map[string]map[string]any) error {
	w := iconWalk{byID: byID, pathIcons: c.pathIcons, memo: map[contentAt]contentName{}}
	for _, id := range sortedKeys(byID) {
		obj := byID[id]
		content, isControl := controlContent(obj)
		if !isControl {
			continue
		}
		if got := w.names(content, 0); !got.named && got.icon != "" {
			return invalidAs(apierr.CodeAppIconLabel, apierr.Params{"component": got.icon},
				"component %q: %s %q shows only icons, so one needs a name: give the icon accessibility.label", got.icon, obj["component"], id)
		}
	}
	return nil
}

// controlContent is the id of what a control shows: a Button's child, a Modal's trigger.
func controlContent(obj map[string]any) (string, bool) {
	switch obj["component"] {
	case "Button":
		s, _ := obj["child"].(string)
		return s, true
	case "Modal":
		s, _ := obj["trigger"].(string)
		return s, true
	}
	return "", false
}

// maxIconContentDepth is how deep iconLabels follows containers inside a control. Deeper content
// is taken to name it, so a rare deep layout is never refused for what wasn't looked at.
const maxIconContentDepth = 4

// contentAt is a component reached at a depth; contentName is what was found under it: whether
// something names the control, and the first unlabelled path icon met.
type (
	contentAt struct {
		id    string
		depth int
	}
	contentName struct {
		named bool
		icon  string
	}
)

// iconWalk walks a control's content. Each component is looked at once per depth (memo), so a
// component that many containers refer to costs no more than one: the work is bounded by the
// depth, the components and their children, however the references fan out. visits counts the
// steps, for a test that holds it to that bound.
type iconWalk struct {
	byID      map[string]map[string]any
	pathIcons map[string]bool
	memo      map[contentAt]contentName
	visits    int
}

// names reports what the content under id says of the control it is in, and the first unlabelled
// path icon it holds. What names it: a labelled path icon; an icon given by name, whose name the
// web app shows where nothing else names the control; a Text that isn't blank; any other component
// that isn't a container or a Divider; a template of a list, a bound Text and content too deep to
// follow (taken to name it). What doesn't: an unlabelled path icon, an icon bound to data, a blank
// literal Text, a Divider.
func (w *iconWalk) names(id string, depth int) contentName {
	w.visits++
	key := contentAt{id, depth}
	if got, seen := w.memo[key]; seen {
		return got
	}
	got := contentName{named: true}
	obj, ok := w.byID[id]
	switch kind, _ := obj["component"].(string); {
	case !ok || depth > maxIconContentDepth:
	case kind == "Icon":
		_, byName := obj["name"].(string)
		switch {
		case w.pathIcons[id]:
			if !hasLabel(obj) {
				got = contentName{icon: id}
			}
		case !byName:
			got.named = false // bound: drawn only if it holds a path, and then unnamed
		}
	case kind == "Text":
		if t, literal := obj["text"].(string); literal && strings.TrimSpace(t) == "" {
			got.named = false
		}
	case kind == "Divider":
		got.named = false
	case kind == "Card":
		child, _ := obj["child"].(string)
		got = w.names(child, depth+1)
	case kind == "Row" || kind == "Column":
		children, isList := obj["children"].([]any)
		if !isList {
			break // a template draws rows of data
		}
		got.named = false
		for _, ch := range children {
			s, _ := ch.(string)
			under := w.names(s, depth+1)
			got.named = got.named || under.named
			if got.icon == "" {
				got.icon = under.icon
			}
		}
	}
	w.memo[key] = got
	return got
}

// hasLabel is whether a component has a non-empty accessibility label: text, or a value bound
// or computed.
func hasLabel(obj map[string]any) bool {
	a, _ := obj["accessibility"].(map[string]any)
	switch l := a["label"].(type) {
	case string:
		return strings.TrimSpace(l) != ""
	case map[string]any:
		return true
	}
	return false
}
