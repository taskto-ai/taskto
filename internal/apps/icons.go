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

// iconLabels checks that a path icon standing alone has a name: one that is the whole content
// of a Button, or the trigger of a Modal, needs an accessibility label of its own, which the web
// app gives the drawn icon and so the button it is all of (the renderer's Button puts its own
// label on an element outside the <button>, which names nothing). Beside other content an icon is
// decoration, hidden from assistive technology unless it has a label. Icons by name draw
// nothing, so the rule is for paths only.
func (c *screenCheck) iconLabels(byID map[string]map[string]any) error {
	for _, id := range sortedKeys(byID) {
		obj := byID[id]
		var icon string
		switch obj["component"] {
		case "Button":
			icon, _ = obj["child"].(string)
		case "Modal":
			icon, _ = obj["trigger"].(string)
		}
		if !c.pathIcons[icon] || hasLabel(byID[icon]) {
			continue
		}
		return invalidAs(apierr.CodeAppIconLabel, apierr.Params{"component": icon},
			"component %q: an icon that is all of %s %q needs a name: give the icon accessibility.label", icon, obj["component"], id)
	}
	return nil
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
