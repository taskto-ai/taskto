package desk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const maxLayoutBytes = 32 * 1024

const maxRequestBytes = maxLayoutBytes + 1024

var layoutID = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

type tab struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type node struct {
	Split    string    `json:"split,omitempty"`
	Sizes    []float64 `json:"sizes,omitempty"`
	Children []*node   `json:"children,omitempty"`
	Group    string    `json:"group,omitempty"`
	Active   string    `json:"active,omitempty"`
	Tabs     []tab     `json:"tabs,omitempty"`
}

type layout struct {
	V     int    `json:"v"`
	Focus string `json:"focus"`
	Root  *node  `json:"root"`
}

type layoutInvalid struct {
	reason string
}

func (e *layoutInvalid) Error() string { return e.reason }

func invalid(reason string) error { return &layoutInvalid{reason: reason} }

func Check(workspaceID int64, raw []byte) ([]byte, error) {
	if len(raw) > maxLayoutBytes {
		return nil, invalid("too_large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var value layout
	if err := dec.Decode(&value); err != nil {
		return nil, invalid("invalid_json")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields["root"] == nil || fields["focus"] == nil {
		return nil, invalid("invalid_json")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("invalid_json")
	}
	if value.V != 1 {
		return nil, invalid("version")
	}
	if value.Root == nil {
		if value.Focus != "" {
			return nil, invalid("focus")
		}
	} else {
		state := layoutCheck{workspace: workspaceID, ids: map[string]bool{}, groups: map[string]bool{}}
		if err := state.checkNode(value.Root, 1); err != nil {
			return nil, err
		}
		if state.tabsCount > 60 {
			return nil, invalid("too_many_tabs")
		}
		if !state.groups[value.Focus] {
			return nil, invalid("focus")
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal desk layout: %w", err)
	}
	if len(encoded) > maxLayoutBytes {
		return nil, invalid("too_large")
	}
	return encoded, nil
}

type layoutCheck struct {
	workspace   int64
	ids         map[string]bool
	groups      map[string]bool
	groupsCount int
	tabsCount   int
}

func (c *layoutCheck) checkNode(n *node, depth int) error {
	if depth > 3 {
		return invalid("too_deep")
	}
	if n == nil {
		return invalid("invalid_node")
	}
	if n.Split != "" {
		if n.Group != "" || n.Active != "" || n.Tabs != nil || (n.Split != "row" && n.Split != "column") {
			return invalid("invalid_node")
		}
		if len(n.Children) < 2 || len(n.Children) > 4 || len(n.Sizes) != len(n.Children) {
			return invalid("invalid_split")
		}
		sum := 0.0
		for _, size := range n.Sizes {
			if size < 0.1 || size > 0.9 {
				return invalid("invalid_sizes")
			}
			sum += size
		}
		if sum < 0.99 || sum > 1.01 {
			return invalid("invalid_sizes")
		}
		for _, child := range n.Children {
			if err := c.checkNode(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if n.Group == "" || n.Split != "" || n.Sizes != nil || n.Children != nil || len(n.Tabs) == 0 {
		return invalid("invalid_group")
	}
	if !layoutID.MatchString(n.Group) || !c.addID(n.Group) {
		return invalid("invalid_id")
	}
	c.groups[n.Group] = true
	c.groupsCount++
	if c.groupsCount > 6 {
		return invalid("too_many_groups")
	}
	if len(n.Tabs) > 20 {
		return invalid("too_many_tabs")
	}
	active := false
	for _, item := range n.Tabs {
		if !layoutID.MatchString(item.ID) || !c.addID(item.ID) {
			return invalid("invalid_id")
		}
		c.tabsCount++
		if item.ID == n.Active {
			active = true
		}
		if len(item.Path) > 512 || !validRoute(c.workspace, item.Path) {
			return invalid("invalid_path")
		}
	}
	if c.tabsCount > 60 {
		return invalid("too_many_tabs")
	}
	if !active {
		return invalid("active")
	}
	return nil
}

func (c *layoutCheck) addID(id string) bool {
	if c.ids[id] {
		return false
	}
	c.ids[id] = true
	return true
}

func validRoute(workspaceID int64, path string) bool {
	prefix := "/w/" + strconv.FormatInt(workspaceID, 10) + "/"
	if len(path) > 512 || len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		return false
	}
	parts := path[len(prefix):]
	for _, route := range []string{"home", "conversations", "knowledge", "boards", "apps", "notifications", "settings"} {
		if parts == route {
			return true
		}
		if len(parts) > len(route) && parts[:len(route)] == route && parts[len(route)] == '/' {
			segment := parts[len(route)+1:]
			if route == "knowledge" && validDocumentSegment(segment) || route != "knowledge" && validSegment(segment) {
				return true
			}
		}
	}
	return false
}

func validSegment(segment string) bool {
	decoded, ok := decodeSegment(segment)
	return ok && !strings.Contains(decoded, "/") && decoded != "." && decoded != ".."
}

func validDocumentSegment(segment string) bool {
	decoded, ok := decodeSegment(segment)
	if !ok {
		return false
	}
	space, path, found := strings.Cut(decoded, ":")
	if !found || space == "" || strings.Trim(space, "0123456789") != "" {
		return !strings.Contains(decoded, "/") && decoded != "." && decoded != ".."
	}
	if path == "" {
		return true
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." {
			return false
		}
	}
	return true
}

func decodeSegment(segment string) (string, bool) {
	if segment == "" || strings.ContainsAny(segment, `/\?#`) {
		return "", false
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil || strings.Contains(decoded, `\`) {
		return "", false
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return decoded, true
}
