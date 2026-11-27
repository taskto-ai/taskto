package cliworker

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

var readOnlyTools = map[string][]string{
	"read":      {"file_path", "path"},
	"glob":      {"path", "pattern"},
	"grep":      {"path", "glob"},
	"ls":        {"path"},
	"todowrite": nil,
	"todoread":  nil,
}

var shellTools = []string{"bash", "shell", "exec_command", "local_shell"}

func readOnly(tool string, input json.RawMessage, dir string) bool {
	name := strings.ToLower(tool)
	var fields map[string]any
	if len(input) > 0 && json.Unmarshal(input, &fields) != nil {
		return false
	}
	if paths, ok := readOnlyTools[name]; ok {
		for _, key := range paths {
			if v, ok := fields[key]; ok {
				s, isString := v.(string)
				if !isString || !inside(s, dir) {
					return false
				}
			}
		}
		return true
	}
	if !slices.Contains(shellTools, name) {
		return false
	}
	if wd, ok := fields["workdir"]; ok {
		if s, isString := wd.(string); !isString || !inside(s, dir) {
			return false
		}
	}
	command, ok := fields["command"].(string)
	return ok && readOnlyCommand(command, dir)
}

func inside(path, dir string) bool {
	if path == "" {
		return true
	}
	if strings.HasPrefix(path, "~") {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

var readOnlyPrograms = map[string][]string{
	"cat": nil, "head": nil, "tail": nil, "wc": nil, "ls": nil, "pwd": nil, "stat": nil,
	"file": nil, "du": nil, "diff": nil, "cmp": nil, "basename": nil, "dirname": nil,
	"realpath": nil, "echo": nil, "uname": nil, "whoami": nil, "id": nil,
	"which": nil, "grep": nil, "egrep": nil, "fgrep": nil, "jq": nil,
	"date": {"-s", "--set"},
	"tree": {"-o", "--fromfile"},
	"rg":   {"--pre", "--pre-glob", "-z", "--search-zip", "--hostname-bin"},
	"find": {"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"},
}

func readOnlyCommand(command, dir string) bool {
	words, ok := splitWords(command)
	if !ok || len(words) == 0 {
		return false
	}
	denied, ok := readOnlyPrograms[words[0]]
	if !ok {
		return false
	}
	args := words[1:]
	for _, a := range args {
		flag, _, _ := strings.Cut(a, "=")
		if slices.Contains(denied, flag) || deniedInCluster(a, denied) {
			return false
		}
		if strings.HasPrefix(a, "-") {
			if _, v, found := strings.Cut(a, "="); found && !inside(v, dir) {
				return false
			}
			continue
		}
		if !inside(a, dir) {
			return false
		}
	}
	return true
}

func splitWords(command string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			cur.WriteString(command[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			end := strings.IndexByte(command[i+1:], '"')
			if end < 0 {
				return nil, false
			}
			quoted := command[i+1 : i+1+end]
			if strings.ContainsAny(quoted, "$`\\!") {
				return nil, false
			}
			cur.WriteString(quoted)
			i += end + 1
			inWord = true
		case strings.IndexByte(";&|<>$`(){}*?[]!#\\\n\r~", c) >= 0:
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

func deniedInCluster(a string, denied []string) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, d := range denied {
		if len(d) == 2 && d[0] == '-' && d[1] != '-' && strings.ContainsRune(a[1:], rune(d[1])) {
			return true
		}
	}
	return false
}

func (t *Tools) readOnly(cli CLI, tool string, input json.RawMessage) bool {
	if t == nil {
		return false
	}
	if cli != Grok {
		name, ok := strings.CutPrefix(tool, "mcp__"+toolsServer+"__")
		return ok && slices.Contains(t.ReadOnly, name)
	}
	name, ok := strings.CutPrefix(tool, toolsServer+"__")
	switch {
	case ok:
	case tool == "search_tool":
		return true
	case tool == "use_tool":
		var fields map[string]json.RawMessage
		if json.Unmarshal(input, &fields) != nil {
			return false
		}
		for k := range fields {
			if k != "tool_name" && k != "tool_input" {
				return false
			}
		}
		var target string
		var args map[string]json.RawMessage
		if json.Unmarshal(fields["tool_name"], &target) != nil || json.Unmarshal(fields["tool_input"], &args) != nil || args == nil {
			return false
		}
		if name, ok = strings.CutPrefix(target, toolsServer+"__"); !ok {
			return false
		}
	default:
		return false
	}
	return slices.Contains(t.ReadOnly, name)
}
