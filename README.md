# TaskTo.AI

An open-source, self-hosted workspace for one person or a whole company:

- **A knowledge base**: Markdown documents and files in spaces, private or shared, each
  space a git repository with its history, and search (full text, and optional semantic
  search).
- **An agent that works with it**: a chat agent that reads and writes the knowledge base,
  works the boards and builds apps, on the model providers you choose, or on the official
  Claude Code, Codex and Grok CLIs signed in with each user's own account.
- **Boards** for everyone's tasks, personal and per team, that people and agents share.
- **Personal apps** you build by asking the agent: small tools with their own data,
  checked against a fixed component catalog.
- **Dictation**, on a local speech model by default, and **MCP** access for your own
  agents.
- **An app on your phone**: installable from the browser (Add to Home Screen), with an
  offline shell, on a server with HTTPS ([docs/pwa.md](docs/pwa.md)).
- **Single sign-on**: Google, Authentik or any OpenID Connect provider, for the people an
  admin invited, set up by the operator ([docs/sso.md](docs/sso.md)).
- **Languages**: English and Brazilian Portuguese so far, built to take more
  ([docs/i18n.md](docs/i18n.md)).

It is one Go binary with the web app inside, a Node.js sidecar for the agent runtime, and
SQLite. Isolation between users, workspaces and spaces is checked on every route, agents
never run as root, and no secret reaches an agent's environment
([docs/agent-safety.md](docs/agent-safety.md)).

**Status: early development.** It runs, and the install guide below sets up a server
behind HTTPS with backups, but expect breaking changes before a first release.

## Quick start

With Docker and Compose v2, from a checkout:

```sh
docker compose up -d                  # builds the image, listens on 127.0.0.1:8080
curl http://127.0.0.1:8080/healthz
docker compose logs taskto | grep setup_token
```

Open <http://localhost:8080> and create the first account with the setup token from the
log. To serve it over HTTPS, add CLI workers, local models and backups, or to run the
published images (`ghcr.io/taskto-ai/taskto`, no build needed), follow
[docs/install.md](docs/install.md). [deploy/INSTALL.md](deploy/INSTALL.md) is a
step-by-step install on a fresh Ubuntu VM.

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | how it fits together |
| [docs/install.md](docs/install.md) | install, HTTPS, backups, upgrades and the automatic updates |
| [docs/releasing.md](docs/releasing.md) | cutting a release, and what makes servers take it |
| [docs/pwa.md](docs/pwa.md) | the installable app: manifest, offline shell, service worker, updates, phones |
| [docs/templates.md](docs/templates.md) | the bundled app templates (expense tracker, equipment checkout, inspection), installing them and duplicating apps |
| [docs/web-e2e.md](docs/web-e2e.md) | the web app's end to end tests: what they cover, how to run them, and the traces a failure leaves |
| [docs/offline.md](docs/offline.md) | apps offline: downloads, the durable outbox for records made offline, sync, sign-out and revocation |
| [docs/sso.md](docs/sso.md) | single sign-on with Google, Authentik or any OpenID Connect provider |
| [docs/audit.md](docs/audit.md) | the audit log of security and admin actions: what is recorded, never recorded, and for how long |
| [docs/api.md](docs/api.md) | the HTTP API |
| [docs/mcp.md](docs/mcp.md) | using it from your own agents (Claude Code, Codex, any MCP client) with an access token |
| [docs/cli-workers.md](docs/cli-workers.md), [docs/account-switching.md](docs/account-switching.md), [docs/cli-tools.md](docs/cli-tools.md) | the official CLIs on users' own accounts, with TaskTo's tools |
| [docs/agent-safety.md](docs/agent-safety.md), [docs/agent-browser.md](docs/agent-browser.md) | what agents may do, and the agent's browser |
| [docs/dictation.md](docs/dictation.md), [docs/i18n.md](docs/i18n.md) | dictation, and languages |
| [docs/credits.md](docs/credits.md) | third-party models and their licences |

## Development

Needs Go (version in `go.mod`), Node 22.19+ and the pnpm version in `package.json`
(`packageManager`).

```sh
make install      # JavaScript dependencies
make check        # what CI runs: lint, tests, web + agent build
make fmt          # format Go and TypeScript
make dev-server   # Go server on 127.0.0.1:8080, with the agent runtime
make dev-web      # web app with hot reload, proxying the API to the Go server
make build        # bin/taskto, with the web app inside
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the rest: how to send a change, and how to add a
language. Report security issues privately, as [SECURITY.md](SECURITY.md) says.

## Running

`bin/taskto` is the whole server: the HTTP API, the web app and the database. It keeps
its data in one directory (`./data` by default), with the SQLite database in
`taskto.db`, and brings the database schema up to date when it starts.

Each setting can be given as a flag, an environment variable or a line in a config
file. A flag beats an environment variable, which beats the config file, which beats
the default.

| Flag | Environment | Config file | Default | |
|---|---|---|---|---|
| `-addr` | `TASKTO_ADDR` | `addr` | `127.0.0.1:8080` | address to listen on |
| `-data-dir` | `TASKTO_DATA_DIR` | `data_dir` | `data` | where the database and other data live |
| `-insecure-cookies` | `TASKTO_INSECURE_COOKIES` | `insecure_cookies` | `false` | send the session cookie over plain http (local development only) |
| `-trusted-proxies` | `TASKTO_TRUSTED_PROXIES` | `trusted_proxies` | | reverse proxies whose `X-Forwarded-For` gives the client's address: comma-separated IP addresses or CIDR prefixes ([docs/install.md](docs/install.md#the-clients-address-behind-a-proxy)) |
| `-sso-providers` | `TASKTO_SSO_PROVIDERS` | `sso_providers` | | the single sign-on providers file, JSON ([docs/sso.md](docs/sso.md)); needs `-public-url` |
| `-public-url` | `TASKTO_PUBLIC_URL` | `public_url` | | the server's external `https` origin, such as `https://taskto.example.com`, which single sign-on's redirect URIs are built on |
| `-disable-password-login` | `TASKTO_DISABLE_PASSWORD_LOGIN` | `disable_password_login` | `false` | turn password logins off, so everyone signs in with a provider; needs `-sso-providers` |
| `-workspace-creation` | `TASKTO_WORKSPACE_CREATION` | `workspace_creation` | `admins` | who may create more workspaces than setup's: `admins` (an admin of any workspace), `anyone` (everyone signed in) or `off` ([docs/install.md](docs/install.md#who-may-create-a-workspace)) |
| `-provider-egress-workspace` | `TASKTO_PROVIDER_EGRESS_WORKSPACE` | `provider_egress_workspace` | `any` | where workspaces' provider accounts may connect: `any`, `public`, or a comma-separated list of `public`, hosts, IP addresses and CIDR prefixes ([docs/install.md](docs/install.md#where-provider-accounts-may-connect)) |
| `-provider-egress-personal` | `TASKTO_PROVIDER_EGRESS_PERSONAL` | `provider_egress_personal` | `any` | the same for members' personal provider accounts |
| `-smtp-host` | `TASKTO_SMTP_HOST` | `smtp_host` | | the SMTP server that sends the server's email; empty means no email ([docs/install.md](docs/install.md#email)) |
| `-smtp-port` | `TASKTO_SMTP_PORT` | `smtp_port` | `587`, `465` or `25` | the SMTP server's port, by `-smtp-security` |
| `-smtp-security` | `TASKTO_SMTP_SECURITY` | `smtp_security` | `starttls` | `starttls`, `tls` (implicit TLS) or `none` (a trusted local relay, with no sign-in) |
| `-smtp-from` | `TASKTO_SMTP_FROM` | `smtp_from` | | the sender: an address, or `Name <address>`; needed with `-smtp-host` |
| `-smtp-username` | `TASKTO_SMTP_USERNAME` | `smtp_username` | | the user to sign in as; goes with `-smtp-password-file` |
| `-smtp-password-file` | `TASKTO_SMTP_PASSWORD_FILE` | `smtp_password_file` | | the file the SMTP password is read from at startup (an absolute path); the password is never a setting |
| `-smtp-egress` | `TASKTO_SMTP_EGRESS` | `smtp_egress` | `any` | where the SMTP connection may go, as the provider accounts' policy above |
| `-integration-egress` | `TASKTO_INTEGRATION_EGRESS` | `integration_egress` | `public` | where calls to integrations' providers (Stripe, Square) may go: `public` (addresses checked on every connection), `any`, or a list as for the provider accounts' policy above ([docs/integrations.md](docs/integrations.md#egress)) |
| `-update-status-dir` | `TASKTO_UPDATE_STATUS_DIR` | `update_status_dir` | | where the host's updater writes the status operators see in Settings → About (read-only; an absolute path; [docs/install.md](docs/install.md#automatic-updates)) |
| `-update-request-dir` | `TASKTO_UPDATE_REQUEST_DIR` | `update_request_dir` | | where Update now (Settings → About) puts its request for the host's updater to take (writable; an absolute path; [docs/install.md](docs/install.md#update-now)) |
| `-agent` | `TASKTO_AGENT` | `agent` | `false` | run the agent runtime (see below) |
| `-agent-node` | `TASKTO_AGENT_NODE` | `agent_node` | `node` | Node.js executable for the agent runtime, looked up in `PATH` unless it is a path |
| `-agent-entry` | `TASKTO_AGENT_ENTRY` | `agent_entry` | see below | the agent runtime's entry point |
| `-agent-providers` | `TASKTO_AGENT_PROVIDERS` | `agent_providers` | | the operator's model providers file ([docs/agent-providers.md](docs/agent-providers.md)), offered to everyone before the providers users and workspaces add; without it, the scripted offline model |
| `-cli-workers` | `TASKTO_CLI_WORKERS` | `cli_workers` | `off` | run conversations on the users' own Claude Code, Codex and Grok Build logins: `off`, `container` (a worker of its own, see `-cli-worker-socket`), `local` (one-user instances only) or `local-unsafe` (development) ([docs/cli-workers.md](docs/cli-workers.md)) |
| `-cli-worker-socket` | `TASKTO_CLI_WORKER_SOCKET` | `cli_worker_socket` | | the CLI worker's socket; needed with `-cli-workers container` |
| `-cli-proxy-listen` | `TASKTO_CLI_PROXY_LISTEN` | `cli_proxy_listen` | `127.0.0.1` on a free port with `local` and `local-unsafe`; none with `container` | where the per-turn proxy listens (workspaces' shared CLI accounts, and TaskTo's tools for CLI turns), apart from the public address: `host:port`, or `unix:/path` in container mode, for the worker to relay ([docs/account-switching.md](docs/account-switching.md)) |
| `-cli-proxy-url` | `TASKTO_CLI_PROXY_URL` | `cli_proxy_url` | its listen address with `local` and `local-unsafe` | the per-turn proxy as the CLIs reach it, such as `http://127.0.0.1:8081`; needs `-cli-proxy-listen`; in container mode it is required whenever `-cli-proxy-listen` is set, TCP or `unix:`, and with a `unix:` listener it is the worker's relay's URL |
| `-browser` | `TASKTO_BROWSER` | `browser` | `false` | give the agent a browser per user, with a live view and hand-over for logins; needs `-cli-workers` ([docs/agent-browser.md](docs/agent-browser.md)) |
| `-browser-chromium` | `TASKTO_BROWSER_CHROMIUM` | `browser_chromium` | `chromium` | the agent browser's Chromium in local mode, looked up in `PATH`; by default `chromium`, then `chromium-browser` |
| `-unsafe-privileged-agents` | `TASKTO_UNSAFE_PRIVILEGED_AGENTS` | `unsafe_privileged_agents` | `false` | development only: start the agent runtime and `local` CLI workers even when the server runs as root or has a passwordless `sudo` ([docs/agent-safety.md](docs/agent-safety.md)) |
| `-kb-max-file-size` | `TASKTO_KB_MAX_FILE_SIZE` | `kb_max_file_size` | `2GiB` | largest file of any kind in the knowledge base (bytes, or with `KiB`, `MiB`, `GiB`) |
| `-kb-large-file-size` | `TASKTO_KB_LARGE_FILE_SIZE` | `kb_large_file_size` | `50MiB` | files other than Markdown over this size are kept out of git, unversioned, as audio, video and heavy binaries such as archives always are; also the largest Markdown document ([docs/api.md](docs/api.md#unversioned-files)) |
| `-kb-max-repo-size` | `TASKTO_KB_MAX_REPO_SIZE` | `kb_max_repo_size` | `1GiB` | cap on each space's knowledge-base repository, history included (unversioned files' bytes aside) |
| `-kb-max-unversioned-size` | `TASKTO_KB_MAX_UNVERSIONED_SIZE` | `kb_max_unversioned_size` | `20GiB` | cap on the bytes of each space's unversioned files together |
| `-storage-quota-workspace` | `TASKTO_STORAGE_QUOTA_WORKSPACE` | `storage_quota_workspace` | unlimited | most each workspace may store in all its spaces together, knowledge base and app photos (bytes, or with `KiB`, `MiB`, `GiB`); writes over it are refused ([docs/quotas.md](docs/quotas.md)) |
| `-storage-quota-user` | `TASKTO_STORAGE_QUOTA_USER` | `storage_quota_user` | unlimited | most each member's private space may store, in each workspace they belong to ([docs/quotas.md](docs/quotas.md)) |
| `-account-deletion-days` | `TASKTO_ACCOUNT_DELETION_DAYS` | `account_deletion_days` | `14` | days a deleted account is kept, switched off, before what it owned alone is purged; an operator can cancel until then ([docs/install.md](docs/install.md#privacy-and-account-deletion)) |
| `-search-vector-cache` | `TASKTO_SEARCH_VECTOR_CACHE` | `search_vector_cache` | `1GiB` | most memory semantic search keeps vectors in, or `off`; a passage of 768 dimensions takes about 3 KiB, so 200,000 take about 600 MiB. Spaces that don't fit are read from the database at each search, about 25 times slower |
| `-dictation-local` | `TASKTO_DICTATION_LOCAL` | `dictation_local` | `parakeet-v3` | the server's speech model for workspaces using the `local` engine (the workspace default; an admin can also choose the hosted API or an installed add-on): `parakeet-v3`, `whistle` or `off`; downloaded on first use ([docs/install.md](docs/install.md#dictation)) |
| `-dictation-cache` | `TASKTO_DICTATION_CACHE` | `dictation_cache` | `<data-dir>/cache/dictation` | where the local speech model's files are kept |
| `-addons-dir` | `TASKTO_ADDONS_DIR` | `addons_dir` | `<data-dir>/addons` | where the operator installs add-ons such as Desert Ant Voz ([docs/install.md](docs/install.md#dictation)) |
| `-audit-retention` | `TASKTO_AUDIT_RETENTION` | `audit_retention` | `365` | days the audit log keeps its events, or `off` to stop deleting them by age (a workspace's events still go when it is deleted; [docs/audit.md](docs/audit.md)) |
| `-push-contact` | `TASKTO_PUSH_CONTACT` | `push_contact` | `https://taskto.ai` | how browsers' push services can reach whoever runs the server (the VAPID contact): a `mailto:` or `https:` URL ([docs/install.md](docs/install.md#web-push-notifications)) |
| `-config` | `TASKTO_CONFIG` | | | config file to read |
| `-version` | | | `false` | Print the version and exit |

The config file is a small subset of TOML: `key = value` lines, strings in double
quotes, `true`/`false`, and `#` comments.

```toml
# taskto.toml
addr = "0.0.0.0:8080"
data_dir = "/var/lib/taskto"
```

`GET /healthz` reports the version, whether the database answers, and the agent runtime's
state (`agent`: `off`, `starting`, `ready` or `stopped`). It returns 503 when the database
doesn't answer; the agent runtime doesn't change the status, because the server works
without it.

### The agent runtime

The agent runs in a Node.js sidecar (`agent/`, Node 22.19+) that the server starts,
restarts when it exits, and stops. It is off by default; `make dev-server` turns it on.
It keeps its state in the data directory: its database in `agent/agent.db`, its socket
in `agent/run/agent.sock`, in a directory only the server's user may enter, and the
providers file the server writes for it, `agent/providers.json`, with its API keys in
`agent/keys/`. None of these is configured separately. A Unix socket path can't be longer than 103 bytes, so if the data
directory's path is too long the server stops and says so: choose a shorter one.

Without `-agent-entry`, the server looks for `agent/dist/index.js` (built by
`pnpm --filter @taskto/agent build`) next to its binary, one directory above it (as with
`bin/taskto` in a checkout), then in the working directory. If the Node.js executable, the
entry point or the providers file is missing, the server doesn't start. The sidecar's
stdin is a pipe from the server, and it exits when that closes, so it never outlives the
server, even one killed with `kill -9`.

The data directory also holds `server.key`, which encrypts the provider API keys stored in
the database. Without it those keys are lost, and with both anyone can read them: `taskto
backup` includes it, so keep backups as private as the server and encrypt them before they
leave the machine.

On first start the server has no users, and prints a one-time setup token to its log.
`POST /api/setup` with that token creates the owner and the first workspace, so only
someone who can read the server's log can set it up. The owner invites the rest. Serve it
over HTTPS: the session cookie is `Secure`. See [docs/install.md](docs/install.md) and
[docs/api.md](docs/api.md).

### Backups

`taskto backup [-config file] [-data-dir dir] [-uncompressed] FILE|-` writes a consistent
snapshot of the data directory while the server runs: SQLite databases through `VACUUM
INTO`, other files (`server.key`, the spaces) as they are; `-uncompressed` writes a plain
tar, for restic. `taskto restore [-config file] [-data-dir dir] FILE|-` restores one into
an empty data directory. Both find the data directory as the server does. The install
guide sets up backups the server can't delete: restic into an append-only rest-server, or
a bucket with object lock, with pruning done elsewhere. `taskto backup-schedule` sends one
there every night, retries a failed night, and keeps the outcome where admins see it
(Settings → About) and monitoring can (`/healthz/backup`). See
[docs/install.md](docs/install.md#backups).

`taskto conversations check` finds conversations the server and the agent runtime disagree
about, and `taskto conversations -lost -orphans clean` removes them. See
[docs/install.md](docs/install.md#conversations-out-of-step-with-the-agent-runtime).

## Licence

[MIT](LICENSE)
