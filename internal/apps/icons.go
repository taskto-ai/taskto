package apps

import (
	"fmt"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

const MaxIconPathLen = 2000

const IconPathCapability = "icon.svg_path"

const iconPathChars = "MmLlHhVvCcSsQqTtAaZz0123456789+-.eE, "

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

const maxIconContentDepth = 4

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

type iconWalk struct {
	byID      map[string]map[string]any
	pathIcons map[string]bool
	memo      map[contentAt]contentName
	visits    int
}

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
			got.named = false
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
			break
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
