package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func deskEntries(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for name, b := range files {
		if strings.HasPrefix(name, "desks/") {
			out[name] = b
		}
	}
	return out
}

func TestExportHoldsOnlyTheCallersOwnDesksOfThatWorkspace(t *testing.T) {
	s := exportStack(t, nil, nil)
	for _, c := range []struct {
		cookie string
		ws     int64
		route  string
	}{
		{s.alice.cookie, s.one, "knowledge/7%3Aalice-note.md"},
		{s.alice.cookie, s.two, "knowledge/7%3Aalice-in-two.md"},
		{s.bob.cookie, s.one, "knowledge/7%3Abob-note.md"},
	} {
		if code, body := s.saveDesk(c.cookie, c.ws, "main", 0, deskLayoutFor(c.ws, c.route)); code != http.StatusOK {
			t.Fatalf("save desk = %d %v", code, body)
		}
	}

	files := unzip(t, s.export(s.alice.cookie, s.one))
	entries := deskEntries(files)
	if len(entries) != 1 {
		t.Fatalf("alice's export has desks %v, want exactly her own", names(entries))
	}
	for name, b := range entries {
		var d struct {
			Name    string          `json:"name"`
			Layout  json.RawMessage `json:"layout"`
			Version int64           `json:"version"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(name, "desks/") || !strings.HasSuffix(name, "-main.json") || d.Version != 1 || !strings.Contains(string(d.Layout), "alice-note.md") {
			t.Fatalf("%s = %s", name, b)
		}
	}
	for name, b := range files {
		if strings.Contains(string(b), "bob-note") || strings.Contains(string(b), "alice-in-two") {
			t.Errorf("alice's export holds another person's or workspace's desk in %s", name)
		}
	}
	var mf struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil || mf.Counts["desks"] != 1 {
		t.Fatalf("manifest counts = %v, err %v", mf.Counts, err)
	}

	bobs := deskEntries(unzip(t, s.export(s.bob.cookie, s.one)))
	if len(bobs) != 1 {
		t.Fatalf("bob's export has desks %v", names(bobs))
	}
	for _, b := range bobs {
		if !strings.Contains(string(b), "bob-note.md") || strings.Contains(string(b), "alice") {
			t.Fatalf("bob's desk = %s", b)
		}
	}

	if none := deskEntries(unzip(t, s.export(s.carol.cookie, s.one))); len(none) != 0 {
		t.Fatalf("carol saved no desk and her export has %v", names(none))
	}
}
