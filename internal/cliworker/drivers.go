package cliworker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A command line for one turn of a CLI. The prompt never goes in argv, where one starting
// with "-" would read as a flag: Claude Code and Codex read it from stdin, Grok Build from a
// file (it doesn't read stdin in headless mode), which the helper writes.
type command struct {
	args []string
	// promptFile: the prompt goes in a file, promptFilePlaceholder in args; else on stdin.
	promptFile bool
}

// turnCommand returns the command line of spec's turn.
func turnCommand(spec TurnSpec) command {
	switch spec.CLI {
	case Claude:
		// Asking: Claude Code's own rules decide what needs permission, and its
		// PermissionRequest hook asks the owner. Only its user settings (ours, rewritten every
		// run) apply, never project or local ones a command could write in the working
		// directory.
		mode := "default"
		if spec.Permissions == AllowAll {
			mode = "bypassPermissions"
		}
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", mode, "--setting-sources", "user"}
		// Only the MCP servers of our file, written for this run, never one from the user's
		// settings or a .mcp.json a command wrote in the working directory; none without tools.
		args = append(args, "--strict-mcp-config")
		if spec.Tools != nil {
			args = append(args, "--mcp-config", toolsFilePlaceholder)
		}
		if spec.Resume {
			args = append(args, "--resume", spec.Session)
		} else {
			args = append(args, "--session-id", spec.Session)
		}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		return command{args: args}
	case Codex:
		// The hooks file is ours, written for every run, so it needs no trust of its own; a
		// project's hooks still need a project trust that is never given.
		args := []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-hook-trust"}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if spec.Resume {
			args = append(args, "resume", spec.Session)
		}
		// "-": the prompt is stdin, which then closes; Codex waits on an open stdin.
		return command{args: append(args, "-")}
	default: // Grok
		args := []string{"--prompt-file", promptFilePlaceholder, "--output-format", "streaming-messages-json", "--permission-mode", "bypassPermissions", "--no-auto-update"}
		if spec.Resume {
			args = append(args, "-r", spec.Session)
		} else {
			args = append(args, "-s", spec.Session)
		}
		if spec.Model != "" {
			args = append(args, "-m", spec.Model)
		}
		return command{args: args, promptFile: true}
	}
}

// Overrides are extra settings for one CLI, for tests that point a real CLI at a gateway
// instead of a subscription. A production worker has none: a CLI's only credential is the
// user's own login in its config directory.
type Overrides struct {
	// Env is added to the CLI's environment (a variable named here replaces the default).
	Env []string
	// CodexConfig is appended to Codex's config.toml.
	CodexConfig string
}

// env returns the clean environment of a CLI run for dirs: built from nothing, so none of
// the server's variables (or keys) reach the CLI or the commands it runs. Its credentials are
// the login in its config directory, or the proxy's token, and the token for TaskTo's tools.
func env(cli CLI, slot Slot, dirs Dirs, path string, o Overrides, proxy *Proxy, tools *Tools) []string {
	vars := []string{
		"PATH=" + path,
		"HOME=" + dirs.Home,
		"LANG=C.UTF-8",
		"TMPDIR=" + dirs.Tmp,
	}
	switch cli {
	case Claude:
		vars = append(vars,
			"CLAUDE_CONFIG_DIR="+dirs.ConfigOf(Claude, slot),
			"DISABLE_AUTOUPDATER=1",
			"DISABLE_TELEMETRY=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		)
	case Codex:
		vars = append(vars, "CODEX_HOME="+dirs.ConfigOf(Codex, slot))
	case Grok:
		vars = append(vars, "GROK_HOME="+dirs.ConfigOf(Grok, slot), "GROK_DISABLE_AUTOUPDATER=1")
	}
	vars = append(vars, proxyEnv(cli, proxy)...)
	if tools != nil {
		vars = append(vars, toolsTokenVar+"="+tools.Token)
	}
	return mergeEnv(vars, o.Env)
}

// TaskTo's own tools (docs/cli-tools.md): each CLI knows the server's MCP endpoint for turns as
// the server "taskto", from settings the helper writes before every run. The token is in the
// CLI's environment, and the settings name the variable, not the value.
const (
	// toolsServer is the MCP server's name in the CLIs' settings, and in their tool names
	// (mcp__taskto__read_doc, taskto__read_doc).
	toolsServer = "taskto"
	// toolsTokenVar holds the turn's token for the endpoint.
	toolsTokenVar = "TASKTO_TOOLS_TOKEN" //nolint:gosec // G101: a variable's name, not a credential
	// toolsFile is Claude Code's MCP settings (--mcp-config), in its config directory.
	toolsFile = "taskto-mcp.json"
	// toolsFilePlaceholder stands for toolsFile's path in Claude Code's arguments.
	toolsFilePlaceholder = "{mcp-config}"
	// toolsTimeout bounds a call, in seconds: a browser tool takes up to a minute.
	toolsTimeout = 120
)

// toolsHeader is the Authorization header's value, with the variable for Claude Code and Grok
// Build to expand.
const toolsHeader = "Bearer ${" + toolsTokenVar + "}"

// writeClaudeTools writes Claude Code's MCP settings for t into its config directory, or
// removes them for a run without tools.
func writeClaudeTools(config string, t *Tools) error {
	path := filepath.Join(config, toolsFile)
	if t == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{toolsServer: map[string]any{
		"type": "http", "url": t.URL, "headers": map[string]string{"Authorization": toolsHeader},
	}}}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// toolsTOML is the MCP server of t in Codex's or Grok Build's config.toml: a table, so it goes
// after every top-level key. Codex reads the token from the variable; its own approval of the
// server's tools is "approve", since its PreToolUse hook (ours) is the gate, and it otherwise
// refuses every tool without readOnlyHint under approval_policy "never". Grok Build expands the
// variable in the header.
func toolsTOML(cli CLI, t *Tools) string {
	lines := []string{"[mcp_servers." + toolsServer + "]", "url = " + tomlString(t.URL)}
	if cli == Codex {
		lines = append(lines, "bearer_token_env_var = "+tomlString(toolsTokenVar), `default_tools_approval_mode = "approve"`)
	} else {
		lines = append(lines, "headers = { Authorization = "+tomlString(toolsHeader)+" }")
	}
	lines = append(lines, "startup_timeout_sec = 20", fmt.Sprintf("tool_timeout_sec = %d", toolsTimeout))
	return strings.Join(lines, "\n")
}

// proxyCodexKey is the variable that holds the proxy's token for Codex (its env_key).
const proxyCodexKey = "TASKTO_PROXY_TOKEN"

// proxyEnv points cli at the server's proxy with the turn's token, the same settings a
// gateway takes: the CLI never sees the shared account's key. None without a proxy.
func proxyEnv(cli CLI, p *Proxy) []string {
	if p == nil {
		return nil
	}
	switch cli {
	case Claude:
		vars := []string{"ANTHROPIC_BASE_URL=" + p.base() + "/cli-proxy", "ANTHROPIC_AUTH_TOKEN=" + p.Token}
		if p.Model != "" {
			vars = append(vars, "ANTHROPIC_MODEL="+p.Model, "ANTHROPIC_SMALL_FAST_MODEL="+p.Model, "ANTHROPIC_DEFAULT_HAIKU_MODEL="+p.Model)
		}
		return vars
	case Codex:
		return []string{proxyCodexKey + "=" + p.Token}
	default:
		return []string{"XAI_API_KEY=" + p.Token, "GROK_CLI_CHAT_PROXY_BASE_URL=" + p.base() + "/cli-proxy/v1"}
	}
}

// proxyCodexConfig is Codex's model provider for the server's proxy.
func proxyCodexConfig(p *Proxy) string {
	lines := []string{
		`model_provider = "taskto"`,
		"[model_providers.taskto]",
		`name = "TaskTo.AI shared account"`,
		"base_url = " + tomlString(p.base()+"/cli-proxy/v1"),
		`env_key = "` + proxyCodexKey + `"`,
		`wire_api = "responses"`,
	}
	if p.Model != "" {
		// A top-level key: it goes before the table.
		lines = append([]string{"model = " + tomlString(p.Model)}, lines...)
	}
	return strings.Join(lines, "\n")
}

// mergeEnv returns base with extra added, an extra variable replacing one of the same name.
func mergeEnv(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	replaced := map[string]bool{}
	for _, kv := range extra {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !replaced[name] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// prepare writes the settings a CLI reads from its config directory (the slot's) before every
// run, so a command that changed them can't change the next run: the permission hook that
// runs approve (docs/agent-safety.md) for each CLI, TaskTo's tools (or none), and Codex's
// config.toml (no approval prompts of its own, no update check, the sandbox,
// "workspace-write" unless given, and a shared account's proxy).
func prepare(cli CLI, slot Slot, dirs Dirs, o Overrides, sandbox string, proxy *Proxy, tools *Tools, approve []string) error {
	config := dirs.ConfigOf(cli, slot)
	if err := writeHooks(cli, config, approve, tools); err != nil {
		return err
	}
	switch cli {
	case Claude:
		return writeClaudeTools(config, tools)
	case Grok:
		return nil
	}
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	lines := []string{
		"# Written by TaskTo.AI for every run; changes here are replaced.",
		`approval_policy = "never"`,
		`sandbox_mode = "` + sandbox + `"`,
		"check_for_update_on_startup = false",
	}
	if proxy != nil {
		lines = append(lines, proxyCodexConfig(proxy))
	}
	if o.CodexConfig != "" {
		lines = append(lines, o.CodexConfig)
	}
	if tools != nil {
		lines = append(lines, toolsTOML(Codex, tools))
	}
	return writeFileAtomic(filepath.Join(dirs.ConfigOf(Codex, slot), "config.toml"), []byte(strings.Join(lines, "\n")+"\n"))
}

// writeFileAtomic replaces path with data (mode 600) in one rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeHooks writes cli's permission hook, which runs approve (the hook program and its
// arguments): Claude Code's PermissionRequest in settings.json, Codex's PreToolUse in
// hooks.json, Grok Build's PreToolUse in config.toml, failing closed, with TaskTo's tools
// after it. Without approve, the files are written without a hook.
func writeHooks(cli CLI, config string, approve []string, tools *Tools) error {
	switch cli {
	case Claude, Codex:
		event, file := hookPermissionRequest, "settings.json"
		if cli == Codex {
			event, file = hookPreToolUse, "hooks.json"
		}
		hooks := map[string]any{}
		if len(approve) > 0 {
			hooks[event] = []any{map[string]any{
				"matcher": "*",
				"hooks":   []any{map[string]any{"type": "command", "command": shellJoin(approve), "timeout": hookTimeout}},
			}}
		}
		b, err := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(config, file), append(b, '\n'))
	default: // Grok
		lines := []string{"# Written by TaskTo.AI for every run; changes here are replaced."}
		if len(approve) > 0 {
			args := make([]string, 0, len(approve)-1)
			for _, a := range approve[1:] {
				args = append(args, tomlString(a))
			}
			lines = append(lines,
				"[[hooks.PreToolUse]]",
				fmt.Sprintf("hooks = [ { type = \"command\", command = %s, args = [%s], timeout = %d, failClosed = true } ]",
					tomlString(approve[0]), strings.Join(args, ", "), hookTimeout),
			)
		}
		if tools != nil {
			lines = append(lines, toolsTOML(Grok, tools))
		}
		return writeFileAtomic(filepath.Join(config, "config.toml"), []byte(strings.Join(lines, "\n")+"\n"))
	}
}

// shellJoin quotes args for a shell, which Claude Code and Codex run a hook's command with.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// tomlString is s as a TOML basic string (JSON's escapes are TOML's).
func tomlString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSpace(b.String())
}
