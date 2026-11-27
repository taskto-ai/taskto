package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ContributionsVersion = 1

const (
	SlotHomeWidgets = "slot.home_widgets.v1"
	SlotNavPin      = "slot.nav_pin.v1"
)

var slotCapabilities = []string{SlotHomeWidgets, SlotNavPin}

const (
	MaxWidgets          = 6
	MaxWidgetSummaries  = 4
	maxWidgetTitleLen   = 60
	maxWidgetLabelLen   = 40
	maxPinLabelLen      = 30
	MaxWidgetItems      = 25
	WidgetKindSummaries = "summaries"
	WidgetKindScreen    = "screen"
	WidgetKindShortcut  = "shortcut"
	WidgetSmall         = "small"
	WidgetMedium        = "medium"
	WidgetLarge         = "large"
)

type Contributions struct {
	Version int      `json:"version"`
	Widgets []Widget `json:"widgets,omitempty"`
	Pin     *Pin     `json:"pin,omitempty"`
}

type Widget struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Kind      string          `json:"kind"`
	Size      string          `json:"size,omitempty"`
	Summaries []WidgetSummary `json:"summaries,omitempty"`
	Screen    string          `json:"screen,omitempty"`
}

type WidgetSummary struct {
	Summary string `json:"summary"`
	Label   string `json:"label"`
}

type Pin struct {
	Label  string `json:"label"`
	Screen string `json:"screen,omitempty"`
}

func (c *Contributions) UnmarshalJSON(raw []byte) error {
	type plain Contributions
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("contributions: %w", err)
	}
	*c = Contributions(p)
	return nil
}

func (c *Contributions) widget(id string) (Widget, bool) {
	if c == nil {
		return Widget{}, false
	}
	for _, w := range c.Widgets {
		if w.ID == id {
			return w, true
		}
	}
	return Widget{}, false
}

func plainText(what, s string, max int) error {
	if n := utf8.RuneCountInString(s); n < 1 || n > max || s != strings.TrimSpace(s) ||
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == ' ' || r == ' ' }) >= 0 {
		return invalidf("contributions: %s is plain text of 1 to %d characters, on one line, with no space at either end", what, max)
	}
	return nil
}

func (d Definition) checkContributions(screens map[string]Screen) (*Contributions, []string, error) {
	c := d.Contributions
	if c == nil {
		return nil, nil, nil
	}
	if c.Version == 0 {
		c.Version = ContributionsVersion
	}
	if c.Version != ContributionsVersion {
		return nil, nil, invalidf("contributions: version %d isn't supported: this server reads version %d", c.Version, ContributionsVersion)
	}
	if len(c.Widgets) == 0 && c.Pin == nil {
		return nil, nil, invalidf("contributions: widgets or pin is required (leave contributions out to offer nothing)")
	}
	if len(c.Widgets) > MaxWidgets {
		return nil, nil, invalidf("contributions: at most %d widgets", MaxWidgets)
	}
	var needs []string
	out := Contributions{Version: c.Version}
	seen := map[string]bool{}
	for i, w := range c.Widgets {
		if !namePattern.MatchString(w.ID) {
			return nil, nil, invalidf("contributions: widgets[%d]: an id is a lowercase letter, then up to 39 lowercase letters, digits or _", i)
		}
		if seen[w.ID] {
			return nil, nil, invalidf("contributions: widget %q is declared twice", w.ID)
		}
		seen[w.ID] = true
		if err := plainText(fmt.Sprintf("widget %q's title", w.ID), w.Title, maxWidgetTitleLen); err != nil {
			return nil, nil, err
		}
		switch w.Size {
		case "":
			w.Size = WidgetSmall
		case WidgetSmall, WidgetMedium, WidgetLarge:
		default:
			return nil, nil, invalidf("contributions: widget %q: size must be small, medium or large", w.ID)
		}
		switch w.Kind {
		case WidgetKindSummaries:
			if w.Screen != "" {
				return nil, nil, invalidf("contributions: widget %q: a summaries widget has no screen", w.ID)
			}
			if len(w.Summaries) < 1 || len(w.Summaries) > MaxWidgetSummaries {
				return nil, nil, invalidf("contributions: widget %q: a summaries widget shows 1 to %d summaries", w.ID, MaxWidgetSummaries)
			}
			named := map[string]bool{}
			for _, s := range w.Summaries {
				if _, ok := d.Summaries[s.Summary]; !ok {
					return nil, nil, invalidf("contributions: widget %q: %q is not one of the app's summaries (%s)", w.ID, s.Summary, strings.Join(sortedKeys(d.Summaries), ", "))
				}
				if named[s.Summary] {
					return nil, nil, invalidf("contributions: widget %q: summary %q is shown twice", w.ID, s.Summary)
				}
				named[s.Summary] = true
				if err := plainText(fmt.Sprintf("widget %q's label", w.ID), s.Label, maxWidgetLabelLen); err != nil {
					return nil, nil, err
				}
			}
			needs = append(needs, ComponentCapability("Column"), ComponentCapability("Stat"))
		case WidgetKindScreen:
			if len(w.Summaries) > 0 {
				return nil, nil, invalidf("contributions: widget %q: a screen widget has no summaries", w.ID)
			}
			target, ok := screens[w.Screen]
			if !ok {
				return nil, nil, invalidf("contributions: widget %q: screen must name a screen of the app (%s)", w.ID, strings.Join(sortedKeys(screens), ", "))
			}
			if target.Record {
				return nil, nil, invalidf("contributions: widget %q: %q shows a record, so a widget can't open on it", w.ID, w.Screen)
			}
		case WidgetKindShortcut:
			if len(w.Summaries) > 0 || w.Screen != "" {
				return nil, nil, invalidf("contributions: widget %q: a shortcut has no summaries or screen", w.ID)
			}
		default:
			return nil, nil, invalidf("contributions: widget %q: kind must be summaries, screen or shortcut", w.ID)
		}
		out.Widgets = append(out.Widgets, w)
	}
	if len(out.Widgets) > 0 {
		needs = append(needs, SlotHomeWidgets)
	}
	if c.Pin != nil {
		p := *c.Pin
		if err := plainText("the pin's label", p.Label, maxPinLabelLen); err != nil {
			return nil, nil, err
		}
		if p.Screen != "" {
			target, ok := screens[p.Screen]
			if !ok || target.Record {
				return nil, nil, invalidf("contributions: the pin's screen must name a screen of the app that doesn't show a record (%s)", strings.Join(sortedKeys(screens), ", "))
			}
		}
		out.Pin = &p
		needs = append(needs, SlotNavPin)
	}
	return &out, needs, nil
}

func withNeeds(requires, more []string) []string {
	for _, n := range more {
		if !slices.Contains(requires, n) {
			requires = append(requires, n)
		}
	}
	slices.Sort(requires)
	return requires
}

func WidgetSurface(surfaceID string, def Definition, w Widget, rows []Row) []Message {
	switch w.Kind {
	case WidgetKindSummaries:
		comps := make([]json.RawMessage, 0, len(w.Summaries)+1)
		children := make([]string, len(w.Summaries))
		data := map[string]any{}
		for i, s := range w.Summaries {
			id := fmt.Sprintf("stat%d", i)
			children[i] = id
			stat, _ := json.Marshal(map[string]any{"id": id, "component": "Stat", "label": s.Label, "value": map[string]string{"path": "/" + s.Summary}})
			comps = append(comps, stat)
			data[s.Summary] = summarize(def.Summaries[s.Summary], def.Collection, rows)
		}
		root, _ := json.Marshal(map[string]any{"id": "root", "component": "Column", "children": children})
		comps = append([]json.RawMessage{root}, comps...)
		return []Message{
			{"version": ProtocolVersion, "createSurface": map[string]any{"surfaceId": surfaceID, "catalogId": CatalogID}},
			{"version": ProtocolVersion, "updateComponents": map[string]any{"surfaceId": surfaceID, "components": comps}},
			dataMessage(surfaceID, "/", data),
		}
	case WidgetKindScreen:
		msgs := Surface(surfaceID, def, rows)
		msgs[2] = dataModel(surfaceID, def, rows, w.Screen, MaxWidgetItems)
		return msgs
	}
	return []Message{}
}

func openedAt(msgs []Message, surfaceID string, def Definition, rows []Row, screen string) []Message {
	if screen == "" || !def.hasScreens() {
		return msgs
	}
	msgs[2] = dataModel(surfaceID, def, rows, screen, 0)
	return msgs
}

func BoundItems(msgs []Message, n int) []Message {
	for _, m := range msgs {
		upd, ok := m["updateDataModel"].(map[string]any)
		if !ok {
			continue
		}
		switch path, _ := upd["path"].(string); path {
		case "/" + ItemsKey:
			if items, ok := upd["value"].([]map[string]any); ok && len(items) > n {
				upd["value"] = items[len(items)-n:]
			}
		case "/":
			if value, ok := upd["value"].(map[string]any); ok {
				if items, ok := value[ItemsKey].([]map[string]any); ok && len(items) > n {
					value[ItemsKey] = items[len(items)-n:]
				}
			}
		}
	}
	return msgs
}
