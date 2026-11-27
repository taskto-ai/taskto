package cliworker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type command struct {
	args       []string
	promptFile bool
}

func turnCommand(spec TurnSpec) command {
	switch spec.CLI {
	case Claude:
		mode := "default"
		if spec.Permissions == AllowAll {
			mode = "bypassPermissions"
		}
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", mode, "--setting-sources", "user"}
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
		args := []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-hook-trust"}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if spec.Resume {
			args = append(args, "resume", spec.Session)
		}
		return command{args: append(args, "-")}
	default:
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

type Overrides struct {
	Env         []string
	CodexConfig string
}

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

const (
	toolsServer          = "taskto"
	toolsTokenVar        = "TASKTO_TOOLS_TOKEN"
	toolsFile            = "taskto-mcp.json"
	toolsFilePlaceholder = "{mcp-config}"
	toolsTimeout         = 120
)

const toolsHeader = "Bearer ${" + toolsTokenVar + "}"

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

const proxyCodexKey = "TASKTO_PROXY_TOKEN"

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

func proxyCodexConfig(p *Proxy) string {
	lines := []string{
		`model_provider = "taskto"`,
		"[model_providers.taskto]",
		`name = "taskto.ai shared account"`,
		"base_url = " + tomlString(p.base()+"/cli-proxy/v1"),
		`env_key = "` + proxyCodexKey + `"`,
		`wire_api = "responses"`,
	}
	if p.Model != "" {
		lines = append([]string{"model = " + tomlString(p.Model)}, lines...)
	}
	return strings.Join(lines, "\n")
}

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
		"# Written by taskto.ai for every run; changes here are replaced.",
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
	default:
		lines := []string{"# Written by taskto.ai for every run; changes here are replaced."}
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

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func tomlString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSpace(b.String())
}
