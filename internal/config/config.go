// Package config reads the server's settings from flags, TASKTO_* environment variables
// and an optional config file.
//
// A setting given in more than one place takes the first of: flag, environment variable,
// config file, default.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	netmail "net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/mail"
)

// Config is the server's configuration.
type Config struct {
	// Addr is the address the HTTP server listens on.
	Addr string
	// DataDir holds everything the server stores: the SQLite database, and later the
	// knowledge-base repositories and the agent runtime's state.
	DataDir string
	// UpdateStatusDir is the directory where the host's updater (deploy/taskto-update.sh) writes
	// the status it shows admins (docs/install.md#automatic-updates); the server only reads it.
	// Empty means no updater is set up for this server.
	UpdateStatusDir string
	// InsecureCookies drops the Secure attribute from the session cookie, so a browser
	// sends it over plain http. For local development only: on any other network the
	// cookie would travel in clear text.
	InsecureCookies bool
	// TrustedProxies lists the reverse proxies, as comma-separated IP addresses or CIDR
	// prefixes, whose X-Forwarded-For gives the client's address (see TrustedProxyPrefixes).
	// Empty means none: the client is whoever connects.
	TrustedProxies string
	// SSOProviders is the path of the operator's single sign-on providers file
	// (docs/sso.md); empty means password logins only.
	SSOProviders string
	// PublicURL is the server's external origin, https://taskto.example.com: the base of the
	// providers' redirect URIs. Required with SSOProviders (see ValidatePublicURL).
	PublicURL string
	// DisablePasswordLogin turns passwords off, for instances where everyone signs in with a
	// provider. Needs SSOProviders.
	DisablePasswordLogin bool
	// WorkspaceCreation says who may create a workspace besides first-run setup: "admins"
	// (the default; an admin of any existing workspace), "anyone" (every signed-in person) or
	// "off" (nobody: the one workspace setup made, and no others). See WorkspaceCreationMode.
	WorkspaceCreation string
	// ProviderEgressWorkspace and ProviderEgressPersonal are the egress policies of the
	// provider accounts workspaces' admins add and those any member adds for themselves: which
	// addresses the server, and the agent runtime, may connect to when they call the
	// accounts' endpoints. "any" (the default), "public", or a comma-separated allowlist of
	// hosts, IP addresses and CIDR prefixes, which may include "public" (see internal/egress).
	ProviderEgressWorkspace string
	ProviderEgressPersonal  string

	// Agent runs the agent runtime, a Node sidecar the server starts, supervises and stops.
	// Its database and socket live under DataDir.
	Agent bool
	// AgentNode is the Node.js executable that runs the sidecar, looked up in PATH unless
	// it is a path.
	AgentNode string
	// AgentEntry is the sidecar's entry point. Empty means agent/dist/index.js, looked for
	// next to the binary, one directory above it (bin/ in a checkout) and in the working
	// directory.
	AgentEntry string
	// AgentProviders is the operator's model providers file (docs/agent-providers.md): its
	// providers are offered to everyone, before the accounts workspaces and users add. Empty
	// means the scripted offline model in their place.
	AgentProviders string

	// CLIWorkers runs conversations on the official coding-agent CLIs, signed in with each
	// user's own subscription (docs/cli-workers.md): "off" or "" (the default); "local", the CLIs
	// running as the server's own OS user, allowed only while the instance has one user,
	// since there is no OS boundary between users then; or "local-unsafe", the same without
	// that check, for development and tests; or "container", a worker of its own
	// (`taskto cli-worker`, CLIWorkerSocket) that runs each user's CLIs as a Unix user of
	// their own.
	CLIWorkers string
	// CLIWorkerSocket is the container worker's socket (with CLIWorkers "container").
	CLIWorkerSocket string
	// UnsafePrivilegedAgents lets the agent sidecar and local CLI workers start although this
	// server runs as root or has a passwordless sudo, with a warning (docs/agent-safety.md).
	// For development only; the install never sets it.
	UnsafePrivilegedAgents bool
	// CLIProxyListen is where the per-turn proxy listens: workspaces' shared CLI accounts, and
	// TaskTo's tools for CLI turns
	// (docs/account-switching.md): a listener of its own, never the public one. A TCP address,
	// or in container mode "unix:" and a socket's absolute path, which the worker relays to
	// the CLIs (the Compose file's, since the worker's network can't reach the server). Empty
	// is 127.0.0.1 on a free port in the local modes, and no proxy in container mode.
	CLIProxyListen string
	// CLIProxyURL is the proxy's address as the CLIs reach it. Empty is the listener's own
	// address in the local modes.
	CLIProxyURL string
	// Browser gives the agent a browser per user, with a live view and hand-over
	// (docs/agent-browser.md). It runs in the CLI worker, so it needs CLIWorkers.
	Browser bool
	// BrowserChromium is the Chromium (or Chrome) executable in local mode; empty looks for
	// chromium on the PATH. The container worker uses its image's.
	BrowserChromium string

	// KBMaxFileSize is the largest file of any kind in the knowledge base, in bytes.
	// Zero means the knowledge base's default.
	KBMaxFileSize int64
	// KBLargeFileSize is the size over which a file other than Markdown is kept out of
	// git, unversioned, in bytes; audio and video always are. It is also the largest
	// Markdown document. Zero means the knowledge base's default.
	KBLargeFileSize int64
	// KBMaxRepoSize caps each space's knowledge-base repository, history included, in
	// bytes. Zero means the knowledge base's default.
	KBMaxRepoSize int64
	// KBMaxUnversionedSize caps the bytes of each space's unversioned files, in bytes.
	// Zero means the knowledge base's default.
	KBMaxUnversionedSize int64
	// StorageQuotaWorkspace caps what all of a workspace's spaces store together, in bytes
	// (docs/quotas.md). Zero, the default, is unlimited.
	StorageQuotaWorkspace int64
	// StorageQuotaUser caps what each member's private space stores, in bytes. Zero, the
	// default, is unlimited.
	StorageQuotaUser int64
	// SearchVectorCache is the most memory semantic search's vector cache may hold, in
	// bytes. Zero means the search index's default; negative, "off", turns the cache off.
	SearchVectorCache int64

	// DictationLocal is the local speech model dictation uses when a workspace has no API
	// key (docs/dictation.md): "parakeet-v3" (the default), "whistle", or "off". Its files are
	// downloaded on first use.
	DictationLocal string
	// DictationCache holds the local speech model's files. Empty means <data>/cache/dictation.
	DictationCache string
	// AddonsDir is where the operator installs add-ons, such as Desert Ant Voz. Empty means
	// <data>/addons.
	AddonsDir string
	// PushContact is how the push services can reach whoever runs this server (the VAPID subject
	// of RFC 8292): a mailto: or https: URL. Empty means the project's address.
	PushContact string

	// SMTPHost, SMTPPort, SMTPSecurity, SMTPFrom, SMTPUsername and SMTPPasswordFile are the
	// operator's outgoing email (docs/install.md#email): the server sends invitations, password
	// resets and notifications through this SMTP server. SMTPHost empty means no email.
	// SMTPSecurity is "starttls" (the default), "tls" (implicit TLS) or "none"; SMTPPort 0 is
	// the security's usual port (587, 465, 25); SMTPFrom is the sender, an address or "Name
	// <address>". SMTPPasswordFile is the file the password is read from at startup, never the
	// password itself. SMTPEgress is the egress policy of the connection (see internal/egress).
	SMTPHost         string
	SMTPPort         int
	SMTPSecurity     string
	SMTPFrom         string
	SMTPUsername     string
	SMTPPasswordFile string
	SMTPEgress       string

	// IntegrationEgress is the egress policy of the calls to integrations' providers (Stripe,
	// Square; see internal/egress and docs/integrations.md). Empty means "public": addresses are
	// checked after name resolution on every connection, so loopback, private, link-local and
	// metadata addresses are refused. Each adapter pins its own hosts besides, so the policy
	// never lists provider names.
	IntegrationEgress string

	// AuditRetentionDays is how many days the audit log's events are kept (docs/audit.md). Zero
	// means the default, 365; -1, "off" in the settings, keeps them for ever.
	AuditRetentionDays int

	// ConfigFile is the config file that was read, or "" if none was.
	ConfigFile string
	// ShowVersion asks the binary to print its version and exit (flag only).
	ShowVersion bool
}

// Default returns the configuration used when nothing is set.
func Default() Config {
	return Config{Addr: "127.0.0.1:8080", DataDir: "data", AgentNode: "node"}
}

// setting is one configuration value and the names it goes by in each source.
type setting struct {
	flag, env, key string
	usage          string
	isBool         bool
	get            func(Config) string
	set            func(*Config, string) error
}

var settings = []setting{
	{
		flag: "addr", env: "TASKTO_ADDR", key: "addr",
		usage: "address to listen on",
		get:   func(c Config) string { return c.Addr },
		set:   func(c *Config, v string) error { c.Addr = v; return nil },
	},
	{
		flag: "data-dir", env: "TASKTO_DATA_DIR", key: "data_dir",
		usage: "directory for the database and other stored data",
		get:   func(c Config) string { return c.DataDir },
		set:   func(c *Config, v string) error { c.DataDir = v; return nil },
	},
	{
		flag: "update-status-dir", env: "TASKTO_UPDATE_STATUS_DIR", key: "update_status_dir",
		usage: "the directory where the host's updater writes its status, which admins see in Settings > About (read-only; an absolute path)",
		get:   func(c Config) string { return c.UpdateStatusDir },
		set: func(c *Config, v string) error {
			if v != "" && !filepath.IsAbs(v) {
				return errors.New("want an absolute path")
			}
			c.UpdateStatusDir = v
			return nil
		},
	},
	{
		flag: "insecure-cookies", env: "TASKTO_INSECURE_COOKIES", key: "insecure_cookies", isBool: true,
		usage: "send the session cookie over plain http (local development only)",
		get:   func(c Config) string { return strconv.FormatBool(c.InsecureCookies) },
		set:   setBool(func(c *Config) *bool { return &c.InsecureCookies }),
	},
	{
		flag: "trusted-proxies", env: "TASKTO_TRUSTED_PROXIES", key: "trusted_proxies",
		usage: "reverse proxies whose X-Forwarded-For is believed: comma-separated IP addresses or CIDR prefixes",
		get:   func(c Config) string { return c.TrustedProxies },
		set: func(c *Config, v string) error {
			if _, err := ParseTrustedProxies(v); err != nil {
				return err
			}
			c.TrustedProxies = v
			return nil
		},
	},
	{
		flag: "sso-providers", env: "TASKTO_SSO_PROVIDERS", key: "sso_providers",
		usage: "the single sign-on providers file, JSON (docs/sso.md); needs -public-url",
		get:   func(c Config) string { return c.SSOProviders },
		set:   func(c *Config, v string) error { c.SSOProviders = v; return nil },
	},
	{
		flag: "public-url", env: "TASKTO_PUBLIC_URL", key: "public_url",
		usage: "the server's external https origin, such as https://taskto.example.com, for single sign-on's redirect URIs",
		get:   func(c Config) string { return c.PublicURL },
		set:   func(c *Config, v string) error { c.PublicURL = v; return nil },
	},
	{
		flag: "disable-password-login", env: "TASKTO_DISABLE_PASSWORD_LOGIN", key: "disable_password_login", isBool: true,
		usage: "turn password logins off, so everyone signs in with a single sign-on provider (needs -sso-providers)",
		get:   func(c Config) string { return strconv.FormatBool(c.DisablePasswordLogin) },
		set:   setBool(func(c *Config) *bool { return &c.DisablePasswordLogin }),
	},
	{
		flag: "workspace-creation", env: "TASKTO_WORKSPACE_CREATION", key: "workspace_creation",
		usage: "who may create a workspace: admins (default; an admin of any workspace), anyone (everyone signed in) or off",
		get:   func(c Config) string { return c.WorkspaceCreationMode() },
		set: func(c *Config, v string) error {
			switch v {
			case "admins", "anyone", "off":
				c.WorkspaceCreation = v
				return nil
			}
			return errors.New("must be admins, anyone or off")
		},
	},
	{
		flag: "provider-egress-workspace", env: "TASKTO_PROVIDER_EGRESS_WORKSPACE", key: "provider_egress_workspace",
		usage: "where workspaces' provider accounts may connect: any, public, or a comma-separated list of public, hosts and CIDR prefixes (default any)",
		get:   func(c Config) string { return c.ProviderEgressWorkspace },
		set:   setEgress(func(c *Config) *string { return &c.ProviderEgressWorkspace }),
	},
	{
		flag: "provider-egress-personal", env: "TASKTO_PROVIDER_EGRESS_PERSONAL", key: "provider_egress_personal",
		usage: "where members' personal provider accounts may connect: any, public, or a comma-separated list of public, hosts and CIDR prefixes (default any)",
		get:   func(c Config) string { return c.ProviderEgressPersonal },
		set:   setEgress(func(c *Config) *string { return &c.ProviderEgressPersonal }),
	},
	{
		flag: "smtp-host", env: "TASKTO_SMTP_HOST", key: "smtp_host",
		usage: "the SMTP server that sends the server's email (invitations, password resets, notifications); empty means no email",
		get:   func(c Config) string { return c.SMTPHost },
		set: func(c *Config, v string) error {
			host, err := mail.NormalizeHost(v)
			if err != nil {
				return errors.New("want a host name or IP address, with the port in -smtp-port")
			}
			c.SMTPHost = host
			return nil
		},
	},
	{
		flag: "smtp-port", env: "TASKTO_SMTP_PORT", key: "smtp_port",
		usage: "the SMTP server's port (default 587 with starttls, 465 with tls, 25 with none)",
		get: func(c Config) string {
			if c.SMTPPort == 0 {
				return ""
			}
			return strconv.Itoa(c.SMTPPort)
		},
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("want a port from 1 to 65535")
			}
			c.SMTPPort = n
			return nil
		},
	},
	{
		flag: "smtp-security", env: "TASKTO_SMTP_SECURITY", key: "smtp_security",
		usage: "how the SMTP connection is secured: starttls (default), tls (implicit TLS) or none (a trusted local relay)",
		get:   func(c Config) string { return c.SMTPSecurityMode() },
		set: func(c *Config, v string) error {
			switch v {
			case "starttls", "tls", "none":
				c.SMTPSecurity = v
				return nil
			}
			return errors.New("must be starttls, tls or none")
		},
	},
	{
		flag: "smtp-from", env: "TASKTO_SMTP_FROM", key: "smtp_from",
		usage: "the sender of the server's email: an address, or Name <address>; needed with -smtp-host",
		get:   func(c Config) string { return c.SMTPFrom },
		set: func(c *Config, v string) error {
			a, err := netmail.ParseAddress(v)
			if err != nil {
				return errors.New("want an email address, or Name <address>")
			}
			c.SMTPFrom = a.String()
			return nil
		},
	},
	{
		flag: "smtp-username", env: "TASKTO_SMTP_USERNAME", key: "smtp_username",
		usage: "the user to sign in to the SMTP server as; needs -smtp-password-file",
		get:   func(c Config) string { return c.SMTPUsername },
		set:   func(c *Config, v string) error { c.SMTPUsername = v; return nil },
	},
	{
		flag: "smtp-password-file", env: "TASKTO_SMTP_PASSWORD_FILE", key: "smtp_password_file",
		usage: "the file the SMTP password is read from at startup (an absolute path); the password itself is never a setting",
		get:   func(c Config) string { return c.SMTPPasswordFile },
		set: func(c *Config, v string) error {
			if !filepath.IsAbs(v) {
				return errors.New("want an absolute path")
			}
			c.SMTPPasswordFile = v
			return nil
		},
	},
	{
		flag: "smtp-egress", env: "TASKTO_SMTP_EGRESS", key: "smtp_egress",
		usage: "where the SMTP connection may go: any, public, or a comma-separated list of public, hosts and CIDR prefixes (default any)",
		get:   func(c Config) string { return c.SMTPEgress },
		set:   setEgress(func(c *Config) *string { return &c.SMTPEgress }),
	},
	{
		flag: "integration-egress", env: "TASKTO_INTEGRATION_EGRESS", key: "integration_egress",
		usage: "where calls to integrations' providers may go: public, any, or a comma-separated list of public, hosts and CIDR prefixes (default public)",
		get:   func(c Config) string { return c.IntegrationEgress },
		set:   setEgress(func(c *Config) *string { return &c.IntegrationEgress }),
	},
	{
		flag: "agent", env: "TASKTO_AGENT", key: "agent", isBool: true,
		usage: "run the agent runtime (a Node.js sidecar)",
		get:   func(c Config) string { return strconv.FormatBool(c.Agent) },
		set:   setBool(func(c *Config) *bool { return &c.Agent }),
	},
	{
		flag: "agent-node", env: "TASKTO_AGENT_NODE", key: "agent_node",
		usage: "Node.js executable for the agent runtime",
		get:   func(c Config) string { return c.AgentNode },
		set:   func(c *Config, v string) error { c.AgentNode = v; return nil },
	},
	{
		flag: "agent-entry", env: "TASKTO_AGENT_ENTRY", key: "agent_entry",
		usage: "the agent runtime's entry point (default: agent/dist/index.js next to the binary or in the working directory)",
		get:   func(c Config) string { return c.AgentEntry },
		set:   func(c *Config, v string) error { c.AgentEntry = v; return nil },
	},
	{
		flag: "agent-providers", env: "TASKTO_AGENT_PROVIDERS", key: "agent_providers",
		usage: "the operator's model providers file for the agent runtime, offered to everyone (default: the scripted offline model)",
		get:   func(c Config) string { return c.AgentProviders },
		set:   func(c *Config, v string) error { c.AgentProviders = v; return nil },
	},
	{
		flag: "cli-workers", env: "TASKTO_CLI_WORKERS", key: "cli_workers",
		usage: "run conversations on the users' own Claude Code, Codex and Grok Build logins: off, container (a worker of its own, see -cli-worker-socket), local (one-user instances only) or local-unsafe (development)",
		get: func(c Config) string {
			if c.CLIWorkers == "" {
				return "off"
			}
			return c.CLIWorkers
		},
		set: func(c *Config, v string) error {
			switch v {
			case "off", "local", "local-unsafe", "container":
				c.CLIWorkers = v
				return nil
			}
			return fmt.Errorf("must be off, container, local or local-unsafe")
		},
	},
	{
		flag: "unsafe-privileged-agents", env: "TASKTO_UNSAFE_PRIVILEGED_AGENTS", key: "unsafe_privileged_agents", isBool: true,
		usage: "development only: start the agent runtime and local CLI workers even when this server runs as root or has a passwordless sudo",
		get:   func(c Config) string { return strconv.FormatBool(c.UnsafePrivilegedAgents) },
		set:   setBool(func(c *Config) *bool { return &c.UnsafePrivilegedAgents }),
	},
	{
		flag: "cli-worker-socket", env: "TASKTO_CLI_WORKER_SOCKET", key: "cli_worker_socket",
		usage: "the CLI worker's socket, with -cli-workers container",
		get:   func(c Config) string { return c.CLIWorkerSocket },
		set:   func(c *Config, v string) error { c.CLIWorkerSocket = v; return nil },
	},
	{
		flag: "cli-proxy-listen", env: "TASKTO_CLI_PROXY_LISTEN", key: "cli_proxy_listen",
		usage: "where the per-turn proxy listens (workspaces' shared CLI accounts, and TaskTo's tools for CLI turns), apart from the public address: host:port, or unix:/path for the CLI worker to relay (default: 127.0.0.1 on a free port with -cli-workers local or local-unsafe; none with container)",
		get:   func(c Config) string { return c.CLIProxyListen },
		set:   func(c *Config, v string) error { c.CLIProxyListen = v; return nil },
	},
	{
		flag: "cli-proxy-url", env: "TASKTO_CLI_PROXY_URL", key: "cli_proxy_url",
		usage: "the per-turn proxy as the CLIs reach it, such as http://127.0.0.1:8081 (default: its listen address with -cli-workers local)",
		get:   func(c Config) string { return c.CLIProxyURL },
		set:   func(c *Config, v string) error { c.CLIProxyURL = v; return nil },
	},
	{
		flag: "browser", env: "TASKTO_BROWSER", key: "browser", isBool: true,
		usage: "give the agent a browser per user, with a live view and hand-over for logins (needs -cli-workers)",
		get:   func(c Config) string { return strconv.FormatBool(c.Browser) },
		set:   setBool(func(c *Config) *bool { return &c.Browser }),
	},
	{
		flag: "browser-chromium", env: "TASKTO_BROWSER_CHROMIUM", key: "browser_chromium",
		usage: "the agent browser's Chromium in local mode (default: chromium on the PATH)",
		get:   func(c Config) string { return c.BrowserChromium },
		set:   func(c *Config, v string) error { c.BrowserChromium = v; return nil },
	},
	{
		flag: "kb-max-file-size", env: "TASKTO_KB_MAX_FILE_SIZE", key: "kb_max_file_size",
		usage: "largest file of any kind in the knowledge base, in bytes or with KiB, MiB or GiB (default 2GiB)",
		get:   func(c Config) string { return formatSize(c.KBMaxFileSize) },
		set:   setSize(func(c *Config) *int64 { return &c.KBMaxFileSize }),
	},
	{
		flag: "kb-large-file-size", env: "TASKTO_KB_LARGE_FILE_SIZE", key: "kb_large_file_size",
		usage: "size over which a knowledge-base file other than Markdown is kept out of git, unversioned (audio and video always are), and the largest Markdown document, in bytes or with KiB, MiB or GiB (default 50MiB)",
		get:   func(c Config) string { return formatSize(c.KBLargeFileSize) },
		set:   setSize(func(c *Config) *int64 { return &c.KBLargeFileSize }),
	},
	{
		flag: "kb-max-repo-size", env: "TASKTO_KB_MAX_REPO_SIZE", key: "kb_max_repo_size",
		usage: "cap on each space's knowledge-base repository, history included, in bytes or with KiB, MiB or GiB (default 1GiB)",
		get:   func(c Config) string { return formatSize(c.KBMaxRepoSize) },
		set:   setSize(func(c *Config) *int64 { return &c.KBMaxRepoSize }),
	},
	{
		flag: "kb-max-unversioned-size", env: "TASKTO_KB_MAX_UNVERSIONED_SIZE", key: "kb_max_unversioned_size",
		usage: "cap on the bytes of each space's unversioned knowledge-base files (large files, audio and video), in bytes or with KiB, MiB or GiB (default 20GiB)",
		get:   func(c Config) string { return formatSize(c.KBMaxUnversionedSize) },
		set:   setSize(func(c *Config) *int64 { return &c.KBMaxUnversionedSize }),
	},
	{
		flag: "storage-quota-workspace", env: "TASKTO_STORAGE_QUOTA_WORKSPACE", key: "storage_quota_workspace",
		usage: "most each workspace may store in all its spaces (knowledge base and app photos), in bytes or with KiB, MiB or GiB (default: unlimited)",
		get:   func(c Config) string { return formatSize(c.StorageQuotaWorkspace) },
		set:   setSize(func(c *Config) *int64 { return &c.StorageQuotaWorkspace }),
	},
	{
		flag: "storage-quota-user", env: "TASKTO_STORAGE_QUOTA_USER", key: "storage_quota_user",
		usage: "most each member's private space may store (knowledge base and app photos), in bytes or with KiB, MiB or GiB (default: unlimited)",
		get:   func(c Config) string { return formatSize(c.StorageQuotaUser) },
		set:   setSize(func(c *Config) *int64 { return &c.StorageQuotaUser }),
	},
	{
		flag: "search-vector-cache", env: "TASKTO_SEARCH_VECTOR_CACHE", key: "search_vector_cache",
		usage: "most memory semantic search keeps vectors in, in bytes or with KiB, MiB or GiB, or off (default 1GiB)",
		get: func(c Config) string {
			if c.SearchVectorCache < 0 {
				return "off"
			}
			return formatSize(c.SearchVectorCache)
		},
		set: func(c *Config, v string) error {
			if v == "off" {
				c.SearchVectorCache = -1
				return nil
			}
			return setSize(func(c *Config) *int64 { return &c.SearchVectorCache })(c, v)
		},
	},
	{
		flag: "dictation-local", env: "TASKTO_DICTATION_LOCAL", key: "dictation_local",
		usage: "the local speech model for dictation, downloaded on first use: parakeet-v3, whistle or off (default parakeet-v3)",
		get:   func(c Config) string { return c.LocalDictation() },
		set: func(c *Config, v string) error {
			switch v {
			case "parakeet-v3", "whistle", "off":
				c.DictationLocal = v
				return nil
			}
			return fmt.Errorf("must be parakeet-v3, whistle or off")
		},
	},
	{
		flag: "dictation-cache", env: "TASKTO_DICTATION_CACHE", key: "dictation_cache",
		usage: "where the local speech model's files are kept (default <data-dir>/cache/dictation)",
		get:   func(c Config) string { return c.DictationCache },
		set:   func(c *Config, v string) error { c.DictationCache = v; return nil },
	},
	{
		flag: "addons-dir", env: "TASKTO_ADDONS_DIR", key: "addons_dir",
		usage: "where the operator installs add-ons such as Desert Ant Voz (default <data-dir>/addons)",
		get:   func(c Config) string { return c.AddonsDir },
		set:   func(c *Config, v string) error { c.AddonsDir = v; return nil },
	},
	{
		flag: "audit-retention", env: "TASKTO_AUDIT_RETENTION", key: "audit_retention",
		usage: "days the audit log's events are kept before they are removed, or off to keep them for ever (default 365)",
		get: func(c Config) string {
			switch {
			case c.AuditRetentionDays < 0:
				return "off"
			case c.AuditRetentionDays == 0:
				return ""
			}
			return strconv.Itoa(c.AuditRetentionDays)
		},
		set: func(c *Config, v string) error {
			if v == "off" {
				c.AuditRetentionDays = -1
				return nil
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 36500 {
				return fmt.Errorf("want a number of days from 1 to 36500, or off, got %q", v)
			}
			c.AuditRetentionDays = n
			return nil
		},
	},
	{
		flag: "push-contact", env: "TASKTO_PUSH_CONTACT", key: "push_contact",
		usage: "how browsers' push services can reach you, a mailto: or https: URL (default https://taskto.ai)",
		get:   func(c Config) string { return c.PushContact },
		set: func(c *Config, v string) error {
			if err := ValidatePushContact(v); err != nil {
				return err
			}
			c.PushContact = v
			return nil
		},
	},
}

// WorkspaceCreationMode is who may create a workspace: "admins" when unset.
func (c Config) WorkspaceCreationMode() string {
	if c.WorkspaceCreation == "" {
		return "admins"
	}
	return c.WorkspaceCreation
}

// SMTPSecurityMode is how the SMTP connection is secured, "starttls" when unset.
func (c Config) SMTPSecurityMode() string {
	if c.SMTPSecurity == "" {
		return "starttls"
	}
	return c.SMTPSecurity
}

// SMTPPortNumber is the SMTP server's port: the one set, else the security's usual one.
func (c Config) SMTPPortNumber() int {
	if c.SMTPPort != 0 {
		return c.SMTPPort
	}
	switch c.SMTPSecurityMode() {
	case "tls":
		return 465
	case "none":
		return 25
	}
	return 587
}

// SMTPEgressPolicy is the egress policy of the SMTP connection. Load has already checked it.
func (c Config) SMTPEgressPolicy() egress.Policy {
	p, _ := egress.Parse(c.SMTPEgress)
	return p
}

// IntegrationEgressPolicy is the egress policy of the calls to integrations' providers: what is
// set, or "public" when nothing is. Load has already checked it.
func (c Config) IntegrationEgressPolicy() egress.Policy {
	if c.IntegrationEgress == "" {
		return egress.MustParse("public")
	}
	p, _ := egress.Parse(c.IntegrationEgress)
	return p
}

// AuditRetention is how long the audit log's events are kept: a year when unset, and zero, for
// ever, when turned off.
func (c Config) AuditRetention() time.Duration {
	switch {
	case c.AuditRetentionDays < 0:
		return 0
	case c.AuditRetentionDays == 0:
		return audit.DefaultRetention
	}
	return time.Duration(c.AuditRetentionDays) * 24 * time.Hour
}

// LocalDictation is the local speech model, "off" included: parakeet-v3 when unset.
func (c Config) LocalDictation() string {
	if c.DictationLocal == "" {
		return "parakeet-v3"
	}
	return c.DictationLocal
}

// DictationCacheDir is where the local speech model's files are kept.
func (c Config) DictationCacheDir() string {
	if c.DictationCache != "" {
		return c.DictationCache
	}
	return filepath.Join(c.DataDir, "cache", "dictation")
}

// AddonsDirectory is where the operator installs add-ons.
func (c Config) AddonsDirectory() string {
	if c.AddonsDir != "" {
		return c.AddonsDir
	}
	return filepath.Join(c.DataDir, "addons")
}

// TrustedProxyPrefixes returns TrustedProxies parsed. Load has already checked it.
func (c Config) TrustedProxyPrefixes() []netip.Prefix {
	prefixes, _ := ParseTrustedProxies(c.TrustedProxies)
	return prefixes
}

// ParseTrustedProxies parses a comma-separated list of IP addresses and CIDR prefixes,
// such as "10.0.0.0/8, 192.0.2.7". An address is a prefix of one address.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("trusted proxies: %q isn't a CIDR prefix", item)
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil || addr.Zone() != "" {
			return nil, fmt.Errorf("trusted proxies: %q isn't an IP address", item)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

// Egress returns the egress policies of workspaces' and personal provider accounts. Load has
// already checked them.
func (c Config) Egress() (workspace, personal egress.Policy) {
	workspace, _ = egress.Parse(c.ProviderEgressWorkspace)
	personal, _ = egress.Parse(c.ProviderEgressPersonal)
	return workspace, personal
}

// setEgress returns a setter for the egress policy field that field points to.
func setEgress(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		p, err := egress.Parse(v)
		if err != nil {
			return fmt.Errorf("egress policy: %w", err)
		}
		*field(c) = p.String()
		return nil
	}
}

// setBool returns a setter for the boolean field that field points to.
func setBool(field func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("want true or false, got %q", v)
		}
		*field(c) = b
		return nil
	}
}

// sizeUnits are the suffixes a size may have, largest first.
var sizeUnits = []struct {
	suffix string
	bytes  int64
}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}

// setSize returns a setter for the size field that field points to: a positive number of
// bytes, or of KiB, MiB or GiB ("10MiB").
func setSize(field func(*Config) *int64) func(*Config, string) error {
	return func(c *Config, v string) error {
		num, unit := v, int64(1)
		for _, u := range sizeUnits {
			if n, ok := strings.CutSuffix(v, u.suffix); ok {
				num, unit = strings.TrimSpace(n), u.bytes
				break
			}
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n < 1 || n > math.MaxInt64/unit {
			return fmt.Errorf("want a size like 1048576 or 10MiB, got %q", v)
		}
		*field(c) = n * unit
		return nil
	}
}

// formatSize writes n with the largest unit that divides it, and 0 (the default) as "".
func formatSize(n int64) string {
	if n == 0 {
		return ""
	}
	for _, u := range sizeUnits {
		if n%u.bytes == 0 {
			return strconv.FormatInt(n/u.bytes, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// rawFlag records a flag's text so it can be applied after the lower layers.
type rawFlag struct {
	value  string
	isBool bool
}

func (f *rawFlag) String() string     { return f.value }
func (f *rawFlag) Set(v string) error { f.value = v; return nil }
func (f *rawFlag) IsBoolFlag() bool   { return f.isBool }

// Load builds the configuration from command-line args (without the program name),
// the environment as seen through getenv, and the config file named by -config or
// TASKTO_CONFIG. Flag errors, including flag.ErrHelp for -h, are returned as is, after
// the flag package has written its message to output.
func Load(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("taskto", flag.ContinueOnError)
	fs.SetOutput(output)
	configFile := fs.String("config", "", "config file to read (env TASKTO_CONFIG)")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print the version and exit")
	flags := make([]*rawFlag, len(settings))
	for i, s := range settings {
		flags[i] = &rawFlag{value: s.get(cfg), isBool: s.isBool}
		fs.Var(flags[i], s.flag, fmt.Sprintf("%s (env %s)", s.usage, s.env))
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	cfg.ConfigFile = *configFile
	if cfg.ConfigFile == "" {
		cfg.ConfigFile = getenv("TASKTO_CONFIG")
	}
	if cfg.ConfigFile != "" {
		if err := readFile(&cfg, cfg.ConfigFile); err != nil {
			return Config{}, err
		}
	}

	for _, s := range settings {
		// An empty environment variable counts as unset.
		if v := getenv(s.env); v != "" {
			if err := s.set(&cfg, v); err != nil {
				return Config{}, fmt.Errorf("%s: %w", s.env, err)
			}
		}
	}

	for i, s := range settings {
		if given[s.flag] {
			if err := s.set(&cfg, flags[i].value); err != nil {
				return Config{}, fmt.Errorf("-%s: %w", s.flag, err)
			}
		}
	}

	if cfg.Addr == "" {
		return Config{}, errors.New("config: the listen address is empty")
	}
	if cfg.DataDir == "" {
		return Config{}, errors.New("config: the data directory is empty")
	}
	if cfg.CLIWorkers == "container" && cfg.CLIWorkerSocket == "" {
		return Config{}, errors.New("config: -cli-workers container needs -cli-worker-socket")
	}
	if cfg.CLIProxyURL != "" {
		u, err := url.Parse(cfg.CLIProxyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return Config{}, errors.New("config: -cli-proxy-url must be an http or https address without a path, such as http://taskto:8081")
		}
	}
	if cfg.CLIProxyURL != "" && cfg.CLIProxyListen == "" {
		return Config{}, errors.New("config: -cli-proxy-url needs -cli-proxy-listen, where the proxy listens")
	}
	if path, ok := strings.CutPrefix(cfg.CLIProxyListen, "unix:"); ok {
		switch {
		case cfg.CLIWorkers != "container":
			return Config{}, errors.New("config: -cli-proxy-listen unix: is for -cli-workers container, whose worker relays the socket to the CLIs")
		case !filepath.IsAbs(path) || filepath.Clean(path) != path:
			return Config{}, errors.New("config: -cli-proxy-listen unix: needs an absolute path, such as unix:/run/taskto-cli/proxy/proxy.sock")
		case cfg.CLIProxyURL == "":
			return Config{}, errors.New("config: -cli-proxy-listen unix: needs -cli-proxy-url, the worker's relay as the CLIs reach it")
		}
	}
	if cfg.Browser && cfg.CLIWorkers == "" {
		return Config{}, errors.New("config: -browser needs -cli-workers: the browser runs in the CLI worker")
	}
	if cfg.SSOProviders != "" {
		if cfg.PublicURL == "" {
			return Config{}, errors.New("config: -sso-providers needs -public-url, the server's external address")
		}
	}
	if cfg.PublicURL != "" {
		origin, err := ValidatePublicURL(cfg.PublicURL, cfg.InsecureCookies)
		if err != nil {
			return Config{}, fmt.Errorf("config: -public-url %w", err)
		}
		cfg.PublicURL = origin
	}
	if cfg.DisablePasswordLogin && cfg.SSOProviders == "" {
		return Config{}, errors.New("config: -disable-password-login needs -sso-providers: nobody could log in")
	}
	if err := validateSMTP(cfg); err != nil {
		return Config{}, err
	}
	if cfg.Agent && cfg.AgentNode == "" {
		return Config{}, errors.New("config: the agent runtime's Node.js executable is empty")
	}
	return cfg, nil
}

// validateSMTP checks that the SMTP settings make sense together: a host needs a sender, a
// username needs a password and the other way round, and a password is never sent over a
// connection that isn't encrypted: with -smtp-security none there is no sign-in, to any host.
func validateSMTP(cfg Config) error {
	if cfg.SMTPHost == "" {
		if cfg.SMTPPort != 0 || cfg.SMTPSecurity != "" || cfg.SMTPFrom != "" || cfg.SMTPUsername != "" ||
			cfg.SMTPPasswordFile != "" || cfg.SMTPEgress != "" {
			return errors.New("config: the smtp settings need -smtp-host, the SMTP server")
		}
		return nil
	}
	if cfg.SMTPFrom == "" {
		return errors.New("config: -smtp-host needs -smtp-from, the sender of the email")
	}
	if (cfg.SMTPUsername == "") != (cfg.SMTPPasswordFile == "") {
		return errors.New("config: -smtp-username and -smtp-password-file go together")
	}
	if cfg.SMTPUsername != "" && cfg.SMTPSecurityMode() == "none" {
		return errors.New("config: -smtp-security none would send the password in clear text; use starttls or tls, or leave out -smtp-username")
	}
	return nil
}

func readFile(cfg *Config, path string) error {
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator chooses the config file.
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := parse(cfg, raw); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

// parse reads a config file. The format is a small subset of TOML, so a file stays
// valid TOML if a full parser is ever needed: one `key = value` per line, where a value
// is a double-quoted string or true/false, and `#` starts a comment. Tables, arrays and
// numbers are not supported, and unknown or repeated keys are errors.
func parse(cfg *Config, raw []byte) error {
	byKey := map[string]setting{}
	for _, s := range settings {
		byKey[s.key] = s
	}
	seen := map[string]bool{}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: want key = value", n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		s, known := byKey[key]
		if !known {
			return fmt.Errorf("line %d: unknown key %q", n, key)
		}
		if seen[key] {
			return fmt.Errorf("line %d: %s is set twice", n, key)
		}
		seen[key] = true

		switch {
		case s.isBool && (value == "true" || value == "false"):
		case !s.isBool && strings.HasPrefix(value, `"`):
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf("line %d: %s: bad string %s", n, key, value)
			}
			value = unquoted
		case s.isBool:
			return fmt.Errorf("line %d: %s: want true or false, got %s", n, key, value)
		default:
			return fmt.Errorf("line %d: %s: want a double-quoted string, got %s", n, key, value)
		}
		if err := s.set(cfg, value); err != nil {
			return fmt.Errorf("line %d: %s: %w", n, key, err)
		}
	}
	return sc.Err()
}

// stripComment removes a `#` comment that isn't inside a quoted string.
func stripComment(line string) string {
	inString, escaped := false, false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case inString && r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case r == '#' && !inString:
			return line[:i]
		}
	}
	return line
}

// ValidatePushContact checks a VAPID contact: a mailto: address or an https: URL with a host,
// which is all RFC 8292 allows a subject to be. "" is valid: the default.
func ValidatePushContact(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || len(v) > 256 || strings.ContainsAny(v, " \t\r\n") {
		return errors.New("must be a mailto: address or an https: URL")
	}
	switch u.Scheme {
	case "mailto":
		local, domain, ok := strings.Cut(u.Opaque, "@")
		if ok && local != "" && domain != "" && !strings.Contains(domain, "@") {
			return nil
		}
		return errors.New("a mailto: contact needs an address, mailto:you@example.org")
	case "https":
		if u.Hostname() != "" && u.User == nil {
			return nil
		}
		return errors.New("an https: contact needs a host and no credentials")
	}
	return errors.New("must be a mailto: address or an https: URL")
}

// ValidatePublicURL checks the server's external origin and returns it without a trailing
// slash. It must be https, with a host, and no path, query, fragment or credentials; http is
// accepted for localhost, 127.x and [::1] when insecureCookies is set, for development.
func ValidatePublicURL(v string, insecureCookies bool) (string, error) {
	u, err := url.Parse(v)
	bad := err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != ""
	if bad {
		return "", errors.New("must be an origin such as https://taskto.example.com, without a path, query or credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip, _ := netip.ParseAddr(host)
		if !insecureCookies || (host != "localhost" && !ip.IsLoopback()) {
			return "", errors.New("must be https; http is accepted only for localhost with -insecure-cookies")
		}
	default:
		return "", errors.New("must be https; http is accepted only for localhost with -insecure-cookies")
	}
	return u.Scheme + "://" + u.Host, nil
}
