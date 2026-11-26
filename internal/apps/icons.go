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
// An icon given by a name, the A2UI basic catalog's other form, is still accepted, so apps
// saved with one stay valid, but nothing is drawn for it: the web app loads no icon font.

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
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(iconPathChars, rune(s[i])) {
			return fmt.Errorf("has %q at %d: only path commands (%s), numbers, commas and spaces are allowed",
				s[i], i, iconPathChars[:20])
		}
	}
	return nil
}

// iconPath checks an Icon's {"svgPath": …} and notes that the screen needs the capability.
func (c *screenCheck) iconPath(id string, o map[string]any) error {
	if len(o) != 1 {
		return fmt.Errorf(`{"svgPath"} takes no other key`)
	}
	s, _ := o["svgPath"].(string)
	if err := checkIconPath(s); err != nil {
		return invalidAs(apierr.CodeAppIconPath, apierr.Params{"max": MaxIconPathLen}, "svgPath %v", err)
	}
	c.used[IconPathCapability] = true
	c.pathIcons[id] = true
	return nil
}

// iconLabels checks that a control drawn only with path icons has a name. A Button's content,
// or a Modal's trigger, followed through the containers that lay it out (Row, Column, Card), up to
// maxIconContentDepth deep: if it holds path icons and nothing else that names it (a Text, or any
// other component), one of its icons needs an accessibility label of its own, which the web app
// gives the drawn icon and so the control (the renderer's Button puts its own label on an element
// outside the <button>, which names nothing). Beside other content an icon is decoration, hidden
// from assistive technology unless it has a label. Icons by name draw nothing, so they neither
// name a control nor need a name.
func (c *screenCheck) iconLabels(byID map[string]map[string]any) error {
	memo := map[contentAt]contentName{}
	for _, id := range sortedKeys(byID) {
		obj := byID[id]
		var content string
		switch obj["component"] {
		case "Button":
			content, _ = obj["child"].(string)
		case "Modal":
			content, _ = obj["trigger"].(string)
		default:
			continue
		}
		if got := namedBy(byID, c.pathIcons, memo, content, 0); !got.named && got.icon != "" {
			return invalidAs(apierr.CodeAppIconLabel, apierr.Params{"component": got.icon},
				"component %q: %s %q shows only icons, so one needs a name: give the icon accessibility.label", got.icon, obj["component"], id)
		}
	}
	return nil
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

// namedBy reports what the content under id says of the control it is in: named by a labelled
// path icon, a Text or any other component that isn't a container, or a template of a list; and
// the first unlabelled path icon it holds. Each component is looked at once per depth (memo), so a
// component that many containers refer to costs no more than one: the work is bounded by the
// depth, the components and their children, however the references fan out.
func namedBy(byID map[string]map[string]any, pathIcons map[string]bool, memo map[contentAt]contentName, id string, depth int) contentName {
	key := contentAt{id, depth}
	if got, seen := memo[key]; seen {
		return got
	}
	got := contentName{named: true}
	obj, ok := byID[id]
	switch {
	case !ok || depth > maxIconContentDepth:
	case obj["component"] == "Icon":
		switch {
		case !pathIcons[id]:
			got.named = false // a name draws nothing
		case !hasLabel(obj):
			got = contentName{icon: id}
		}
	case obj["component"] == "Card":
		child, _ := obj["child"].(string)
		got = namedBy(byID, pathIcons, memo, child, depth+1)
	case obj["component"] == "Row" || obj["component"] == "Column":
		children, isList := obj["children"].([]any)
		if !isList {
			break // a template draws rows of data
		}
		got.named = false
		for _, ch := range children {
			s, _ := ch.(string)
			under := namedBy(byID, pathIcons, memo, s, depth+1)
			got.named = got.named || under.named
			if got.icon == "" {
				got.icon = under.icon
			}
		}
	}
	memo[key] = got
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
