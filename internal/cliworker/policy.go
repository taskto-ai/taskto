package cliworker

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// The read-only policy (docs/agent-safety.md): for Codex and Grok Build, whose hook sees every
// tool call (PreToolUse), the calls that only read inside the turn's working directory go
// through without asking. Everything else asks. It is deliberately small: a call it doesn't
// understand asks, which costs a question, never a mistake.
//
// It is lexical: it doesn't follow symlinks (the worker never opens a path in a user's home).
// A symlink that leads out of the working directory had to be made by a command the user
// allowed, or brought in by one (a clone).

// readOnlyTools are tools that only read, with the input fields that name what they read.
var readOnlyTools = map[string][]string{
	"read":      {"file_path", "path"},
	"glob":      {"path", "pattern"},
	"grep":      {"path", "glob"},
	"ls":        {"path"},
	"todowrite": nil,
	"todoread":  nil,
}

// shellTools are the tools that run a shell command, in an input field "command".
var shellTools = []string{"bash", "shell", "exec_command", "local_shell"}

// readOnly reports whether tool, called with input, only reads inside dir.
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
	// A command run elsewhere than the working directory asks.
	if wd, ok := fields["workdir"]; ok {
		if s, isString := wd.(string); !isString || !inside(s, dir) {
			return false
		}
	}
	command, ok := fields["command"].(string)
	return ok && readOnlyCommand(command, dir)
}

// inside reports whether path (relative to dir, or absolute) stays in dir, lexically.
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

// readOnlyPrograms are the programs a read-only command may run, each with the arguments
// that would make it run or write something. git is not among them: even `git status` runs
// what the repository's own .git/config says (core.fsmonitor, a diff driver), and a
// repository can come with one, so every git command asks.
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

// readOnlyCommand reports whether a shell command is one plain read-only program: no shell
// syntax that could run or redirect anything, no argument that makes the program run or
// write something, and no path outside dir, the working directory (absolute elsewhere, ~ or
// ..).
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
			// A flag's value can be a path too (--file=/etc/x).
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

// splitWords splits a command into words the way a shell would for a simple command, and
// reports false for anything else: unquoted shell syntax (; & | < > $ ` ( ) { } * ? [ ] ! # ~
// and newlines), backslashes, and $, `, \ or ! inside double quotes.
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

// deniedInCluster reports whether a is a cluster of short options (-zi) with a denied
// one-letter option (-z) among them.
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

// readOnly reports whether tool, called with input, is one of TaskTo's tools that change
// nothing (docs/cli-tools.md), in the form cli names it: mcp__taskto__<tool> from Claude Code
// and Codex; taskto__<tool>, or the use_tool meta-tool calling it with its input inline, from
// Grok Build, whose search_tool, which only lists the tools, is read-only too. A turn without
// TaskTo's tools has none.
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
		// Only the inline form: {"tool_name", "tool_input": {…}}. The file forms read a file
		// first, which is the CLI's own tool, and ask; so does anything else.
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
