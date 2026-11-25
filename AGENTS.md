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
| `internal/auth/` | users, workspaces (the first by setup, more by `POST /api/workspaces` as far as `-workspace-creation` allows), members, invites, sessions, passwords and their reset links (`passwords.go`), disabling a member (`disable.go`: signed out everywhere and refused at login, as an access token's user and as a conversation's owner, everything kept so enabling restores access; who may is the reset links' rule; `live.go`, `WatchSession`: a connection that outlives the request, such as the agent browser's live view, ends with its login; queued work of a disabled account waits, through `access.ErrDisabled`, instead of failing), single sign-on through OpenID Connect for invitees only (`sso.go`; the operator's providers file, `ssoconfig.go`; [docs/sso.md](docs/sso.md)), two-factor login for password logins (`twofactor.go`, `totp.go`: a TOTP secret sealed with `serverkey`, recovery codes kept as keyed hashes, a half-done login that is a challenge and never a session, its own limits; single sign-on has no second step; [docs/api.md](docs/api.md#two-factor-login)), your own sessions, listed with the browser's and system's names and last use, and signed out one by one or all but the current (`sessions.go`, `device.go`), the auth middleware and the CSRF guard | Go |
| `internal/audit/` | the audit log of security and admin actions: `Record` writes an event in the caller's transaction (actor and client address from the request's context, each action's details fixed and typed, so no free text is recorded; the target's creation time with its id), `Log` for an event with no transaction, `Prune` and `Retain` the retention sweep, `ListWorkspace` the admins' read of one workspace's events (it refuses a missing workspace; the route is `GET /api/workspaces/{workspace}/audit` in `internal/server/audit.go`); `audittest/` checks that no known secret is in any row ([docs/audit.md](docs/audit.md)) | Go |
| `internal/access/` | spaces (private and shared), who may read and write them, their data directories and safe paths | Go |
| `internal/knowledge/` | the knowledge base: each space's documents and uploads in a git repository (go-git, through the space's `os.Root`); large files, audio and video, and heavy binaries (archives, disk images, design files) unversioned, a Git LFS pointer in git and their bytes in the space's `.git/lfs/objects/` (`large.go`); a versioned file that becomes unversioned leaves history, by a crash-safe rewrite of the space's commits (`rewrite.go`) | Go |
| `internal/search/` | the knowledge base's search index: full text (SQLite FTS5) and optional semantic search, kept in step with git; and the full-text search of board cards (`cards.go`), apart from documents, never semantic, fed by a queue that triggers fill when a card's title or body changes or its thread does, ranked from each card's own text, never from the index's statistics, and access checked again after indexing | Go |
| `internal/boards/` | personal and team boards, cards and their threads, and the methods the agent's board tools call | Go |
| `internal/apps/` | personal apps: the component catalog and the strict check of saved screens, collections and their rows, the actions Go runs, the effects of actions on boards and documents with their transactional outbox and its worker (`effects.go`, `outbox.go`, `effects_api.go`), the methods the agent's app tools call, photo attachments (`attachments.go`: chunked, resumable uploads into the space's managed storage, thumbnails, metadata stripped at finalize, authenticated reads by record access, orphan cleanup, files following an app that moves), the capture fields (`capture.go`: location, signature and barcode values, checked strictly and, for a location and a signature, locked once stored), the download of the live release and the caller's records for offline use, which an admin can refuse for a shared space (`offline.go`), and the extension slots (`contributions.go`: widgets and a pin an app offers) with each person's Home placements and a shared space's defaults (`home.go`); the bundled starter templates (`templates/`, [docs/templates.md](docs/templates.md)) and copies of an app, both only a definition into a new draft with no records, roles or secrets, where effect targets the copier can't write are cleared (`templates.go`) | Go |
| `internal/notifications/` | the notification inbox and optional Web Push: notifications are references (`Add`, from the boards and the apps' outbox), rendered when read or pushed from what the recipient may read then (`render.go`); the inbox API, email (`email.go`: per-person, per-kind choices, off by default, instant mails from a queue that a worker of its own sends and a daily summary on the general scheduler that schedules each next run from the local date, each mail written to a ledger before it is submitted and never submitted twice unless the relay definitely refused it, the hourly limit counted there, and nothing sent to an address its owner hasn't confirmed by a link (`emailverify.go`), never with record data, only where the server has SMTP and a public URL), push subscriptions bound to a session, the VAPID key pair sealed with `serverkey`, message encryption (RFC 8291) and VAPID (RFC 8292) with the standard library only (`webpush.go`), and the worker that sends pushes with bounded tries under the egress policy for public addresses (`push.go`); a push never carries record data ([docs/pwa.md](docs/pwa.md#notifications)) | Go |
| `internal/schedule/` | the general scheduler for timed work that runs for people (reminders, polling, scheduled app actions): jobs in `scheduled_jobs` of a kind whose handler the server's Go code registers before the worker starts (a closed list), with an owner, workspace and space, a payload of IDs with no secret, a due time, an optional interval and a dedupe key unique per owner and kind; `Schedule`, `Reschedule` and `Cancel` take the caller's transaction; the worker (`Start`, in the server process, one per data directory) takes a due job with a lease and runs it at least once with bounded tries and backoff, so handlers are idempotent, honour their context (a result after the time limit is a failed try) and key what they make by `Job.Occurrence()` (job, revision, time); times are inside `MinRunAt`..`MaxRunAt`, which the schema checks; the owner's access is checked through `access.Checker` when the job runs, never trusted from when it was scheduled, and a handler returns `ErrAccessLost` when refused, and a handler that makes something asks `Holds` in its own transaction that its job is still the live claim; the schema drops a person's jobs when they leave the space or the workspace; tests use an injected clock ([docs/architecture.md](docs/architecture.md#scheduled-work)) | Go |
| `internal/checklist/` | the first-run checklist on an admin's Home: which steps (invite the team, a model provider, a shared space, an app; optionally email and backups) are done is worked out from what exists, never ticked; only the workspace's dismissal and its skipped steps are stored (`setup_checklist_*`); admin-only routes, `/api/workspaces/{workspace}/checklist` ([docs/api.md](docs/api.md#setup-checklist)) | Go |
| `internal/conversations/` | conversations with the agent: who owns each, the API to them, their live updates as Server-Sent Events, and finding where they drift from the agent runtime's | Go |
| `internal/agenttools/` | the knowledge-base, board and app tools of agents, defined once in `tools.json`: the sidecar's tools call back into it for the conversation's owner, and MCP calls it for an access token's user | Go |
| `internal/mcpserver/` | the MCP endpoint, `/mcp`: the same tools for people's own agents, with an access token ([docs/mcp.md](docs/mcp.md)); and `ForTurns`, the endpoint for CLI turns, `/cli-mcp` on the per-turn proxy's listener, with the browser tools too ([docs/cli-tools.md](docs/cli-tools.md)) | Go |
| `internal/cliworker/` | CLI workers: runs the official Claude Code, Codex and Grok Build CLIs for users, one process group per turn with a clean environment in the user's own directories, and turns their output into one event stream ([docs/cli-workers.md](docs/cli-workers.md)); every CLI starts through the helper (`taskto cli-helper`), as the user's own Unix user in container mode (`taskto cli-worker`, whose API the server reaches with `Remote`); `clitest/` has fake CLIs for tests | Go |
| `internal/browser/` | the agent browser ([docs/agent-browser.md](docs/agent-browser.md)): a Chromium per user, driven over its DevTools pipe (`cdp.go`, `page.go`, with `snapshot.js` run in the page's isolated world); the personal one headed on Xvfb with a kept profile and a live view through x11vnc on a Unix socket, the testing one headless and profile-less; the `Manager` starts them on use, stops them when idle and caps how many run; processes start through a `Host`, as the server's user (`LocalHost`) or as the user's Unix user in the CLI worker | Go |
| `internal/agentbrowser/` | the server's side of the agent browser: `/api/me/browser`, the live view's WebSocket (session and same-origin checked, bridged to the user's own browser only), and the agent's `browser_*` tools for the conversation's owner; the browsers run in the CLI worker (`browser.Remote`) or, in local mode, in the server (`browser.LocalHost`) | Go |
| `internal/cliaccounts/` | each user's CLI accounts (Claude Code, Codex, Grok Build): the `-cli-workers` mode and its one-user rule for local mode, the logins through each CLI's own login flow, their state and usage limits, `/api/me/cli-accounts`, and the `/cli/` routes on the tools socket that run a conversation's turn on its owner's login (the sidecar's side is `agent/src/cli.ts`); several accounts per CLI, and workspaces' shared accounts (API keys, with members' modes) that turns use through the proxy on a listener of its own (`proxy.go`), which adds the key ([docs/account-switching.md](docs/account-switching.md)); and every turn's token for TaskTo's tools, checked on each request at `/cli-mcp` on that listener (`tools.go`, [docs/cli-tools.md](docs/cli-tools.md)) | Go |
| `internal/dictation/` | dictation in the chat box: each workspace's speech-to-text settings and sealed API key, the engines (a hosted OpenAI-compatible API; the operator's local model, its pinned and sha256-checked downloads and the runtime run per dictation; add-ons the operator installs, such as Desert Ant Voz), the knowledge base's glossary and the correction it makes ([docs/dictation.md](docs/dictation.md)); never commit a model or a runtime, and keep add-ons' code out | Go |
| `internal/mail/` | outgoing email: a small `Sender` interface, an SMTP implementation on the standard library (STARTTLS or implicit TLS, `PLAIN`/`LOGIN`; the connection goes through `internal/egress`; the password comes from a file at startup and never leaves memory), and the translated templates (`locales/<tag>.json`, embedded; `Catalog.Render` makes a message in the recipient's language); one SMTP server for the whole server, set by the operator (`smtp_*` settings, [docs/install.md](docs/install.md#email)); the admins' status and test-send routes are in `internal/server/mail.go`, and a test mail goes only to the caller's own address | Go |
| `internal/export/` | a person's export of their own data as a streamed zip (`POST /api/workspaces/{workspace}/export` to start it, then a single-use `GET …/export/{token}` for the zip, [docs/api.md](docs/api.md#export-my-data)): only their private space's documents (current versions, unversioned files from storage), boards with comments, their conversations in the workspace and their private apps' records, read through each package and never written; entry names cleaned and made unique (`names.go`), rate-limited (`gate.go`), download tickets (`ticket.go`), no access tokens | Go |
| `internal/providers/` | model provider accounts of workspaces and users, and the agent runtime's providers file | Go |
| `internal/egress/` | egress policies: which addresses calls to provider accounts' endpoints may connect to, checked on every connection; every Go caller of a `base_url` uses its client (the sidecar's twin is `agent/src/egress.ts`) | Go |
| `internal/serverkey/` | the server's own key (`<data>/server.key`), sealing secrets such as API keys | Go |
| `internal/quota/` | storage quotas: the operator's default for a workspace and for each member's private space (0 = unlimited, the default), checked by the knowledge base (`knowledge.Store.SetQuota`, inside the space's write lock, for changes that add content) and by the apps' uploads (declared size at start, kept size at finalize) with a `*quota.Error` the HTTP routes answer `507 quota_workspace` or `quota_user`; the usage view (`/usage`, admins; `/usage/me`) reads the knowledge base's counters (`knowledge.Store.Usage`, kept as it commits, never a walk of git per request) and one indexed query for photos ([docs/quotas.md](docs/quotas.md)) | Go |
| `internal/drain/` | tracks the requests in flight on a listener, so shutting down can end their contexts, close the connections and wait for the handlers before the database closes (`http.Server` does none of that for a hijacked or force-closed connection); every listener whose handlers use the database (the public server, the CLI proxy, the sidecar's tools socket) goes through it, and `cmd/taskto/shutdown.go` leaves the database open, and exits with an error, when something still runs after the bounded wait | Go |
| `internal/lockfile/` | the data directory's lock (`<data>/taskto.lock`) and the agent runtime's, both exclusive `flock`s that children don't inherit | Go |
| `internal/backup/` | `taskto backup` / `taskto restore`: a consistent snapshot of the data directory (gzipped, or a plain tar for restic), and restoring one; `taskto backup-schedule`, the nightly backup to restic with its retries, and its status in `<data>/backup-schedule/status.json`, which the server shows admins and `/healthz/backup` (`schedule.go`, `status.go`, `restic.go`) | Go |
| `internal/apitypes/` | writes `web/src/api.gen.ts`, the TypeScript shapes of the JSON replies, from the Go structs that make them (`go generate ./internal/apitypes`; `go test` fails when the committed file is stale); `web/src/api.contract.ts` checks the types of `web/src/api.ts` against it, so a field renamed in Go fails `pnpm typecheck` ([docs/api.md](docs/api.md#api-types)); a new reply struct goes in `Roots` and the contract | Go |
| `internal/webui/` | serves the built web app from the binary, with `sw.js` and the manifest never cached; `make build` copies `web/dist` into its `dist/` | Go |
| `web/` | the web app: React + Vite; setup, login, invites, the shell (workspaces, sections), the conversations chat over the events stream (with the apps the agent saves in it launched inline), the knowledge base (editing, history, uploads) with search, the Home page (apps' widgets, in error boundaries of their own, and for admins the setup checklist, `SetupChecklist.tsx`) and the pins in the navigation (`web/src/home/`), the boards, the workspace settings (members, shared spaces, the audit log, model providers, search consent, your access tokens, your CLI accounts with their sign-in and usage limits), and the apps page with the A2UI renderer and the app builder (`web/src/apps/AppBuilder.tsx`: the agent's conversation beside a preview of the draft, which runs on sample rows the server never stores, and a Build and Preview switch on a phone); the installable app (`web/pwa/`, the manifest and icons in `web/public/`, `web/src/pwa/`: an offline shell, updates that wait for the person, install guidance, [docs/pwa.md](docs/pwa.md)); the look is the taskto.ai website's: colours are tokens at the top of `web/src/styles.css`, written once as `light-dark()` and never as a literal below them, and the fonts are Inter, Bricolage Grotesque and JetBrains Mono served from the app itself, never from a third party ([docs/design.md](docs/design.md)); `web/src/markdown.tsx` is the one Markdown renderer, which never makes HTML; the offline use of apps (`web/src/offline/`: a downloaded release and records and a durable outbox of inserts, edits and deletions with conflict recovery in IndexedDB, one database per user, changes to existing records made by one writer tab at a time (`writer.ts`), sent with their operation keys and photos uploaded first, [docs/offline.md](docs/offline.md)); `web/src/i18n/` has the languages: `t()`, the dates' and numbers' formats, and a file per language in `locales/` ([docs/i18n.md](docs/i18n.md)); `web/e2e/app/` has the end to end tests CI runs in a real Chromium against the built server (`web/e2e/support.mjs`, [docs/web-e2e.md](docs/web-e2e.md)), the other `web/e2e/*.e2e.mjs` being manual ones | TypeScript |
| `agent/` | the agent runtime sidecar, hosting Pi Durable; its system prompt says the user's language (`src/language.ts`, [docs/i18n.md](docs/i18n.md#the-agent-and-dictation)); unit tests are Vitest's, `src/*.test.ts` (`pnpm --filter @taskto/agent run test`), apart from the integration tests of `make agent-it` | TypeScript (Node) |
| `docs/` | architecture and design notes | Markdown |
| `Dockerfile`, `compose.yaml`, `deploy/` | the container images (the server; `--target cli-worker`, the CLI worker with the CLIs pinned in `deploy/cli-workers.json`; `--target backup`, the backup job with restic; CI publishes all three to ghcr.io with immutable tags, `.github/workflows/images.yml`) and the Compose install ([docs/install.md](docs/install.md); `deploy/INSTALL.md`, the step-by-step runbook from published images, for an agent over SSH), with `deploy/backup-host/`, the append-only backup host, and `deploy/worker-firewall.sh`, the host firewall that leaves the CLI worker's own network the internet only (`deploy/worker-network-test.sh` proves it in CI); never enable development switches there, and never let the server's side delete backups (tests check); `deploy/taskto-update.sh` with its systemd unit and timer is the host-side updater ([docs/install.md](docs/install.md#automatic-updates)): pull-based, on the VM, never in the server and never with the Docker socket, it takes every file and image from one commit, checks the new Compose settings against the safety rules before it starts anything, backs up, records a transaction, switches and verifies; a failure before the new server started puts the old version back, one after stops the stack and alerts (`taskto-update --clear-block` once a person has restored the backup), and it never restores data, empties, copies or removes a volume or runs `taskto restore`; it never downgrades and does only the safe common case (it leaves a `manual-upgrade: true` release, a changed worker firewall, an interrupted or stopped update and a server that isn't the version on record to a person, and changes nothing then), and `cmd/taskto/update_test.go` runs it against a fake GitHub and a fake `docker`; `deploy/safety-check.sh` is the one script of the agent safety rules in the running containers, run by runbook step 12 and by the updater, so they can't drift | |

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
- **A reply's shape is one struct, and the web app is checked against it.** A reply the web
  app reads is a named, exported Go struct listed in `internal/apitypes` (`Roots`); after
  changing one, run `go generate ./internal/apitypes` and fix what `pnpm typecheck` says
  ([docs/api.md](docs/api.md#api-types)). Don't build such a reply as a `map`.
- **State changes are never GETs.** The CSRF guard (`auth.CSRF`) checks every other
  method under `/api` and requires `Content-Type: application/json`, or
  `application/octet-stream` on the routes that take a file's bytes, named in
  `knowledge.RawBodyRoutes` and `apps.RawBodyRoutes`; `GET` routes are left alone. The API is described in [docs/api.md](docs/api.md); keep it current.
- **One binary works for one person or a company.** Don't add code paths that only work in
  one of the two modes.
- **Security and admin actions are audited.** A new action that changes who can do what or
  reach what (accounts, sessions, members, invites, spaces and their members, tokens, provider
  or CLI accounts, search and dictation settings, single sign-on) records an event with
  `audit.Record` in the same transaction as the change, and a test checks the row and that a
  refused request records none. An event never holds a secret: a password, token, key, reset
  link or request body ([docs/audit.md](docs/audit.md)). The admin view reads a workspace's
  events only through `audit.ListWorkspace` with the workspace its access check found, and a
  new action gets its label in the web app's two languages (a test checks).
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
make web-e2e      # the web app's end to end tests: Playwright's Chromium against the built server, on every pull request in CI (docs/web-e2e.md)
make screens      # captures of every screen at three sizes, light and dark, into web/screens/ (git-ignored), for UI reviews; never in CI
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
- A release is cut by the Release workflow (`.github/workflows/release.yml`, [docs/releasing.md](docs/releasing.md)): a `vMAJOR.MINOR.PATCH` tag (no leading zeros, no suffix), pushed atomically at the `main` commit whose CI passed, with its images; servers on the release channel update themselves to it. A release that needs a person is cut with the `manual_upgrade` input, which puts `manual-upgrade: true` in its notes from the moment it is published.
- CI is `.github/workflows/`, on GitHub-hosted runners only: never a self-hosted runner,
  since fork pull requests run there. Its scripts are in `.github/scripts/`.
- Docs, comments and tests explain themselves inside the repository: no issue or ticket
  numbers, links to private notes, private hostnames or people's names. CI's reference
  check (`.github/scripts/refguard.go`) fails on them.
