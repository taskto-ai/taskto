package conversations

import (
	"bytes"
	"encoding/json"
	"slices"
)

const cliTool = "cli_turn"

func redactForViewer(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	dropped := false
	if event, ok := doc.(map[string]any); ok && event["type"] == "ops" {
		if ops, ok := event["ops"].([]any); ok {
			kept := slices.DeleteFunc(slices.Clone(ops), outputOp)
			dropped = len(kept) != len(ops)
			event["ops"] = kept
		}
	}
	if !scrub(doc) && !dropped {
		return raw, nil
	}
	return json.Marshal(doc)
}

func outputOp(op any) bool {
	parts, ok := op.([]any)
	if !ok || len(parts) < 2 {
		return false
	}
	path, ok := parts[1].([]any)
	if !ok {
		return false
	}
	underTools := false
	for _, step := range path {
		switch step {
		case "transcript":
			return true
		case "tools":
			underTools = true
		case "output":
			if underTools {
				return true
			}
		}
	}
	return false
}

func scrub(v any) bool {
	changed := false
	switch v := v.(type) {
	case map[string]any:
		if v["role"] == "toolResult" && v["toolName"] == cliTool {
			if details, ok := v["details"].(map[string]any); ok {
				if _, kept := details["transcript"]; kept {
					delete(details, "transcript")
					changed = true
				}
			}
		}
		if _, slot := v["callId"]; slot && v["name"] == cliTool {
			if _, kept := v["output"]; kept {
				delete(v, "output")
				changed = true
			}
		}
		for _, child := range v {
			changed = scrub(child) || changed
		}
	case []any:
		for _, child := range v {
			changed = scrub(child) || changed
		}
	}
	return changed
}
