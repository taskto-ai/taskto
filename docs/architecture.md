# Architecture

taskto.ai combines three things that feed each other:

1. **Knowledge base:** the docs, decisions, processes and notes of a person or a team.
2. **Agent:** a chat agent that reads and writes the knowledge base, works the boards and
   builds apps. It cites its sources.
3. **Boards:** a personal board for every user and boards for teams. Cards link to docs and
   conversations, and agents can be assigned cards, like people.

On top of these, users build **personal apps** by asking the agent.

Conversation entries keep the model's messages and TaskTo input metadata together; person-sent
inputs identify their author, while server-started turns omit one.

## Principles

- **The same binary works for one person or a company.** The data model has workspaces and
  members from day one. Going from solo to a team means adding members, not migrating.
- **Real separation between people.** Every user has a private space, a personal board and
  personal apps. Permissions are enforced on files, conversations, boards and apps, and an
  agent only sees what its user may see.
- **Users choose how they reach their models.** Providers, accounts and account switching
  are the user's choice. Each provider's terms are linked in the UI.
- **Agent-first.** Nearly everything can be done by asking the agent. Screens are there for
  speed and overview.

## Components

```
browser ── React app: conversations chat + A2UI renderer
   │
   ▼
Go server (single binary) ── auth, workspaces, permissions, knowledge base,
   │                         boards, app registry + app data, MCP + REST,
   │                         serves the built frontend
   ▼
agent runtime (Node sidecar) ── Pi Durable: conversations, tasks, documents
   │
   ├─ model API worker ── API keys, OpenAI-compatible endpoints, local models
   │                      (Ollama, vLLM), subscription logins the provider allows
   └─ CLI worker ──────── official Claude Code / Codex / Grok CLIs on the user's
                          own login
```

- **Go server:** one binary with the frontend embedded. SQLite (no cgo); there is no other database.
- **Agent runtime:** [Pi Durable](https://earendil.com/posts/pi-durable/) in a Node sidecar
  that only the Go server talks to, over a Unix socket. The server starts it, restarts it
  when it exits and stops it; it never outlives the server. Conversations survive crashes
  and restarts, can fork, and can move between accounts and agents. The agent's
  knowledge-base and board tools call back into the server on a second owner-only socket,
  naming their conversation with a capability the server issued for it; the server acts
  for the conversation's owner, checking their access on every call
  ([agent-tools.md](agent-tools.md)).
- **Agent usage:** `internal/usage` records each person's daily input and output token totals by
  workspace, source and engine. CLI turns are recorded by the Go turn handler; sidecar turns are
  read from the assistant message usage stored in Pi Durable. Each turn and each durable entry is
  counted once: a CLI turn's ID is claimed in the same transaction as its totals, so a replayed or
  overlapping request adds nothing, and a conversation keeps the last entry counted
  (`usage_mark`), advanced in the transaction that records the entries after it. Rows contain no
  conversation text or cost and are kept for 400 days.
- **Spaces and access:** a workspace contains spaces: a private one per member, and shared
  ones with editors and viewers. Everything people store lives in a space, and one
  package, `internal/access`, decides who may read and write each, for the API and for
  the agent acting for a user. Admins don't get into private spaces. Each space has its
  own directory, `<data>/workspaces/<id>/spaces/<id>/`, reached only through paths that
  can't lead out of it. See [api.md](api.md#spaces-and-access).
- **Audit log:** security and admin actions (logins, password resets, members, invites, spaces,
  provider accounts, tokens, single sign-on, search and dictation settings) are recorded in
  `audit_events` by the code that makes the change, in its transaction, with the actor, the
  workspace, the target and the client's address, and never a secret
  ([audit.md](audit.md)).
- **Model providers:** workspaces and users add their own provider accounts (any
  OpenAI-compatible endpoint, local or hosted). The server stores them with their API keys
  encrypted under its own key, writes them into the sidecar's providers file and has it
  reload, so changes apply without interrupting conversations
  ([agent-providers.md](agent-providers.md#provider-accounts)). An egress policy per scope
  (workspaces' accounts, personal ones) limits which addresses the server and the sidecar
  may connect to for them, checked on every connection
  ([install.md](install.md#where-provider-accounts-may-connect)).
- **Knowledge base:** Markdown files in git, one repository per space, in the space's
  directory: the files are its working tree, so agents can read them directly, and every
  change is a commit with its author, person or agent. `internal/knowledge` runs git in
  pure Go (go-git, no git binary, so no hook or repository config ever runs), over the
  space's `os.Root`, and never on a caller's raw path. Large files (over 50 MiB by
  default), audio and video, and heavy binaries (archives, disk images, design files) are
  unversioned: git records a Git LFS pointer to them,
  and their bytes stay beside it in the space's `.git/lfs/objects/`, only the current
  version of each, so they don't bloat the history; a versioned file that becomes
  unversioned has its older versions taken out of history too, by a rewrite of the
  space's commits ([api.md](api.md#unversioned-files)). See [api.md](api.md#knowledge-base).
- **Search:** `internal/search` indexes every space's documents in SQLite: FTS5 full-text
  search (scripts written without spaces, such as Chinese and Thai, indexed a character at
  a time), and, where a workspace has chosen an embeddings model (from one of its provider
  accounts), vectors of their passages, compared in Go since the pure-Go driver can't load
  `sqlite-vec`, from an in-memory cache of them (bounded by `-search-vector-cache`, sound
  since one server owns the data directory) that every write to them updates; the two
  rankings are fused. Git stays the truth: the index records the
  commit of `main` it reflects per space and catches up from the diff, after every change
  and before every search. Embeddings are made in the background, and only of spaces whose
  owner or editors consented to that very endpoint, so an admin can't route a space's text
  elsewhere. A search only ever looks in spaces the caller can read. See
  [api.md](api.md#search). Board cards (title, body, comments) have a full-text index of
  their own, searched apart from documents and never embedded; it follows the boards by
  a queue that database triggers fill with every card whose text or thread changed, so
  whatever wrote them, a search is right ([api.md](api.md#searching-cards)). So do the text
  values of app records, only those the caller may read by their roles in each app
  ([api.md](api.md#searching-app-records)), and a person's own conversations, from the
  agent runtime's transcript ([api.md](api.md#searching-conversations)).
- **Boards:** backlog → todo → doing → review → done. Cards have an assignee (person or
  agent), links to docs and conversations, and a blocked reason.
- **Personal apps:** saved [A2UI](https://a2ui.org/) screens built from a fixed component
  catalog, a data collection with a schema, and actions served by Go (add, change or
  delete a row; a delete is recoverable, and each row keeps a revision and its history, so a stale edit is refused instead of overwriting a newer one). No generated code runs. `internal/apps` checks every saved screen
  strictly against the catalog, and every row against the schema; the browser's renderer
  checks again. An icon is SVG path data the agent writes, accepted only as path commands and
  numbers and drawn by the web app into an `<svg>` it makes itself, never as markup or an
  address ([api.md](api.md#the-definition)). An app lives in a space, so its access and its rows' are the space's, and
  sharing an app is moving it to a shared space. Inside that boundary, app roles (builder,
  publisher, user, data manager, reader, resolved by `access.Checker`) only narrow what the
  space allows: a user can submit records without being able to redesign the app, and
  reads all records or only their own, in every way records are read (screens, summaries,
  counts, the agent's tools). An app without roles behaves as before. An app may also offer the platform a few **contributions** in named, versioned slots (widgets for people's Home page, a pin for the navigation): plain data, checked strictly, that grants no access of its own. Each person places the ones they want, a shared space's publishers may set defaults for its members, and a widget draws through the app's own surface and actions with the viewer's access; one that fails, or whose app goes away, is isolated or disappears and can't break the shell ([api.md](api.md#home-widgets-and-pins)). The agent builds and changes apps with
  its app tools, and the conversation shows each app it saved there, launched from the
  server like on the Apps page (never from the tool call's arguments), with a link to it.
  An insert or update may declare **effects** from a fixed registry (a card, a document, a route),
  which a transactional outbox carries out: the record's change and its entries commit
  together, a worker in the server does them as the user who acted with that user's access at that
  time, once however often a request is repeated, with a failed one shown and retried on purpose
  ([api.md](api.md#effects-of-actions)).
  See [api.md](api.md#personal-apps). Sandboxed code apps may come later,
  enabled by an admin.
- **Integrations:** people's own agents (Claude Code, Codex, any MCP client) use the
  knowledge base, boards and apps through the MCP endpoint, `/mcp`, with the conversation
  agent's tools, or the knowledge base and boards through the REST API. They authenticate
  with a personal access token: one user's in one workspace, with exactly their access,
  checked on every request, and never good for the account itself ([mcp.md](mcp.md),
  [api.md](api.md#access-tokens)). The routes that read an app and its rows, and `POST …/actions`, take a token too,
  with a budget of actions; building, publishing, roles, the offline download and uploads stay
  behind a session.
- **Conversation notes:** a person can add a note to a conversation that stays in its history
  for the people who read it but is never part of the model's context. A note is a passive
  `taskto.note` entry the sidecar writes (`kind` and `data` only, no `model`), so Pi Durable
  appends it at once, even in the middle of a run; the messages view, the CLI hand-off and the
  conversation search, which read the model's context, never contain it
  ([api.md](api.md#post-apiworkspacesworkspaceconversationsconversationnotes)).
- **CLI workers:** a conversation can run on the official Claude Code, Codex or Grok Build
  CLI, signed in with the user's own subscription. Go runs the CLIs: in a worker container
  where each user is a Unix user of their own, or, on a one-user install, in the server
  itself. The sidecar records each turn in the conversation through a replay-safe tool
  ([cli-workers.md](cli-workers.md)).
- **Agent browser:** a Chromium per user, run in the CLI worker as the user's own Unix user,
  with a profile kept between runs. The agent drives it over a DevTools pipe only the worker
  holds. The user watches it, or takes over for a login, in a live view in the page (VNC
  through the Go server). A clean, profile-less headless browser is for testing apps
  ([agent-browser.md](agent-browser.md)).
- **Install:** `docker compose up` (Go binary + agent sidecar + optional Ollama); see [install.md](install.md).

## Previews of uploaded files

The knowledge base shows a Word file (`.docx`) on its page, formatted and read-only, instead of
only a Download link. It is drawn in the browser, not on the server: the bytes are the same
`?raw=true` download a person could make (the server changes nothing, and an older version in the
history is asked for with its `at` commit), and the library that reads them (`docx-preview`, with
`jszip`) is a separate chunk loaded when the first such file is opened (the service worker does not
precache it). A file is previewed only if it is within 20 MB, expands to no more than 100 MB over
at most 2000 parts (the archive is read through once, stopping at the budget, before the renderer
sees it), and the renderer does not draw HTML embedded in the archive. The preview of a version
is drawn again when the file's version changes, and a download that outlives the person who
asked for it is dropped. The alternative, converting
with an office suite on the server, would put a very large program that parses untrusted files
into the image, with its own sandbox, CPU per view and a cache with access rules; the browser has
none of that to keep, and nothing leaves it.

The rendered document never enters the app's DOM. It is written into an `<iframe>` whose `sandbox`
is `allow-same-origin` alone (no scripts, forms, popups or navigation), whose own policy
(`default-src 'none'`, inline styles, `data:` and `blob:` images, `data:` fonts) lets it fetch
nothing, and in which a click on a link opens only an `http` or `https` address, in a new tab with
no opener. `allow-same-origin` without `allow-scripts` is what lets the app write into the frame
while nothing inside can run. Files over 20 MB, and files the library can't read, show a notice
and keep the Download link. The code is `web/src/knowledge/preview/`; other formats join it by
adding a renderer.

## Integrations with outside services

An app can't call an outside service, hold a secret or receive a webhook by itself, and the
server never lets it: a workspace's admin connects an account at a provider, and an app asks for
a **slot** of a kind (today `payments`) that the admin binds to that account. Everything that
touches the provider is Go code in `internal/integrations`, a closed registry (Stripe, with Square
connectable but not yet able to make links) with fixed hosts and a fixed set of operations. A
definition has no place for a URL, a header or a key. The guide for admins and app builders is
[integrations.md](integrations.md); this is the shape of it.

- **The account** is a row of the workspace with its secrets sealed by `server.key`, write-only
  through the API. Its limits (the largest payment, the currencies) and the spaces that may use
  it are the admin's. Calls leave through an egress policy of their own (`-integration-egress`,
  `public` by default) and the adapter's host pinning.
- **The approval** ties an app to an account. The server works out a **contract** from the app's
  draft (what it sends out, which fields a payment may write, and with what), an admin approves
  its hash through a queue that shows nothing of the app's records, and publish, rollback and
  every run check it again. A copy of an app, or an installed template, has no approval.
- **Out:** the `create_payment_link` effect runs in the app's outbox as the user who acted and
  keeps a reference per payment link, written ahead of the call, so a crash or a timeout can
  never lead to a second payable link; an unknown outcome waits for an admin.
- **In:** `POST /hooks/<public id>` verifies the provider's signature over the raw bytes, stores
  the normalized event once and answers; a worker applies it later through the **system writer**
  (`internal/apps/system.go`), the one path that changes a record without a user, bounded by the
  approved contract and checked at the moment of the write. Its history entry names the
  integration, not a person.
- **Not here:** card data (hosted checkout only), raw webhook bodies, refunds made from TaskTo,
  and any call a model composes.

## Live updates

`internal/live` is the building block for anything several people see change together (a whiteboard, cursors, presence). It has no routes of its own: a feature mounts them.

- **SSE down, JSON POST up.** A feature's `GET …/spaces/{space}/<resource>/{id}/live` is wrapped in `access.Checker.RequireSpace(access.Read, …)` and calls `Hub.Serve`; its writes are ordinary `POST`s wrapped in `RequireSpace(access.Write, …)`, so the CSRF guard and the access checks apply to them as to every other state change, and call `Hub.Publish`. The GET only reads. A WebSocket is reconsidered only if cursor traffic proves too heavy over POST.
- **One process.** The hub is in memory: exactly one server process uses a data directory, so there is no fan-out across processes.
- **Topics by space generation.** A `Topic` is a kind, a space, the space's `Generation` and a resource. A space that is removed and later takes the same ID has another generation, so it never receives the old topic's events, and a stream opened for the old one fails its next check.
- **Order and gaps.** Each topic numbers its events (`seq`, from 1 while someone is subscribed). A stream starts with a `ready` event carrying the topic's current number, so a client knows where it joined and sees a gap as a missing number.
- **A slow reader is dropped, not waited for.** Each subscriber has a bounded buffer (at least two events). When it overflows, the hub discards what is queued, keeps the `ready` event if the client has not read it yet, queues a `resync` after it and ends that stream; the client reloads its state and reconnects. Nobody else waits. The stream keeps its slot of the per-person cap until its handler has returned, however it ended (an overflow, a revocation or a cancelled session included), because `Serve` releases it only when it returns.
- **Access is checked on connect, at every write and while idle.** `Serve` runs the feature's `recheck` after the subscription is registered, every 30 seconds, and inside the one function every write goes through (`eventWriter.deliver`): under the stream's write lock it refuses a stream whose context ended or that was revoked, runs `recheck` for an event that carries data, arms the write deadline after the check (a slow check does not use up the write's budget), checks the context and the revocation once more, and only then writes. An access check that runs after a removal committed therefore denies, whatever ended the access (a removal from the workspace, a role the feature's resource check denies, a replaced space); a feature whose resource needs more than space access passes a stricter `recheck`, and checks the same right in its POSTs. A removal with a hook revokes at once: a removed space member's streams end through `access.Checker.OnMemberRemoved` (called after the removal commits) and `Hub.EndUserIn`, and a revocation detaches every matching stream and closes its revoked signal before any hook runs, so each of them refuses a write already taken even while the hook waits on another's write lock; a hook that returns has ended the stream's context, which also ends a `recheck` in progress, cut a write in flight short with a past deadline, and waited for the write lock, so no write starts after the hook returned; the hub also discards the queue. Signing out, disabling the account or an expired session ends the stream's context through `auth.Service.WatchSession`, and the writer refuses writes once it ends. A stream's slot is released last, after its writer's cleanup. A change of access with no hook is seen by the next write's check or the periodic one; a write that passed its check before the change committed was authorized when it began.
- **Caps and shutdown.** A person has at most 16 live streams across topics (`too_many_streams`, counted apart from the conversations' own). On shutdown `Hub.EndStreams` ends every stream and refuses new ones (`live_closed`), so the drain finishes.
- **An event** is a type, no more than 64 characters and never `ready` or `resync`, and JSON data of at most 64 KiB. Anything else is refused by `Publish`, which copies the data it validated, so a caller may reuse its bytes.

## Scheduled work

Timed work for people (a card's due-date reminder, an app's scheduled
action; an integration's polling has a worker of its own, [integrations](integrations.md#reconciliation-polling)) runs on one general scheduler, `internal/schedule`, not on tickers of its own. It is a
library and a worker in the server process (the data directory has one server, so one worker, and
no extra process); it has no feature of its own.

- **Jobs are rows.** A job in `scheduled_jobs` has a kind, the user it runs for, its workspace (and
  space), a small JSON payload of IDs and settings (no secret, no copy of what people wrote), when
  it is due, and optionally a fixed interval for one that repeats. The kinds are a closed list: the
  Go code of each feature registers a handler for its kind at startup, before the worker starts,
  and nothing else can name one. A **dedupe key**, unique per owner and kind, names what the job is
  for, so the same reminder can't be scheduled twice; `Schedule` leaves an existing one alone,
  `Reschedule` replaces it (and re-arms one that already ran), `Cancel` removes it, `CancelMatching`
  removes those of a kind whose key matches a pattern, and `Relocate` moves a pending or running
  job to another space of its workspace without a new revision. All take the caller's transaction,
  so a job is made, moved or removed in the same commit as the change that needs it, and a
  rolled-back change leaves no job.
- **At least once, safe across restarts.** The worker takes a due job with a lease and runs its
  handler with a time limit. A server that dies leaves the job running until the lease ends, and
  the next server (or the same one, restarted) takes it again, so a handler runs more than once
  for an occurrence in that case and must be idempotent. `Job.Occurrence()` is the same on every
  try of an occurrence, and is the key of whatever a handler makes (a notification's dedupe key, a
  card's origin); it includes the job's revision, which a reschedule raises, so work scheduled
  again for the same time is a new occurrence. Each claim raises the job's lease token, and so does
  a reschedule: a worker that was too slow, or whose job was changed or cancelled under it, can't
  finish it for anyone else. The outcome of a try is also dropped if its lease had ended, checked
  with the token and the state under the write lock; the expired lease is recovered like a crashed
  worker's. A handler must honour its context: a try that returns after its time limit has failed,
  even with `nil`, and stopping the server waits a bounded time for a handler that ignores it.
  Failures are tried again with growing waits up to a bound that counts crashed tries too; a job
  that used them all has failed and stays listed with its last error. A recurring job runs once
  when due however many occurrences passed while the server was off, and keeps to its grid.
- **Access is checked when the job runs, as its owner.** What a person could do when they
  scheduled something says nothing about what they may do later. Before a handler is called the
  worker checks, through `access.Checker`, that the owner can still read the job's space (or, with
  none, belongs to its workspace); otherwise the job fails and the handler is never called. That
  is a floor: each handler checks the rights its work needs, as close to the write as it can, and
  stops the job by returning `schedule.ErrAccessLost`. Nothing runs with the authority of whoever
  scheduled it or of the server, which is the rule the apps' effect outbox follows too.
- **Bounded.** A person has at most 1000 jobs waiting or running, counted at every change into
  those states (a finished job scheduled again too) by the statement that writes, so concurrent
  callers can't pass it; a payload is at most 16 KiB; a recurring job repeats at most once a minute;
  a job is due between the years 2000 and 8999 (its stored time sorts as text, and the database
  refuses any other); and finished jobs are purged after a week.
- **Cheap to poll.** Partial indexes order the pending jobs by when they are due and the running
  ones by when their lease ends, so a poll reads the first row of each and a queue of finished jobs
  costs nothing (`EXPLAIN QUERY PLAN` is checked in a test). The worker runs bounded batches, so
  cleanup gets a turn however long the queue is, and fails a row whose time can't be read instead
  of stopping on it.
- **Wall-clock calendars.** `schedule.Calendar` describes a local time or an interval of at least
  an hour, an optional set of weekdays, and an IANA time zone. `Next` returns the next UTC
  occurrence: a time in a spring-forward gap runs at the first instant after the gap, and a time
  in a fall-back overlap runs once at its first instant. A feature stores the calendar with its own
  settings, schedules a one-off job at `Calendar.Next`, and reschedules the following occurrence
  in the handler's transaction after confirming the job is still current, as
  `notifications.digestNext` does. The database job remains a single due time, so retries retain
  the same occurrence and idempotency key.
- **First user: due-date reminders.** A card with a due date and a person as its assignee has two
  jobs for that person, `card.due-reminder` and `card.overdue`, keyed by the card. They are made,
  moved and cancelled in the transaction of the card change that needs them, and their handlers
  make a `card_due` and a `card_overdue` notification once per person, card, kind and date
  ([api.md](api.md#due-date-reminders)). Every path that changes a card shares the one scheduler
  (the command gives it to the HTTP server too), archiving a board or space follows through a hook
  (`access.Checker.OnArchiveChange`), the schema drops a person's jobs when they lose access to
  the space or workspace (`0038`) and hooks in the transaction that adds them back make them again
  (`access.Checker.OnMemberAdded` for a shared space, `auth.Service.OnMemberAdded` for a workspace;
  neither package knows the boards), and the server schedules the jobs of cards dated before
  reminders existed when it starts (`boards.Service.ReconcileReminders`). Their time is 08:00 on the
  wall clock of the assignee's time zone (`users.time_zone`, `0047`), worked out from the card's date
  with `schedule.LocalHour`; a change of a person's zone reaches the jobs through a hook in the
  transaction that stores it (`auth.Service.OnTimeZoneChanged`: `boards.Service.SyncTimeZone` and
  `notifications.Service.SyncTimeZone`), so auth knows no feature that is timed by it.
- **Second user: reminders of app records.** A record of an app whose live release declares
  [reminders](api.md#reminders-of-records) has one `app.record-reminder` job for each reminder, for
  the person who created it, keyed by app, record and reminder name and holding only those IDs. The
  jobs are made, moved and cancelled in the transaction of every write of a record
  (`apps.Service.syncRecordReminders`: an action, a system write, a restore) and of what changes
  who is reminded or when (`syncAppReminders`: a release, a rollback, an app archived or moved, an
  app's roles changed (`access.Checker.OnAppGrantsChanged`) and an account enabled again
  (`auth.Service.OnMemberEnabled`); the same hooks as the boards' for a space archived, a member
  added and a time zone changed). A sync first cancels the jobs of reminders the live release no
  longer declares (`Service.CancelMatching`), so they never hold the slot of their replacements, and
  an app that moves has the space of its unchanged jobs changed in place (`Service.Relocate`, which
  keeps the occurrence: a job that was claimed or awaits a retry is told once). The wall-clock time
  is resolved with `schedule.LocalTime`, hour and minute together. The handler
  (`apps.Service.remindRecord`) trusts none of it: it asks `Holds`, the creator's access to the
  space the job is in now and the app, the live release, the record and its condition and the time
  again, and makes an `app_reminder` notification keyed by `Job.Occurrence()`. The notification's text is read
  from the record when the inbox is (`notifications.ReminderSource`, which `apps.Service`
  implements, because `internal/notifications` can't import `internal/apps`).

## Agent safety

Agents read text other people wrote, so any of them can carry a prompt injection. One
injection must not become root, or read every secret of a team
([agent-safety.md](agent-safety.md)):

- **No root, no sudo.** The sidecar, the CLI workers' CLIs and every command they run are
  never root and can't escalate: the server and the worker check this at start and refuse,
  and the CLIs run with `no_new_privs`, as a Unix user of their own in `container` mode.
- **No secrets in agents' environments.** A CLI gets a clean environment and only its user's
  own login. Secrets stay with the server, which uses them in tools it runs for the
  conversation's owner.
- **Permission prompts.** A CLI asks before it runs a command or edits a file, through a
  hook that reaches the conversation's owner in the web app. Only the owner turns this off,
  for one conversation.
