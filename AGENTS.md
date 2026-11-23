# TaskTo.AI — instructions for agents and contributors

TaskTo.AI is an open-source (MIT), self-hosted workspace for one person or a whole company:
a knowledge base, an agent that works with it, boards for everyone's tasks, and personal
apps built by asking the agent. See [docs/architecture.md](docs/architecture.md).

## Layout

| Path | What | Language |
|---|---|---|
| `cmd/taskto/` | the single binary | Go |
| `internal/` | server packages (HTTP, storage, permissions, knowledge base, boards, apps) | Go |
| `internal/apierr/` | the API's errors: `{"error", "code", "params"}`, with every code in `codes.go`, which the web app translates | Go |
| `internal/config/` | flags, `TASKTO_*` variables and the config file, in that order of precedence | Go |
| `internal/db/` | SQLite (modernc.org/sqlite, no cgo) and the embedded migrations in `migrations/` | Go |
| `internal/auth/` | users, workspaces, members, invites, sessions, passwords and their reset links (`passwords.go`), the auth middleware and the CSRF guard | Go |
| `internal/access/` | spaces (private and shared), who may read and write them, their data directories and safe paths | Go |
| `internal/knowledge/` | the knowledge base: each space's documents and uploads in a git repository (go-git, through the space's `os.Root`); large files, audio and video, and heavy binaries (archives, disk images, design files) unversioned, a Git LFS pointer in git and their bytes in the space's `.git/lfs/objects/` (`large.go`); a versioned file that becomes unversioned leaves history, by a crash-safe rewrite of the space's commits (`rewrite.go`) | Go |
| `internal/search/` | the knowledge base's search index: full text (SQLite FTS5) and optional semantic search, kept in step with git | Go |
| `internal/boards/` | personal and team boards, cards and their threads, and the methods the agent's board tools call | Go |
| `internal/apps/` | personal apps: the component catalog and the strict check of saved screens, collections and their rows, the actions Go runs, the effects of actions on boards and documents with their transactional outbox and its worker (`effects.go`, `outbox.go`, `effects_api.go`), the methods the agent's app tools call, photo attachments (`attachments.go`: chunked, resumable uploads into the space's managed storage, thumbnails, metadata stripped at finalize, authenticated reads by record access, orphan cleanup, files following an app that moves), the capture fields (`capture.go`: location, signature and barcode values, checked strictly and, for a location and a signature, locked once stored), the download of the live release and the caller's records for offline use, which an admin can refuse for a shared space (`offline.go`), and the extension slots (`contributions.go`: widgets and a pin an app offers) with each person's Home placements and a shared space's defaults (`home.go`); the bundled starter templates (`templates/`, [docs/templates.md](docs/templates.md)) and copies of an app, both only a definition into a new draft with no records, roles or secrets, where effect targets the copier can't write are cleared (`templates.go`) | Go |
| `internal/notifications/` | the notification inbox and optional Web Push: notifications are references (`Add`, from the boards and the apps' outbox), rendered when read or pushed from what the recipient may read then (`render.go`); the inbox API, push subscriptions bound to a session, the VAPID key pair sealed with `serverkey`, message encryption (RFC 8291) and VAPID (RFC 8292) with the standard library only (`webpush.go`), and the worker that sends pushes with bounded tries under the egress policy for public addresses (`push.go`); a push never carries record data ([docs/pwa.md](docs/pwa.md#notifications)) | Go |
| `internal/conversations/` | conversations with the agent: who owns each, the API to them, their live updates as Server-Sent Events, and finding where they drift from the agent runtime's | Go |
| `internal/agenttools/` | the knowledge-base, board and app tools of agents, defined once in `tools.json`: the sidecar's tools call back into it for the conversation's owner, and MCP calls it for an access token's user | Go |
| `internal/mcpserver/` | the MCP endpoint, `/mcp`: the same tools for people's own agents, with an access token ([docs/mcp.md](docs/mcp.md)); and `ForTurns`, the endpoint for CLI turns, `/cli-mcp` on the per-turn proxy's listener, with the browser tools too ([docs/cli-tools.md](docs/cli-tools.md)) | Go |
| `internal/cliworker/` | CLI workers: runs the official Claude Code, Codex and Grok Build CLIs for users, one process group per turn with a clean environment in the user's own directories, and turns their output into one event stream ([docs/cli-workers.md](docs/cli-workers.md)); every CLI starts through the helper (`taskto cli-helper`), as the user's own Unix user in container mode (`taskto cli-worker`, whose API the server reaches with `Remote`); `clitest/` has fake CLIs for tests | Go |
| `internal/browser/` | the agent browser ([docs/agent-browser.md](docs/agent-browser.md)): a Chromium per user, driven over its DevTools pipe (`cdp.go`, `page.go`, with `snapshot.js` run in the page's isolated world); the personal one headed on Xvfb with a kept profile and a live view through x11vnc on a Unix socket, the testing one headless and profile-less; the `Manager` starts them on use, stops them when idle and caps how many run; processes start through a `Host`, as the server's user (`LocalHost`) or as the user's Unix user in the CLI worker | Go |
| `internal/agentbrowser/` | the server's side of the agent browser: `/api/me/browser`, the live view's WebSocket (session and same-origin checked, bridged to the user's own browser only), and the agent's `browser_*` tools for the conversation's owner; the browsers run in the CLI worker (`browser.Remote`) or, in local mode, in the server (`browser.LocalHost`) | Go |
| `internal/cliaccounts/` | each user's CLI accounts (Claude Code, Codex, Grok Build): the `-cli-workers` mode and its one-user rule for local mode, the logins through each CLI's own login flow, their state and usage limits, `/api/me/cli-accounts`, and the `/cli/` routes on the tools socket that run a conversation's turn on its owner's login (the sidecar's side is `agent/src/cli.ts`); several accounts per CLI, and workspaces' shared accounts (API keys, with members' modes) that turns use through the proxy on a listener of its own (`proxy.go`), which adds the key ([docs/account-switching.md](docs/account-switching.md)); and every turn's token for TaskTo's tools, checked on each request at `/cli-mcp` on that listener (`tools.go`, [docs/cli-tools.md](docs/cli-tools.md)) | Go |
| `internal/dictation/` | dictation in the chat box: each workspace's speech-to-text settings and sealed API key, the engines (a hosted OpenAI-compatible API; the operator's local model, its pinned and sha256-checked downloads and the runtime run per dictation; add-ons the operator installs, such as Desert Ant Voz), the knowledge base's glossary and the correction it makes ([docs/dictation.md](docs/dictation.md)); never commit a model or a runtime, and keep add-ons' code out | Go |
| `internal/providers/` | model provider accounts of workspaces and users, and the agent runtime's providers file | Go |
| `internal/egress/` | egress policies: which addresses calls to provider accounts' endpoints may connect to, checked on every connection; every Go caller of a `base_url` uses its client (the sidecar's twin is `agent/src/egress.ts`) | Go |
| `internal/serverkey/` | the server's own key (`<data>/server.key`), sealing secrets such as API keys | Go |
| `internal/lockfile/` | the data directory's lock (`<data>/taskto.lock`) and the agent runtime's, both exclusive `flock`s that children don't inherit | Go |
| `internal/backup/` | `taskto backup` / `taskto restore`: a consistent snapshot of the data directory (gzipped, or a plain tar for restic), and restoring one; `taskto backup-schedule`, the nightly backup to restic with its retries, and its status in `<data>/backup-schedule/status.json`, which the server shows admins and `/healthz/backup` (`schedule.go`, `status.go`, `restic.go`) | Go |
| `internal/webui/` | serves the built web app from the binary, with `sw.js` and the manifest never cached; `make build` copies `web/dist` into its `dist/` | Go |
| `web/` | the web app: React + Vite; setup, login, invites, the shell (workspaces, sections), the conversations chat over the events stream (with the apps the agent saves in it launched inline), the knowledge base (editing, history, uploads) with search, the Home page (apps' widgets, in error boundaries of their own) and the pins in the navigation (`web/src/home/`), the boards, the workspace settings (members, shared spaces, model providers, search consent, your access tokens, your CLI accounts with their sign-in and usage limits), and the apps page with the A2UI renderer and the app builder (`web/src/apps/AppBuilder.tsx`: the agent's conversation beside a preview of the draft, which runs on sample rows the server never stores, and a Build and Preview switch on a phone); the installable app (`web/pwa/`, the manifest and icons in `web/public/`, `web/src/pwa/`: an offline shell, updates that wait for the person, install guidance, [docs/pwa.md](docs/pwa.md)); `web/src/markdown.tsx` is the one Markdown renderer, which never makes HTML; the offline use of apps (`web/src/offline/`: a downloaded release and records and a durable outbox of inserts in IndexedDB, one database per user, sent with their operation keys and photos uploaded first, [docs/offline.md](docs/offline.md)); `web/src/i18n/` has the languages: `t()`, the dates' and numbers' formats, and a file per language in `locales/` ([docs/i18n.md](docs/i18n.md)) | TypeScript |
| `agent/` | the agent runtime sidecar, hosting Pi Durable; its system prompt says the user's language (`src/language.ts`, [docs/i18n.md](docs/i18n.md#the-agent-and-dictation)) | TypeScript (Node) |
| `docs/` | architecture and design notes | Markdown |
| `Dockerfile`, `compose.yaml`, `deploy/` | the container images (the server; `--target cli-worker`, the CLI worker with the CLIs pinned in `deploy/cli-workers.json`; `--target backup`, the backup job with restic; CI publishes all three to ghcr.io with immutable tags, `.github/workflows/images.yml`) and the Compose install ([docs/install.md](docs/install.md); `deploy/INSTALL.md`, the step-by-step runbook from published images, for an agent over SSH), with `deploy/backup-host/`, the append-only backup host, and `deploy/worker-firewall.sh`, the host firewall that leaves the CLI worker's own network the internet only (`deploy/worker-network-test.sh` proves it in CI); never enable development switches there, and never let the server's side delete backups (tests check) | |

## Rules

- **One server process per data directory.** Exactly one server process uses a data
  directory at a time. Agents (and tests, tools and scripts) may read a space's files, but
  never write its git repository: every change is a commit `internal/knowledge` makes, the
  only code that writes one, with a one-writer-at-a-time lock that is in-process. The
  server enforces the rule: its first step, before it opens the database, is an
  exclusive flock on `<data>/taskto.lock` (`internal/lockfile`), held until it exits and
  never inherited by the sidecar; a second server exits at once, naming the holder's pid.
  `taskto restore` takes the same lock; `taskto backup` and `taskto conversations`
  don't, since they run beside the server and go through SQLite or the sidecar. A new
  command that writes the data directory without the server takes it too
  ([docs/install.md](docs/install.md)).
- **Every string people see is translated.** The web app takes its text from
  `web/src/i18n/locales/en.json` through `t()`, never from a literal in a component, and
  formats dates, numbers and sizes with `useFormat()`, never `toLocaleString()`; add the
  key to `en.json` (and to the other files if you can), and CI checks the rest. The
  server answers errors with `internal/apierr` and a code from its `codes.go`, which the
  web app translates (`errors.<code>`), never with text meant to be shown as it is. Lay out
  with logical CSS properties (`margin-inline-start`, not `margin-left`), so
  right-to-left languages work ([docs/i18n.md](docs/i18n.md)).
- **The backend is Go.** TypeScript lives only in `web/` and `agent/`. Don't add a Node
  service anywhere else.
- **The agent sidecar is internal.** Only the Go server talks to it; it never listens on a
  public interface and never handles auth itself. `cmd/taskto` runs it through
  `agentrt.Supervisor` when the agent runtime is on (`-agent`, see the README). Its
  database and socket are derived from the data directory (`<data>/agent/`), never
  configured on their own: one database, one socket, so a leftover sidecar is always found.
- **Isolation is a feature, not a later fix.** Every new API checks the caller's access to
  the workspace, space, board or app it touches, and has a test for a cross-user attempt.
  Workspace routes live under `/api/workspaces/{workspace}/` and wrap their handler in
  `auth.Service.RequireMember` or `RequireAdmin`. Anything stored for people (files,
  conversations, boards, apps) belongs to a space: its routes live under
  `/api/workspaces/{workspace}/spaces/{space}/` and wrap their handler in
  `access.Checker.RequireSpace`, and code acting for a user (the agent) asks
  `access.Checker` first. Don't write access checks of your own.
- **File names from people or models never meet `filepath.Join`.** Open a space's files
  through `access.OpenRoot`, or build paths with `access.SafeJoin`.
- **State changes are never GETs.** The CSRF guard (`auth.CSRF`) checks every other
  method under `/api` and requires `Content-Type: application/json`, or
  `application/octet-stream` on the routes that take a file's bytes, named in
  `knowledge.RawBodyRoutes` and `apps.RawBodyRoutes`; `GET` routes are left alone. The API is described in [docs/api.md](docs/api.md); keep it current.
- **One binary works for one person or a company.** Don't add code paths that only work in
  one of the two modes.
- **Agents never run as root or with sudo.** The server refuses to start the agent sidecar
  or `local` CLI workers as root or with passwordless `sudo`/`doas`. The container worker
  refuses an image with a setuid or setgid program. The CLI helper refuses to start a CLI as
  root, and sets `no_new_privs`. Don't add a path that runs an agent, a CLI or a model's
  command with more privilege. The one override, `-unsafe-privileged-agents`, is for
  development and is never set by the install ([docs/agent-safety.md](docs/agent-safety.md)).
- **No secrets in an agent's environment.** A CLI's environment is built from nothing
  (`internal/cliworker`). Its only credential is the user's own login, or for a workspace
  account a short-lived token for that turn that the server's proxy swaps for the key
  ([docs/account-switching.md](docs/account-switching.md)). Never put a
  workspace or server secret (an API key, a token, `server.key`) in an agent's environment,
  command line or files. When an agent needs a secret to act, a server-side tool does the
  action for the conversation's owner, and never returns the secret.
- **Agents ask before they act.** CLI turns run with permission prompts, answered by the
  conversation's owner in the web app. Only the owner turns them off, for one conversation
  (`cli_permissions: "allow_all"`). Don't add a default or a setting that allows everything
  for more than that.
- **No secrets in the repo**: no keys, tokens, real hostnames of private servers, or personal
  account names. CI runs gitleaks over the full history.
- **Secrets in the database are sealed** with `serverkey` and never returned, logged or put
  in an error message. API keys are write-only: the API says `"set"` or `"not set"`.
- Keep dependencies few. A new dependency needs a reason in the PR description.
- **Schema changes are new migrations.** Add `internal/db/migrations/NNNN_description.sql`
  with the next number; never edit one that has been merged. Each runs in a transaction
  when the server starts.
- **The Go build must work without the web app built.** `go test`, `go vet` and CI's Go job
  don't build it; `internal/webui` then serves a placeholder page. Don't make Go code
  depend on files under `web/`.

## Commands

```sh
make check        # lint, go test, web + agent build (what CI runs)
make lint         # golangci-lint (Go) and Biome (TypeScript)
make fmt          # format, and apply the linters' safe fixes
make dev-server   # Go server on 127.0.0.1:8080, data in ./data, plain-http cookies and the agent runtime on
make agent-it     # integration tests with the real agent sidecar (needs Node), as CI runs them
make dictation-e2e # dictation in a real Chromium with the real local model (~700 MB, ~11 min); never in CI
make browser-handover-e2e # the browser's hand-over with a real model on a gateway, in Docker; never in CI
make dev-web      # Vite dev server, proxies the API to the Go server
make build        # web + agent build, then bin/taskto with the web app embedded
```

Tooling: Go (version in `go.mod`), Node 22.19+, and the pnpm version in `packageManager` in
`package.json` (the only place it's pinned; `npx pnpm@<that version>` works if pnpm isn't
installed).

## Workflow

- `main` is protected: every change goes through a pull request with green CI.
- Small PRs with conventional titles (`feat:`, `fix:`, `docs:`, `chore:`), squash-merged.
- A PR description says what changed, why, and how it was checked.
- When CI's Go job fails, its "Test results" step prints each package's result and the whole
  output of every failing test (`.github/scripts/gotest-report.go`), and the run keeps
  `go test -json`'s output as the `go-test-json` artifact for 30 days.
- CI is `.github/workflows/`, on GitHub-hosted runners only: never a self-hosted runner,
  since fork pull requests run there. Its scripts are in `.github/scripts/`.
- Docs, comments and tests explain themselves inside the repository: no issue or ticket
  numbers, links to private notes, private hostnames or people's names. CI's reference
  check (`.github/scripts/refguard.go`) fails on them.
