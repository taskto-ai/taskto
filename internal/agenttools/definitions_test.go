package agenttools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDefinitions checks that tools.json defines every tool once, as an object schema, and
// nothing else: the sidecar and the MCP server list what it says.
func TestDefinitions(t *testing.T) {
	s := newWorld(t).tools
	seen := map[string]bool{}
	for _, d := range Definitions() {
		if seen[d.Name] {
			t.Errorf("%s is defined twice", d.Name)
		}
		seen[d.Name] = true
		if d.Description == "" {
			t.Errorf("%s has no description", d.Name)
		}
		var schema struct {
			Type                 string `json:"type"`
			AdditionalProperties *bool  `json:"additionalProperties"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil || schema.Type != "object" ||
			schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: the input schema must be an object without additional properties: %s", d.Name, d.InputSchema)
		}
	}
	for name := range s.tools {
		if !seen[name] {
			t.Errorf("%s is implemented but not defined in tools.json", name)
		}
	}
}

// TestSidecarDefinitions checks the sidecar's copy of tools.json is the same file.
func TestSidecarDefinitions(t *testing.T) {
	copied, err := os.ReadFile(filepath.Join("..", "..", "agent", "src", "taskto-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied, definitionsJSON) {
		t.Fatal("agent/src/taskto-tools.json differs from internal/agenttools/tools.json: run go generate ./internal/agenttools")
	}
}
