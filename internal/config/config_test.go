package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "taskto.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, args []string, vars map[string]string) Config {
	t.Helper()
	cfg, err := Load(args, env(vars), io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := load(t, nil, nil)
	if cfg != Default() {
		t.Fatalf("cfg = %+v, want the defaults %+v", cfg, Default())
	}
	if cfg.InsecureCookies {
		t.Fatal("insecure cookies are on by default")
	}
	if cfg.Agent || cfg.AgentNode != "node" || cfg.AgentEntry != "" || cfg.AgentProviders != "" {
		t.Fatalf("agent runtime defaults = %+v, want off, node from PATH, no entry or providers", cfg)
	}
}

// TestPrecedence sets every value in each layer and checks that the higher layer wins:
// flag > environment > file > default.
func TestPrecedence(t *testing.T) {
	file := writeConfig(t, `
# every setting, from the file
addr = "file:1"
data_dir = "/file" # trailing comment
insecure_cookies = true
agent = true
agent_node = "/file/node"
agent_entry = "/file/index.js"
agent_providers = "/file/providers.json"
`)
	allEnv := map[string]string{
		"TASKTO_CONFIG":           file,
		"TASKTO_ADDR":             "env:2",
		"TASKTO_DATA_DIR":         "/env",
		"TASKTO_INSECURE_COOKIES": "false",
		"TASKTO_AGENT":            "false",
		"TASKTO_AGENT_NODE":       "/env/node",
		"TASKTO_AGENT_ENTRY":      "/env/index.js",
		"TASKTO_AGENT_PROVIDERS":  "/env/providers.json",
	}
	allFlags := []string{"-addr", "flag:3", "-data-dir", "/flag", "-insecure-cookies",
		"-agent", "-agent-node", "/flag/node", "-agent-entry", "/flag/index.js", "-agent-providers", "/flag/providers.json"}
	fromFile := Config{Agent: true, AgentNode: "/file/node", AgentEntry: "/file/index.js", AgentProviders: "/file/providers.json"}
	fromEnv := Config{Agent: false, AgentNode: "/env/node", AgentEntry: "/env/index.js", AgentProviders: "/env/providers.json"}
	fromFlags := Config{Agent: true, AgentNode: "/flag/node", AgentEntry: "/flag/index.js", AgentProviders: "/flag/providers.json"}
	with := func(agent Config, c Config) Config {
		c.Agent, c.AgentNode, c.AgentEntry, c.AgentProviders = agent.Agent, agent.AgentNode, agent.AgentEntry, agent.AgentProviders
		return c
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want Config
	}{
		{"file over default", nil, map[string]string{"TASKTO_CONFIG": file},
			with(fromFile, Config{Addr: "file:1", DataDir: "/file", InsecureCookies: true, ConfigFile: file})},
		{"env over file", nil, allEnv,
			with(fromEnv, Config{Addr: "env:2", DataDir: "/env", ConfigFile: file})},
		{"flag over env", allFlags, allEnv,
			with(fromFlags, Config{Addr: "flag:3", DataDir: "/flag", InsecureCookies: true, ConfigFile: file})},
		{"flag set back to the default still wins", []string{"-addr", "127.0.0.1:8080", "-insecure-cookies=false", "-agent-node", "node"}, allEnv,
			with(Config{AgentNode: "node", AgentEntry: "/env/index.js", AgentProviders: "/env/providers.json"},
				Config{Addr: "127.0.0.1:8080", DataDir: "/env", ConfigFile: file})},
		{"env over default", nil, map[string]string{"TASKTO_ADDR": ":9000", "TASKTO_AGENT": "true"},
			Config{Addr: ":9000", DataDir: "data", Agent: true, AgentNode: "node"}},
		{"empty env is unset", nil, map[string]string{"TASKTO_CONFIG": file, "TASKTO_ADDR": "", "TASKTO_AGENT_NODE": ""},
			with(fromFile, Config{Addr: "file:1", DataDir: "/file", InsecureCookies: true, ConfigFile: file})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := load(t, tt.args, tt.env); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigFlagWinsOverConfigEnv(t *testing.T) {
	fromEnv := writeConfig(t, `addr = "env-file:1"`)
	fromFlag := writeConfig(t, `addr = "flag-file:2"`)
	cfg := load(t, []string{"-config", fromFlag}, map[string]string{"TASKTO_CONFIG": fromEnv})
	if cfg.Addr != "flag-file:2" || cfg.ConfigFile != fromFlag {
		t.Fatalf("cfg = %+v, want the file named by -config", cfg)
	}
}

func TestVersionFlag(t *testing.T) {
	if !load(t, []string{"-version"}, nil).ShowVersion {
		t.Fatal("-version did not set ShowVersion")
	}
}

func TestFileStrings(t *testing.T) {
	file := writeConfig(t, `data_dir = "/srv/task#to \"data\""`)
	if got := load(t, []string{"-config", file}, nil).DataDir; got != `/srv/task#to "data"` {
		t.Fatalf("DataDir = %q", got)
	}
}

func TestStorageQuotasAreUnlimitedUntilSet(t *testing.T) {
	if cfg := load(t, nil, nil); cfg.StorageQuotaWorkspace != 0 || cfg.StorageQuotaUser != 0 {
		t.Fatalf("quotas by default = %d, %d; want none", cfg.StorageQuotaWorkspace, cfg.StorageQuotaUser)
	}
	file := writeConfig(t, "storage_quota_user = \"2 GiB\"")
	cfg := load(t, []string{"-config", file, "-storage-quota-workspace", "100GiB"}, nil)
	if cfg.StorageQuotaWorkspace != 100<<30 || cfg.StorageQuotaUser != 2<<30 {
		t.Fatalf("quotas = %d, %d; want 100 GiB and 2 GiB", cfg.StorageQuotaWorkspace, cfg.StorageQuotaUser)
	}
	if cfg := load(t, nil, map[string]string{"TASKTO_STORAGE_QUOTA_USER": "512MiB"}); cfg.StorageQuotaUser != 512<<20 {
		t.Fatalf("user quota from the environment = %d", cfg.StorageQuotaUser)
	}
	if _, err := Load([]string{"-storage-quota-user", "0"}, env(nil), io.Discard); err == nil {
		t.Error("a quota of 0 is written by leaving it out: want an error")
	}
}

func TestSizes(t *testing.T) {
	file := writeConfig(t, "kb_max_file_size = \"2 MiB\"\nkb_large_file_size = \"1 MiB\"")
	cfg := load(t, []string{"-config", file, "-kb-max-repo-size", "3GiB"}, map[string]string{"TASKTO_KB_MAX_UNVERSIONED_SIZE": "5GiB"})
	if cfg.KBMaxFileSize != 2<<20 || cfg.KBMaxRepoSize != 3<<30 || cfg.KBLargeFileSize != 1<<20 || cfg.KBMaxUnversionedSize != 5<<30 {
		t.Fatalf("sizes = %d, %d, %d, %d; want 2 MiB, 3 GiB, 1 MiB and 5 GiB",
			cfg.KBMaxFileSize, cfg.KBMaxRepoSize, cfg.KBLargeFileSize, cfg.KBMaxUnversionedSize)
	}
	cfg = load(t, nil, map[string]string{"TASKTO_KB_MAX_FILE_SIZE": "1048577", "TASKTO_KB_MAX_REPO_SIZE": "64KiB"})
	if cfg.KBMaxFileSize != 1<<20+1 || cfg.KBMaxRepoSize != 64<<10 {
		t.Fatalf("sizes = %d, %d; want 1048577 and 64 KiB", cfg.KBMaxFileSize, cfg.KBMaxRepoSize)
	}
	for _, bad := range []string{"0", "-1", "10MB", "MiB", "1.5GiB", "9999999999999GiB"} {
		if _, err := Load([]string{"-kb-max-file-size", bad}, env(nil), io.Discard); err == nil {
			t.Errorf("-kb-max-file-size %q: no error", bad)
		}
	}
}

func TestSearchVectorCache(t *testing.T) {
	if got := load(t, nil, nil).SearchVectorCache; got != 0 {
		t.Fatalf("default = %d, want 0 (the search index's)", got)
	}
	if got := load(t, nil, map[string]string{"TASKTO_SEARCH_VECTOR_CACHE": "512MiB"}).SearchVectorCache; got != 512<<20 {
		t.Fatalf("from the environment = %d, want 512 MiB", got)
	}
	file := writeConfig(t, `search_vector_cache = "off"`)
	if got := load(t, []string{"-config", file}, nil).SearchVectorCache; got != -1 {
		t.Fatalf("off = %d, want -1", got)
	}
	if got := load(t, []string{"-config", file, "-search-vector-cache", "2GiB"}, nil).SearchVectorCache; got != 2<<30 {
		t.Fatalf("flag over file = %d, want 2 GiB", got)
	}
	for _, bad := range []string{"0", "-1", "none", "1.5GiB"} {
		if _, err := Load([]string{"-search-vector-cache", bad}, env(nil), io.Discard); err == nil {
			t.Errorf("-search-vector-cache %q: no error", bad)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		file    string
		wantErr string
	}{
		{name: "unknown flag", args: []string{"-nope"}, wantErr: "not defined"},
		{name: "positional argument", args: []string{"serve"}, wantErr: `unexpected argument "serve"`},
		{name: "bad bool flag", args: []string{"-insecure-cookies=maybe"}, wantErr: "-insecure-cookies: want true or false"},
		{name: "bad bool env", env: map[string]string{"TASKTO_INSECURE_COOKIES": "maybe"}, wantErr: "TASKTO_INSECURE_COOKIES"},
		{name: "empty address", args: []string{"-addr", ""}, wantErr: "listen address is empty"},
		{name: "agent without node", args: []string{"-agent", "-agent-node", ""}, wantErr: "Node.js executable is empty"},
		{name: "bad agent env", env: map[string]string{"TASKTO_AGENT": "yes"}, wantErr: "TASKTO_AGENT:"},
		{name: "missing file", args: []string{"-config", "/does/not/exist.toml"}, wantErr: "no such file"},
		{name: "unknown key", file: "port = \"80\"", wantErr: `line 1: unknown key "port"`},
		{name: "repeated key", file: "addr = \"a:1\"\naddr = \"b:2\"", wantErr: "line 2: addr is set twice"},
		{name: "no equals sign", file: "addr", wantErr: "line 1: want key = value"},
		{name: "unquoted string", file: "addr = :80", wantErr: "want a double-quoted string"},
		{name: "unterminated string", file: `addr = "a:1`, wantErr: "bad string"},
		{name: "quoted bool", file: `insecure_cookies = "true"`, wantErr: "want true or false"},
		{name: "table", file: "[server]\naddr = \"a:1\"", wantErr: "line 1: want key = value"},
		{name: "bad trusted proxy", env: map[string]string{"TASKTO_TRUSTED_PROXIES": "10.0.0.0/8, proxy.example.com"}, wantErr: `TASKTO_TRUSTED_PROXIES: trusted proxies: "proxy.example.com" isn't an IP address`},
		{name: "bad trusted prefix", args: []string{"-trusted-proxies", "10.0.0.0/33"}, wantErr: "isn't a CIDR prefix"},
		{name: "bad egress policy", env: map[string]string{"TASKTO_PROVIDER_EGRESS_PERSONAL": "public, any"}, wantErr: `TASKTO_PROVIDER_EGRESS_PERSONAL: egress policy: "any" can't be combined`},
		{name: "bad egress entry", args: []string{"-provider-egress-workspace", "public, ollama:11434"}, wantErr: "isn't a host name, IP address or CIDR prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if tt.file != "" {
				args = append(args, "-config", writeConfig(t, tt.file))
			}
			_, err := Load(args, env(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestTrustedProxies(t *testing.T) {
	file := writeConfig(t, `trusted_proxies = "10.1.2.3/8, 192.0.2.7 ,2001:db8::/32,::ffff:198.51.100.1"`)
	cfg := load(t, []string{"-config", file}, nil)
	var got []string
	for _, p := range cfg.TrustedProxyPrefixes() {
		got = append(got, p.String())
	}
	if want := "10.0.0.0/8 192.0.2.7/32 2001:db8::/32 198.51.100.1/32"; strings.Join(got, " ") != want {
		t.Fatalf("trusted proxies = %v, want %s", got, want)
	}
	if p := load(t, nil, nil).TrustedProxyPrefixes(); len(p) != 0 {
		t.Fatalf("trusted proxies by default = %v, want none", p)
	}
}

func TestProviderEgress(t *testing.T) {
	ws, personal := load(t, nil, nil).Egress()
	if !ws.IsAny() || !personal.IsAny() {
		t.Fatalf("egress by default = %s, %s; want any for both", ws, personal)
	}
	file := writeConfig(t, `provider_egress_workspace = "public, 10.1.2.3/24"`)
	ws, personal = load(t, []string{"-config", file}, map[string]string{"TASKTO_PROVIDER_EGRESS_PERSONAL": "public"}).Egress()
	if ws.String() != "public,10.1.2.0/24" || personal.String() != "public" {
		t.Fatalf("egress = %s, %s", ws, personal)
	}
}

func TestHelp(t *testing.T) {
	var out strings.Builder
	_, err := Load([]string{"-h"}, env(nil), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"-addr", "TASKTO_ADDR", "-data-dir", "TASKTO_DATA_DIR", "-insecure-cookies", "TASKTO_INSECURE_COOKIES", "-config",
		"-agent", "TASKTO_AGENT", "-agent-node", "TASKTO_AGENT_NODE", "-agent-entry", "TASKTO_AGENT_ENTRY", "-agent-providers", "TASKTO_AGENT_PROVIDERS", "-trusted-proxies", "TASKTO_TRUSTED_PROXIES",
		"-provider-egress-workspace", "TASKTO_PROVIDER_EGRESS_WORKSPACE", "-provider-egress-personal", "TASKTO_PROVIDER_EGRESS_PERSONAL",
		"-cli-workers", "TASKTO_CLI_WORKERS", "-cli-worker-socket", "TASKTO_CLI_WORKER_SOCKET",
		"-cli-proxy-listen", "TASKTO_CLI_PROXY_LISTEN", "-cli-proxy-url", "TASKTO_CLI_PROXY_URL",
		"-browser", "TASKTO_BROWSER", "-browser-chromium", "TASKTO_BROWSER_CHROMIUM"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage doesn't mention %s:\n%s", want, out.String())
		}
	}
}

func TestCLIProxy(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{"TASKTO_CLI_PROXY_LISTEN": "0.0.0.0:8081", "TASKTO_CLI_PROXY_URL": "http://taskto:8081"}), io.Discard)
	if err != nil || cfg.CLIProxyListen != "0.0.0.0:8081" || cfg.CLIProxyURL != "http://taskto:8081" {
		t.Fatalf("proxy: %+v %v", cfg, err)
	}
	for _, bad := range []string{"taskto:8081", "ftp://taskto", "http://u:p@taskto:8081", "http://taskto:8081/v1", "http://taskto:8081?x=1", "http://taskto:8081?", "http://taskto:8081#x"} {
		if _, err := Load([]string{"-cli-proxy-listen", ":8081", "-cli-proxy-url", bad}, env(nil), io.Discard); err == nil {
			t.Errorf("proxy URL %q accepted", bad)
		}
	}
	if _, err := Load([]string{"-cli-proxy-url", "http://taskto:8081"}, env(nil), io.Discard); err == nil {
		t.Error("a proxy URL without a listener accepted")
	}

	// The Compose file's: a socket the worker relays, since the worker's network can't reach
	// the server (docs/cli-workers.md).
	container := []string{"-cli-workers", "container", "-cli-worker-socket", "/run/taskto-cli/worker.sock"}
	cfg, err = Load(append(container, "-cli-proxy-listen", "unix:/run/taskto-cli/proxy/proxy.sock", "-cli-proxy-url", "http://127.0.0.1:8081"), env(nil), io.Discard)
	if err != nil || cfg.CLIProxyListen != "unix:/run/taskto-cli/proxy/proxy.sock" {
		t.Fatalf("proxy on a socket: %+v %v", cfg, err)
	}
	for name, args := range map[string][]string{
		"relative path":  append(container, "-cli-proxy-listen", "unix:proxy.sock", "-cli-proxy-url", "http://127.0.0.1:8081"),
		"unclean path":   append(container, "-cli-proxy-listen", "unix:/run/../proxy.sock", "-cli-proxy-url", "http://127.0.0.1:8081"),
		"no URL":         append(container, "-cli-proxy-listen", "unix:/run/taskto-cli/proxy/proxy.sock"),
		"not container":  {"-cli-workers", "local", "-cli-proxy-listen", "unix:/tmp/proxy.sock", "-cli-proxy-url", "http://127.0.0.1:8081"},
		"no CLI workers": {"-cli-proxy-listen", "unix:/tmp/proxy.sock", "-cli-proxy-url", "http://127.0.0.1:8081"},
	} {
		if _, err := Load(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The agent browser runs in the CLI worker: it needs CLI workers on.
func TestBrowser(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil || cfg.Browser {
		t.Fatalf("default: %v %v", cfg.Browser, err)
	}
	if _, err := Load([]string{"-browser"}, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "-cli-workers") {
		t.Fatalf("-browser without CLI workers: %v", err)
	}
	cfg, err = Load([]string{"-browser", "-cli-workers", "local", "-browser-chromium", "/opt/chromium"}, env(nil), io.Discard)
	if err != nil || !cfg.Browser || cfg.BrowserChromium != "/opt/chromium" {
		t.Fatalf("on: %+v %v", cfg, err)
	}
	cfg, err = Load(nil, env(map[string]string{"TASKTO_BROWSER": "true", "TASKTO_CLI_WORKERS": "container", "TASKTO_CLI_WORKER_SOCKET": "/run/w.sock"}), io.Discard)
	if err != nil || !cfg.Browser {
		t.Fatalf("env: %v %v", cfg.Browser, err)
	}
}

func TestCLIWorkers(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil || cfg.CLIWorkers != "" {
		t.Fatalf("default: %q %v", cfg.CLIWorkers, err)
	}
	cfg, err = Load([]string{"-cli-workers", "container", "-cli-worker-socket", "/run/w.sock"}, env(nil), io.Discard)
	if err != nil || cfg.CLIWorkers != "container" || cfg.CLIWorkerSocket != "/run/w.sock" {
		t.Errorf("container: %+v %v", cfg, err)
	}
	if _, err := Load([]string{"-cli-workers", "container"}, env(nil), io.Discard); err == nil {
		t.Error("container without a socket accepted")
	}
	for _, v := range []string{"off", "local", "local-unsafe"} {
		cfg, err := Load([]string{"-cli-workers", v}, env(nil), io.Discard)
		if err != nil || cfg.CLIWorkers != v {
			t.Errorf("%s: %q %v", v, cfg.CLIWorkers, err)
		}
		cfg, err = Load(nil, env(map[string]string{"TASKTO_CLI_WORKERS": v}), io.Discard)
		if err != nil || cfg.CLIWorkers != v {
			t.Errorf("env %s: %q %v", v, cfg.CLIWorkers, err)
		}
	}
	for _, v := range []string{"on", "docker", "LOCAL"} {
		if _, err := Load([]string{"-cli-workers", v}, env(nil), io.Discard); err == nil {
			t.Errorf("%s accepted", v)
		}
	}
}

func TestPushContact(t *testing.T) {
	if got := load(t, nil, nil).PushContact; got != "" {
		t.Fatalf("push contact by default = %q, want none (the server's default applies)", got)
	}
	file := writeConfig(t, `push_contact = "mailto:file@example.org"`)
	// The file, then the environment over it, then the flag over both.
	if got := load(t, []string{"-config", file}, nil).PushContact; got != "mailto:file@example.org" {
		t.Fatalf("from the file: %q", got)
	}
	env := map[string]string{"TASKTO_PUSH_CONTACT": "https://ops.example.org/contact"}
	if got := load(t, []string{"-config", file}, env).PushContact; got != "https://ops.example.org/contact" {
		t.Fatalf("env over file: %q", got)
	}
	if got := load(t, []string{"-config", file, "-push-contact", "mailto:flag@example.org"}, env).PushContact; got != "mailto:flag@example.org" {
		t.Fatalf("flag over env: %q", got)
	}
}

func TestPushContactMustBeMailtoOrHTTPS(t *testing.T) {
	for _, bad := range []string{
		"http://example.org", "ftp://example.org", "example.org", "mailto:", "mailto:nobody", "mailto:@example.org",
		"mailto:a@", "https://", "https://user:pw@example.org", "https://exa mple.org", "javascript:alert(1)",
	} {
		if err := ValidatePushContact(bad); err == nil {
			t.Errorf("%q accepted as a push contact", bad)
		}
		if _, err := Load([]string{"-push-contact", bad}, func(string) string { return "" }, io.Discard); err == nil {
			t.Errorf("-push-contact %q accepted", bad)
		}
	}
	for _, good := range []string{"", "mailto:ops@example.org", "https://example.org", "https://example.org/contact?x=1"} {
		if err := ValidatePushContact(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}

func TestSSOSettings(t *testing.T) {
	file := writeConfig(t, `
sso_providers = "/file/sso.json"
public_url = "https://file.example.com"
disable_password_login = true
`)
	got := load(t, nil, map[string]string{"TASKTO_CONFIG": file})
	if got.SSOProviders != "/file/sso.json" || got.PublicURL != "https://file.example.com" || !got.DisablePasswordLogin {
		t.Fatalf("from the file: %+v", got)
	}
	got = load(t, nil, map[string]string{"TASKTO_CONFIG": file, "TASKTO_SSO_PROVIDERS": "/env/sso.json",
		"TASKTO_PUBLIC_URL": "https://env.example.com/", "TASKTO_DISABLE_PASSWORD_LOGIN": "false"})
	if got.SSOProviders != "/env/sso.json" || got.PublicURL != "https://env.example.com" || got.DisablePasswordLogin {
		t.Fatalf("from the environment: %+v", got)
	}
	got = load(t, []string{"-sso-providers", "/flag/sso.json", "-public-url", "https://flag.example.com", "-disable-password-login"},
		map[string]string{"TASKTO_CONFIG": file, "TASKTO_PUBLIC_URL": "https://env.example.com"})
	if got.SSOProviders != "/flag/sso.json" || got.PublicURL != "https://flag.example.com" || !got.DisablePasswordLogin {
		t.Fatalf("from flags: %+v", got)
	}
}

func TestSSOValidation(t *testing.T) {
	sso := map[string]string{"TASKTO_SSO_PROVIDERS": "/etc/taskto/sso.json"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range sso {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, tt := range map[string]struct {
		env  map[string]string
		fail string // a part of the error; "" means it loads
	}{
		"providers need a public url":           {sso, "-public-url"},
		"https":                                 {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com"}), ""},
		"a port":                                {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com:8443"}), ""},
		"http":                                  {with(map[string]string{"TASKTO_PUBLIC_URL": "http://taskto.example.com"}), "must be https"},
		"http localhost without insecure":       {with(map[string]string{"TASKTO_PUBLIC_URL": "http://localhost:8080"}), "must be https"},
		"http localhost":                        {with(map[string]string{"TASKTO_PUBLIC_URL": "http://localhost:8080", "TASKTO_INSECURE_COOKIES": "true"}), ""},
		"http 127.0.0.1":                        {with(map[string]string{"TASKTO_PUBLIC_URL": "http://127.0.0.1:8080", "TASKTO_INSECURE_COOKIES": "true"}), ""},
		"http [::1]":                            {with(map[string]string{"TASKTO_PUBLIC_URL": "http://[::1]:8080", "TASKTO_INSECURE_COOKIES": "true"}), ""},
		"http elsewhere with insecure cookies":  {with(map[string]string{"TASKTO_PUBLIC_URL": "http://taskto.example.com", "TASKTO_INSECURE_COOKIES": "true"}), "must be https"},
		"a path":                                {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com/app"}), "without a path"},
		"a query":                               {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com?a=b"}), "without a path"},
		"a fragment":                            {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com#a"}), "without a path"},
		"credentials":                           {with(map[string]string{"TASKTO_PUBLIC_URL": "https://user:fake@taskto.example.com"}), "without a path"}, //nolint:gosec // G101: a fake, to be refused
		"no host":                               {with(map[string]string{"TASKTO_PUBLIC_URL": "https://"}), "without a path"},
		"another scheme":                        {with(map[string]string{"TASKTO_PUBLIC_URL": "ftp://taskto.example.com"}), "must be https"},
		"passwords off without providers":       {map[string]string{"TASKTO_DISABLE_PASSWORD_LOGIN": "true"}, "-disable-password-login needs -sso-providers"},
		"passwords off with providers":          {with(map[string]string{"TASKTO_PUBLIC_URL": "https://taskto.example.com", "TASKTO_DISABLE_PASSWORD_LOGIN": "true"}), ""},
		"a public url alone is checked as well": {map[string]string{"TASKTO_PUBLIC_URL": "http://taskto.example.com"}, "must be https"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, env(tt.env), io.Discard)
			switch {
			case tt.fail == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tt.fail != "" && (err == nil || !strings.Contains(err.Error(), tt.fail)):
				t.Fatalf("Load error = %v, want one with %q", err, tt.fail)
			}
		})
	}
}

func TestUpdateStatusDir(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{"TASKTO_UPDATE_STATUS_DIR": "/update-status"}), io.Discard)
	if err != nil || cfg.UpdateStatusDir != "/update-status" {
		t.Fatalf("%q, %v", cfg.UpdateStatusDir, err)
	}
	if cfg, err := Load(nil, env(nil), io.Discard); err != nil || cfg.UpdateStatusDir != "" {
		t.Fatalf("default: %q, %v", cfg.UpdateStatusDir, err)
	}
	if _, err := Load([]string{"-update-status-dir", "relative/dir"}, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative path: %v", err)
	}
}

// Integrations' providers are reached through addresses checked on every connection unless the
// operator says otherwise: the default is "public", not "any".
func TestIntegrationEgress(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.IntegrationEgressPolicy(); p.IsAny() || p.String() != "public" {
		t.Fatalf("default policy = %q", p.String())
	}
	cfg, err = Load(nil, env(map[string]string{"TASKTO_INTEGRATION_EGRESS": "any"}), io.Discard)
	if err != nil || !cfg.IntegrationEgressPolicy().IsAny() {
		t.Fatalf("any: %v, %+v", err, cfg.IntegrationEgress)
	}
	cfg, err = Load([]string{"-integration-egress", "public"}, env(map[string]string{"TASKTO_INTEGRATION_EGRESS": "any"}), io.Discard)
	if err != nil || cfg.IntegrationEgressPolicy().IsAny() {
		t.Fatalf("the flag takes precedence: %v, %+v", err, cfg.IntegrationEgress)
	}
	if _, err := Load(nil, env(map[string]string{"TASKTO_INTEGRATION_EGRESS": "nonsense!"}), io.Discard); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("a bad policy: %v", err)
	}
}

// An empty setting is "public", never "any": it must not open calls that carry credentials.
func TestIntegrationEgressEmptyIsPublic(t *testing.T) {
	file := writeConfig(t, `integration_egress = ""`)
	for name, tt := range map[string]struct {
		args []string
		env  map[string]string
	}{
		"an empty flag":     {args: []string{"-integration-egress", ""}},
		"a blank flag":      {args: []string{"-integration-egress", "  "}},
		"an empty variable": {env: map[string]string{"TASKTO_INTEGRATION_EGRESS": ""}},
		"an empty file key": {args: []string{"-config", file}},
	} {
		cfg, err := Load(tt.args, env(tt.env), io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p := cfg.IntegrationEgressPolicy(); p.IsAny() || p.String() != "public" {
			t.Errorf("%s: policy = %q, want public", name, p.String())
		}
	}
	// The method fails closed on its own: an unset, blank or unparsable field is "public".
	for _, v := range []string{"", "  ", "nonsense!", "any, public", ",,"} {
		if p := (Config{IntegrationEgress: v}).IntegrationEgressPolicy(); p.IsAny() || p.String() != "public" {
			t.Errorf("IntegrationEgress = %q: policy = %q, want public", v, p.String())
		}
	}
	if p := (Config{IntegrationEgress: "any"}).IntegrationEgressPolicy(); !p.IsAny() {
		t.Error("an explicit any was refused")
	}
}

// "any." is a host name, not the opt-out: it must stay one when the policy is stored and read back.
func TestIntegrationEgressKeepsWhatWasWritten(t *testing.T) {
	for _, v := range []string{"any.", "ANY.", "public, any."} {
		cfg, err := Load([]string{"-integration-egress", v}, env(nil), io.Discard)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if cfg.IntegrationEgressPolicy().IsAny() {
			t.Errorf("%q was read as any", v)
		}
	}
	cfg, err := Load([]string{"-integration-egress", "ANY"}, env(nil), io.Discard)
	if err != nil || !cfg.IntegrationEgressPolicy().IsAny() {
		t.Errorf("a literal any is the opt-out: %v", err)
	}
}

func TestSMTPSettings(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{ //nolint:gosec // G101: names of settings and a path, not credentials
		"TASKTO_SMTP_HOST": "smtp.example.com", "TASKTO_SMTP_FROM": "taskto <taskto@example.com>",
		"TASKTO_SMTP_USERNAME": "taskto", "TASKTO_SMTP_PASSWORD_FILE": "/run/secrets/smtp",
		"TASKTO_SMTP_EGRESS": "public",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPSecurityMode() != "starttls" || cfg.SMTPPortNumber() != 587 ||
		cfg.SMTPUsername != "taskto" || cfg.SMTPPasswordFile != "/run/secrets/smtp" || cfg.SMTPEgressPolicy().IsAny() {
		t.Fatalf("config = %+v", cfg)
	}
	if !strings.Contains(cfg.SMTPFrom, "taskto@example.com") {
		t.Fatalf("from = %q", cfg.SMTPFrom)
	}

	for security, port := range map[string]int{"starttls": 587, "tls": 465, "none": 25} {
		c, err := Load([]string{"-smtp-host", "relay.example.com", "-smtp-from", "a@example.com", "-smtp-security", security}, env(nil), io.Discard)
		if err != nil || c.SMTPPortNumber() != port {
			t.Fatalf("%s: port %d, %v", security, c.SMTPPortNumber(), err)
		}
	}
	c, err := Load([]string{"-smtp-host", "relay.example.com", "-smtp-from", "a@example.com", "-smtp-port", "2525"}, env(nil), io.Discard)
	if err != nil || c.SMTPPortNumber() != 2525 {
		t.Fatalf("port %d, %v", c.SMTPPortNumber(), err)
	}
}

func TestSMTPValidation(t *testing.T) {
	base := map[string]string{"TASKTO_SMTP_HOST": "smtp.example.com", "TASKTO_SMTP_FROM": "taskto@example.com"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, tt := range map[string]struct {
		env  map[string]string
		fail string
	}{
		"none":                     {nil, ""},
		"a host and a sender":      {base, ""},
		"a host without sender":    {map[string]string{"TASKTO_SMTP_HOST": "smtp.example.com"}, "needs -smtp-from"},
		"a sender without host":    {map[string]string{"TASKTO_SMTP_FROM": "taskto@example.com"}, "need -smtp-host"},
		"a bad sender":             {with(map[string]string{"TASKTO_SMTP_FROM": "not an address"}), "TASKTO_SMTP_FROM"},
		"a host with a port":       {map[string]string{"TASKTO_SMTP_HOST": "smtp.example.com:587", "TASKTO_SMTP_FROM": "a@example.com"}, "-smtp-port"},
		"a bad port":               {with(map[string]string{"TASKTO_SMTP_PORT": "70000"}), "port"},
		"a bad security":           {with(map[string]string{"TASKTO_SMTP_SECURITY": "ssl"}), "starttls, tls or none"},
		"a username alone":         {with(map[string]string{"TASKTO_SMTP_USERNAME": "u"}), "go together"},
		"a relative password":      {with(map[string]string{"TASKTO_SMTP_USERNAME": "u", "TASKTO_SMTP_PASSWORD_FILE": "smtp"}), "absolute"},
		"credentials in clear":     {with(map[string]string{"TASKTO_SMTP_USERNAME": "u", "TASKTO_SMTP_PASSWORD_FILE": "/p", "TASKTO_SMTP_SECURITY": "none"}), "clear text"},
		"clear to a loopback":      {map[string]string{"TASKTO_SMTP_HOST": "127.0.0.1", "TASKTO_SMTP_FROM": "a@example.com", "TASKTO_SMTP_USERNAME": "u", "TASKTO_SMTP_PASSWORD_FILE": "/p", "TASKTO_SMTP_SECURITY": "none"}, "clear text"},
		"clear to localhost":       {map[string]string{"TASKTO_SMTP_HOST": "localhost", "TASKTO_SMTP_FROM": "a@example.com", "TASKTO_SMTP_USERNAME": "u", "TASKTO_SMTP_PASSWORD_FILE": "/p", "TASKTO_SMTP_SECURITY": "none"}, "clear text"},
		"a relay without sign-in":  {map[string]string{"TASKTO_SMTP_HOST": "127.0.0.1", "TASKTO_SMTP_FROM": "a@example.com", "TASKTO_SMTP_SECURITY": "none"}, ""},
		"an IPv6 host with a port": {map[string]string{"TASKTO_SMTP_HOST": "[::1]:587", "TASKTO_SMTP_FROM": "a@example.com"}, "-smtp-port"},
		"an IPv6 zone":             {map[string]string{"TASKTO_SMTP_HOST": "fe80::1%eth0", "TASKTO_SMTP_FROM": "a@example.com"}, "-smtp-port"},
		"a bad egress":             {with(map[string]string{"TASKTO_SMTP_EGRESS": "nonsense!"}), "egress"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, env(tt.env), io.Discard)
			switch {
			case tt.fail == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tt.fail != "" && (err == nil || !strings.Contains(err.Error(), tt.fail)):
				t.Fatalf("Load error = %v, want one with %q", err, tt.fail)
			}
		})
	}
}

// An IPv6 address is a host, bracketed or not, and is kept without brackets, as the connection
// joins it to the port; a host with a port is still refused.
func TestSMTPHostTakesIPv6Literals(t *testing.T) {
	for in, want := range map[string]string{
		"smtp.example.com": "smtp.example.com", "::1": "::1", "[::1]": "::1", "[2001:DB8::25]": "2001:db8::25", "2001:db8::25": "2001:db8::25",
	} {
		for how, args := range map[string][]string{
			"flag": {"-smtp-host", in, "-smtp-from", "a@example.com"},
			"env":  nil,
		} {
			e := env(map[string]string{})
			if how == "env" {
				e = env(map[string]string{"TASKTO_SMTP_HOST": in, "TASKTO_SMTP_FROM": "a@example.com"})
			}
			c, err := Load(args, e, io.Discard)
			if err != nil || c.SMTPHost != want {
				t.Errorf("%s %q: host %q, %v; want %q", how, in, c.SMTPHost, err, want)
			}
		}
	}
	for _, in := range []string{"[::1]:25", "::1]", "[::1", "1.2.3.4:25", "smtp.example.com:587"} {
		if _, err := Load([]string{"-smtp-host", in, "-smtp-from", "a@example.com"}, env(nil), io.Discard); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestAuditRetention(t *testing.T) {
	if got := load(t, nil, nil).AuditRetention(); got != 365*24*time.Hour {
		t.Fatalf("by default %v, want a year", got)
	}
	file := writeConfig(t, `audit_retention = "90"`)
	// The file, then the environment over it, then the flag over both.
	if got := load(t, []string{"-config", file}, nil).AuditRetention(); got != 90*24*time.Hour {
		t.Fatalf("from the file: %v", got)
	}
	env := map[string]string{"TASKTO_AUDIT_RETENTION": "30"}
	if got := load(t, []string{"-config", file}, env).AuditRetention(); got != 30*24*time.Hour {
		t.Fatalf("env over file: %v", got)
	}
	if got := load(t, []string{"-config", file, "-audit-retention", "7"}, env).AuditRetention(); got != 7*24*time.Hour {
		t.Fatalf("flag over env: %v", got)
	}
	// Off keeps everything: a retention of zero, which the sweep reads as no limit.
	if got := load(t, []string{"-audit-retention", "off"}, nil).AuditRetention(); got != 0 {
		t.Fatalf("off: %v, want 0", got)
	}
}

func TestAuditRetentionMustBeDaysOrOff(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc", "1.5", "40000", "1y", ""} {
		if _, err := Load([]string{"-audit-retention", bad}, env(nil), io.Discard); err == nil {
			t.Errorf("-audit-retention %q accepted", bad)
		}
	}
}

func TestWorkspaceCreationSetting(t *testing.T) {
	if got := load(t, nil, nil).WorkspaceCreationMode(); got != "admins" {
		t.Fatalf("default = %q, want admins", got)
	}
	file := writeConfig(t, `workspace_creation = "off"`)
	if got := load(t, nil, map[string]string{"TASKTO_CONFIG": file}).WorkspaceCreationMode(); got != "off" {
		t.Fatalf("from the file: %q", got)
	}
	if got := load(t, nil, map[string]string{"TASKTO_CONFIG": file, "TASKTO_WORKSPACE_CREATION": "anyone"}).WorkspaceCreationMode(); got != "anyone" {
		t.Fatalf("from the environment: %q", got)
	}
	got := load(t, []string{"-workspace-creation", "admins"}, map[string]string{"TASKTO_CONFIG": file, "TASKTO_WORKSPACE_CREATION": "anyone"})
	if got.WorkspaceCreationMode() != "admins" {
		t.Fatalf("from a flag: %q", got.WorkspaceCreationMode())
	}
	for _, bad := range []string{"everyone", "Admins", "true"} {
		if _, err := Load(nil, env(map[string]string{"TASKTO_WORKSPACE_CREATION": bad}), io.Discard); err == nil ||
			!strings.Contains(err.Error(), "must be admins, anyone or off") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestAppDeletedRetention(t *testing.T) {
	if got := load(t, nil, nil).AppDeletedRetention(); got != 90*24*time.Hour {
		t.Fatalf("by default %v, want 90 days", got)
	}
	file := writeConfig(t, `app_deleted_retention = "30"`)
	if got := load(t, []string{"-config", file}, nil).AppDeletedRetention(); got != 30*24*time.Hour {
		t.Fatalf("from the file: %v", got)
	}
	env := map[string]string{"TASKTO_APP_DELETED_RETENTION": "14"}
	if got := load(t, []string{"-config", file}, env).AppDeletedRetention(); got != 14*24*time.Hour {
		t.Fatalf("env over file: %v", got)
	}
	if got := load(t, []string{"-config", file, "-app-deleted-retention", "7"}, env).AppDeletedRetention(); got != 7*24*time.Hour {
		t.Fatalf("flag over env: %v", got)
	}
	if got := load(t, []string{"-app-deleted-retention", "off"}, nil).AppDeletedRetention(); got != 0 {
		t.Fatalf("off: %v, want 0 (for ever)", got)
	}
}

func TestAppDeletedRetentionMustBeDaysOrOff(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc", "1.5", "40000", "1y", ""} {
		if _, err := Load([]string{"-app-deleted-retention", bad}, env(nil), io.Discard); err == nil {
			t.Errorf("-app-deleted-retention %q accepted", bad)
		}
	}
}
