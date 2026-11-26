package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Screens.
//
// An app has one screen (Definition.Components), or several (Definition.Screens): named, each a
// component tree with its own root, one of them the entry the app opens on. The surface the
// browser draws is one for the whole app: the server adds a host component with the id "root"
// (the catalog's Screens), which draws the screen named by /screen in the data model, so a
// screen is shown by the data model saying so. Every component's id is unique across the
// screens, and a component refers only to those of its own screen.
//
// Moving between screens is the navigate action: an action {"kind": "navigate", "screen": …}
// that a Button's event names. The server answers it with the new /screen, and, for a screen
// that shows one record (Screen.Record), the record at /record, read through the one filter
// of what the caller may read; it changes no row, so anyone who may open the app may navigate
// it. Nothing else in the data model is touched, so the form at /draft keeps what was typed
// across screens. An insert, update or delete may say Then, the screen it opens once it has
// run.

// Screen is one screen of an app.
type Screen struct {
	Name string `json:"name"`
	// Root is the id of the screen's root component, one of its Components.
	Root string `json:"root"`
	// Record is a screen that shows one record, at /record, and that is opened with the
	// record's id (the navigate action's context, {"id": …}).
	Record     bool              `json:"record,omitempty"`
	Components []json.RawMessage `json:"components" apitypes:"json"`
}

// Bounds of an app's screens. Its components in all are bounded by MaxComponents, as one
// screen's always were.
const MaxScreens = 12

// hostID is the id of the component the server adds to draw the current screen.
const hostID = "root"

// screenScope is what the components of a screen are checked against.
type screenScope struct {
	actions    map[string]Action
	collection Collection
	screens    map[string]Screen
	copied     bool // a copy of another app: the icon label rule is not applied (icons.go)
}

func (d Definition) hasScreens() bool { return len(d.Screens) > 0 }

// screen returns the screen named name.
func (d Definition) screen(name string) (Screen, bool) {
	for _, s := range d.Screens {
		if s.Name == name {
			return s, true
		}
	}
	return Screen{}, false
}

// screenIndex checks the screens' names and entry and indexes them by name; there are none to
// index in an app with one screen.
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

// checkActionScreens checks what an action says about screens: navigate names one, and then
// (after an insert, update or delete) names one that doesn't show a record.
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

// checkScreens validates every screen's components against the catalog and returns the screens
// re-encoded, with what the app needs of a client (the host's component among it).
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

// screensComponent is the catalog's host of the screens, which only the server adds.
const screensComponent = "Screens"

// surfaceComponents are the components the surface is drawn from: an app with one screen
// sends its components as they are; one with screens has the host in front of every screen's.
func (d Definition) surfaceComponents() []json.RawMessage {
	if !d.hasScreens() {
		return d.Components
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
		return d.Components // strings and maps of strings always encode
	}
	out := []json.RawMessage{host}
	for _, s := range d.Screens {
		out = append(out, s.Components...)
	}
	return out
}

// Opened is the answer to a navigate action: the screen to show, and the record it shows (an
// empty one for a screen that shows none). It touches nothing else in the data model.
func Opened(surfaceID, screen string, record map[string]any) []Message {
	return []Message{dataMessage(surfaceID, "/"+ScreenKey, screen), dataMessage(surfaceID, "/"+RecordKey, record)}
}

// planNavigate works out a navigate action: the screen, and for one that shows a record, which.
func planNavigate(def Definition, a Action, ch Change, action ClientAction) (Change, error) {
	target, ok := def.screen(a.Screen)
	if !ok {
		return Change{}, invalidf("the screen %q is not one of the app's", a.Screen)
	}
	// A navigate reads, so a revision it carries is ignored: the web app names the revision of the
	// record a button is on for every action, as it can't tell what an action does.
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
