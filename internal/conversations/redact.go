package conversations

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/agentrt"
)

const cliTool = "cli_turn"

type viewerFilter struct {
	cli     []bool
	unknown bool
}

func redactForViewer(raw []byte) ([]byte, error) {
	return new(viewerFilter).redact(raw)
}

func (f *viewerFilter) redact(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	dropped := false
	if event, ok := doc.(map[string]any); ok {
		switch event["type"] {
		case "value":
			f.load(event["value"])
		case "ops":
			if ops, ok := event["ops"].([]any); ok {
				kept := make([]any, 0, len(ops))
				for _, op := range ops {
					if f.outputOp(op) {
						continue
					}
					f.apply(op)
					kept = append(kept, op)
				}
				dropped = len(kept) != len(ops)
				event["ops"] = kept
			}
		}
	}
	if !scrub(doc, nil) && !dropped {
		return raw, nil
	}
	return json.Marshal(doc)
}

func (f *viewerFilter) outputOp(op any) bool {
	parts, ok := op.([]any)
	if !ok || len(parts) < 2 {
		return false
	}
	path, ok := parts[1].([]any)
	if !ok {
		return false
	}
	if slot, ok := liveSlotOutput(path); ok {
		return f.unknown || slot >= len(f.cli) || f.cli[slot]
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

var liveToolsPath = []any{"docs", "pi.live", "tools"}

func liveSlotOutput(path []any) (int, bool) {
	if len(path) != 5 || path[4] != "output" || !slices.Equal(path[:3], liveToolsPath) {
		return 0, false
	}
	return slotIndex(path[3])
}

func slotIndex(step any) (int, bool) {
	n, ok := step.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return int(i), err == nil && i >= 0
}

func (f *viewerFilter) load(root any) {
	f.unknown = false
	f.cli = nil
	if doc, ok := root.(map[string]any); ok {
		f.loadDocs(doc["docs"])
	}
}

func (f *viewerFilter) loadDocs(docs any) {
	m, _ := docs.(map[string]any)
	f.loadLive(m["pi.live"])
}

func (f *viewerFilter) loadLive(live any) {
	m, _ := live.(map[string]any)
	f.loadTools(m["tools"])
}

func (f *viewerFilter) loadTools(tools any) {
	f.unknown = false
	f.cli = nil
	list, _ := tools.([]any)
	for _, slot := range list {
		f.cli = append(f.cli, slotIsCLI(slot))
	}
}

func slotIsCLI(slot any) bool {
	m, ok := slot.(map[string]any)
	name, _ := m["name"].(string)
	return !ok || name == "" || name == cliTool
}

func (f *viewerFilter) apply(op any) {
	parts, ok := op.([]any)
	if !ok || len(parts) < 1 {
		return
	}
	if parts[0] == "r" && len(parts) == 2 {
		f.load(parts[1])
		return
	}
	if len(parts) < 2 {
		return
	}
	path, ok := parts[1].([]any)
	if !ok || !touchesSlots(path) {
		return
	}
	switch {
	case parts[0] == "s" && len(parts) == 3:
		f.set(path, parts[2])
	case parts[0] == "p" && len(parts) == 5 && slices.Equal(path, liveToolsPath):
		f.splice(parts)
	default:
		f.unknown = true
	}
}

func touchesSlots(path []any) bool {
	n := min(len(path), len(liveToolsPath))
	if !slices.Equal(path[:n], liveToolsPath[:n]) {
		return false
	}
	return len(path) <= len(liveToolsPath)+1 || len(path) == len(liveToolsPath)+2 && path[len(liveToolsPath)+1] == "name"
}

func (f *viewerFilter) set(path []any, value any) {
	switch {
	case len(path) == 1:
		f.loadDocs(value)
	case len(path) == 2:
		f.loadLive(value)
	case len(path) == 3:
		f.loadTools(value)
	case len(path) == 4:
		f.setSlot(path[3], slotIsCLI(value))
	case path[4] == "name":
		f.setSlot(path[3], slotIsCLI(map[string]any{"name": value}))
	}
}

func (f *viewerFilter) setSlot(step any, cli bool) {
	i, ok := slotIndex(step)
	if !ok || i > len(f.cli) {
		f.unknown = true
		return
	}
	if i == len(f.cli) {
		f.cli = append(f.cli, cli)
		return
	}
	f.cli[i] = cli
}

func (f *viewerFilter) splice(parts []any) {
	at, ok := slotIndex(parts[2])
	count, countOK := slotIndex(parts[3])
	items, itemsOK := parts[4].([]any)
	if !ok || !countOK || !itemsOK || at+count > len(f.cli) {
		f.unknown = true
		return
	}
	added := make([]bool, len(items))
	for i, item := range items {
		added[i] = slotIsCLI(item)
	}
	f.cli = slices.Replace(slices.Clone(f.cli), at, at+count, added...)
}

func scrub(v any, diagnostics []any) bool {
	changed := false
	switch v := v.(type) {
	case map[string]any:
		if v["role"] == "toolResult" && v["toolName"] == cliTool {
			changed = keepHarnessDiagnostics(v, diagnostics) || changed
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
		for key, child := range v {
			var own []any
			if key == "model" {
				own = entryDiagnostics(v)
			}
			changed = scrub(child, own) || changed
		}
	case []any:
		for _, child := range v {
			changed = scrub(child, diagnostics) || changed
		}
	}
	return changed
}

func entryDiagnostics(entry map[string]any) []any {
	data, _ := entry["data"].(map[string]any)
	list, _ := data["diagnostics"].([]any)
	return list
}

func renderDiagnostics(diagnostics []any) (string, bool) {
	lines := make([]string, 0, len(diagnostics))
	for _, item := range diagnostics {
		d, ok := item.(map[string]any)
		severity, _ := d["severity"].(string)
		message, _ := d["message"].(string)
		if !ok || severity == "" || message == "" {
			return "", false
		}
		lines = append(lines, "["+severity+"] "+message)
	}
	return "<harness>\n" + strings.Join(lines, "\n") + "\n</harness>", len(lines) > 0
}

func keepHarnessDiagnostics(result map[string]any, diagnostics []any) bool {
	if failed, known := result["isError"].(bool); known && !failed {
		return false
	}
	content, ok := result["content"].([]any)
	if !ok {
		return false
	}
	var kept []any
	if note, ok := renderDiagnostics(diagnostics); ok && len(content) > 0 {
		last, _ := content[len(content)-1].(map[string]any)
		if text, _ := last["text"].(string); last["type"] == "text" && text == note {
			kept = content[len(content)-1:]
		}
	}
	if kept == nil {
		kept = []any{}
	}
	if len(kept) == len(content) {
		return false
	}
	result["content"] = kept
	return true
}

func withoutToolResults(msgs []agentrt.Message) []agentrt.Message {
	return slices.DeleteFunc(slices.Clone(msgs), func(m agentrt.Message) bool { return m.Role == "toolResult" })
}
