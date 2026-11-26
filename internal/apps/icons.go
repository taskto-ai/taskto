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
		var icons []string
		if c.namedBy(byID, content, 0, &icons) || len(icons) == 0 {
			continue
		}
		return invalidAs(apierr.CodeAppIconLabel, apierr.Params{"component": icons[0]},
			"component %q: %s %q shows only icons, so one needs a name: give the icon accessibility.label", icons[0], obj["component"], id)
	}
	return nil
}

// maxIconContentDepth is how deep iconLabels follows containers inside a control. Deeper content
// is taken to name it, so a rare deep layout is never refused for what wasn't looked at.
const maxIconContentDepth = 4

// namedBy reports whether the content under id names the control it is in: a labelled path icon, a
// Text or any other component that isn't a container, an icon by name or a template of a list.
// It adds the unlabelled path icons it passes to icons.
func (c *screenCheck) namedBy(byID map[string]map[string]any, id string, depth int, icons *[]string) bool {
	obj, ok := byID[id]
	if !ok || depth > maxIconContentDepth {
		return true
	}
	switch obj["component"] {
	case "Icon":
		if !c.pathIcons[id] {
			return false // a name draws nothing
		}
		if hasLabel(obj) {
			return true
		}
		*icons = append(*icons, id)
		return false
	case "Card":
		child, _ := obj["child"].(string)
		return c.namedBy(byID, child, depth+1, icons)
	case "Row", "Column":
		children, isList := obj["children"].([]any)
		if !isList {
			return true // a template draws rows of data
		}
		named := false
		for _, ch := range children {
			s, _ := ch.(string)
			if c.namedBy(byID, s, depth+1, icons) {
				named = true
			}
		}
		return named
	}
	return true
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
