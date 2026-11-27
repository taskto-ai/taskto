package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Screen struct {
	Name       string            `json:"name"`
	Root       string            `json:"root"`
	Record     bool              `json:"record,omitempty"`
	Components []json.RawMessage `json:"components" apitypes:"json"`
}

const MaxScreens = 12

const hostID = "root"

type screenScope struct {
	actions    map[string]Action
	collection Collection
	screens    map[string]Screen
	copied     bool
}

func (d Definition) hasScreens() bool { return len(d.Screens) > 0 }

func (d Definition) screen(name string) (Screen, bool) {
	for _, s := range d.Screens {
		if s.Name == name {
			return s, true
		}
	}
	return Screen{}, false
}

func (d Definition) screenIndex() (map[string]Screen, error) {
	index := map[string]Screen{}
	if len(d.Screens) == 0 {
		return index, nil
	}
	if len(d.Screens) > MaxScreens {
		return nil, invalidf("screens: at most %d", MaxScreens)
	}
	for _, s := range d.Screens {
		if !namePattern.MatchString(s.Name) {
			return nil, invalidf("screens: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", s.Name)
		}
		if _, dup := index[s.Name]; dup {
			return nil, invalidf("screens: %q is declared twice", s.Name)
		}
		index[s.Name] = s
	}
	entry, ok := index[d.Entry]
	if !ok {
		return nil, invalidf("entry: must name one of the screens (%s)", strings.Join(sortedKeys(index), ", "))
	}
	if entry.Record {
		return nil, invalidf("entry: %q shows a record, so it can't be the one the app opens on", d.Entry)
	}
	return index, nil
}

func checkActionScreens(name string, a Action, screens map[string]Screen) error {
	switch {
	case a.Kind == ActionNavigate:
		if _, ok := screens[a.Screen]; !ok {
			return invalidf("actions: %q: navigate names a screen the app has (%s)", name, strings.Join(sortedKeys(screens), ", "))
		}
		if a.Then != "" {
			return invalidf("actions: %q: only an insert, update or delete has then", name)
		}
	case a.Screen != "":
		return invalidf("actions: %q: only navigate has a screen", name)
	case a.Then != "":
		target, ok := screens[a.Then]
		if !ok {
			return invalidf("actions: %q: then names a screen the app has (%s)", name, strings.Join(sortedKeys(screens), ", "))
		}
		if target.Record {
			return invalidf("actions: %q: then names %q, which shows a record: only navigate opens it, with the record's id", name, a.Then)
		}
	}
	return nil
}

func checkScreens(screens []Screen, scope *screenScope) ([]Screen, []string, error) {
	total := 0
	for _, s := range screens {
		total += len(s.Components)
	}
	if total > MaxComponents {
		return nil, nil, invalidf("screens: at most %d components in all", MaxComponents)
	}
	taken := map[string]bool{hostID: true}
	used := map[string]bool{ComponentCapability(screensComponent): true}
	out := make([]Screen, len(screens))
	for i, s := range screens {
		if s.Root == hostID {
			return nil, nil, invalidf("screen %q: root names the screen's root component; %q is the id of the screens' host, so use another", s.Name, hostID)
		}
		comps, needs, err := checkComponents(s.Components, s.Root, taken, scope)
		if err != nil {
			var ie *invalidError
			if errors.As(err, &ie) {
				return nil, nil, &invalidError{msg: fmt.Sprintf("screen %q: %s", s.Name, ie.msg), code: ie.code, params: ie.params}
			}
			return nil, nil, err
		}
		for _, n := range needs {
			used[n] = true
		}
		s.Components = comps
		out[i] = s
	}
	return out, sortedKeys(used), nil
}

const screensComponent = "Screens"

func (d Definition) surfaceComponents() []json.RawMessage {
	if !d.hasScreens() {
		return d.withColumnTypes(d.Components)
	}
	refs := make([]map[string]string, len(d.Screens))
	for i, s := range d.Screens {
		refs[i] = map[string]string{"name": s.Name, "child": s.Root}
	}
	host, err := json.Marshal(map[string]any{
		"id": hostID, "component": screensComponent,
		"current": map[string]string{"path": "/" + ScreenKey},
		"screens": refs,
	})
	if err != nil {
		return d.Components
	}
	out := []json.RawMessage{host}
	for _, s := range d.Screens {
		out = append(out, d.withColumnTypes(s.Components)...)
	}
	return out
}

func (d Definition) withColumnTypes(components []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(components))
	for i, raw := range components {
		out[i] = d.withColumnTypesIn(raw)
	}
	return out
}

func (d Definition) withColumnTypesIn(raw json.RawMessage) json.RawMessage {
	var head struct {
		Component string `json:"component"`
	}
	if json.Unmarshal(raw, &head) != nil || head.Component != "Table" {
		return raw
	}
	obj, err := decodeObject(raw)
	if err != nil {
		return raw
	}
	columns, _ := obj["columns"].([]any)
	for _, c := range columns {
		column, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := column["field"].(string)
		if f, declared := d.Collection.field(name); declared {
			column["type"] = f.Type
		}
	}
	typed, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return typed
}

func Opened(surfaceID, screen string, record map[string]any) []Message {
	return []Message{dataMessage(surfaceID, "/"+ScreenKey, screen), dataMessage(surfaceID, "/"+RecordKey, record)}
}

func planNavigate(def Definition, a Action, ch Change, action ClientAction) (Change, error) {
	target, ok := def.screen(a.Screen)
	if !ok {
		return Change{}, invalidf("the screen %q is not one of the app's", a.Screen)
	}
	action.ExpectedRevision = nil
	for name := range action.Context {
		if name != "id" {
			return Change{}, invalidf("navigate takes only the record's id, not %s", name)
		}
	}
	ch.Screen = target.Name
	_, given := action.Context["id"]
	switch {
	case target.Record:
		id, ok := rowID(action.Context["id"])
		if !ok {
			return Change{}, invalidf("opening %q needs the record's id", target.Name)
		}
		ch.ID = id
	case given:
		return Change{}, invalidf("%q doesn't show a record: navigate it without an id", target.Name)
	}
	return ch, nil
}

func setOf(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}
