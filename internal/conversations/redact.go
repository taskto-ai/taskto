package conversations

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/agentrt"
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
			changed = keepHarnessNotes(v) || changed
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

const harnessNotes = "<harness>\n"

func keepHarnessNotes(result map[string]any) bool {
	content, ok := result["content"].([]any)
	if !ok {
		return false
	}
	isNote := func(item any) bool {
		part, ok := item.(map[string]any)
		text, _ := part["text"].(string)
		return ok && part["type"] == "text" && strings.HasPrefix(text, harnessNotes)
	}
	if !slices.ContainsFunc(content, isNote) {
		return false
	}
	kept := slices.DeleteFunc(slices.Clone(content), func(item any) bool {
		part, ok := item.(map[string]any)
		return ok && part["type"] == "text" && !isNote(item)
	})
	if len(kept) == len(content) {
		return false
	}
	result["content"] = kept
	return true
}

func withoutToolResults(msgs []agentrt.Message) []agentrt.Message {
	return slices.DeleteFunc(slices.Clone(msgs), func(m agentrt.Message) bool { return m.Role == "toolResult" })
}
