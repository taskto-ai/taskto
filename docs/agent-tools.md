# The agent's tools: knowledge base, boards and apps

In a conversation, the agent searches, reads and writes the knowledge base, works the
boards and builds personal apps with tools. The tools run in the agent sidecar (`agent/src/taskto-tools.ts`), but
the work is the Go server's (`internal/agenttools`): each tool call is a request back into
the server, which does it for the conversation's owner, with exactly their access. The
board tools' contract is in [boards-agent-tools.md](boards-agent-tools.md).

People's own agents get the same tools over MCP ([mcp.md](mcp.md)), for an access token's
user in its workspace, through the same code (`agenttools.Service.Call`): only how the
caller is found, and who a change is marked as made by, differ. Conversations that run on
Claude Code, Codex or Grok Build get them over MCP too, for the conversation's owner, with a
token for each turn, the browser's among them ([cli-tools.md](cli-tools.md)).

## The definitions

Each tool's name, description and argument schema (JSON Schema) is defined once, in
`internal/agenttools/tools.json`, with `read_only` for the tools that change nothing. The
MCP server lists them from there (`agenttools.Definitions`), read-only ones with
`readOnlyHint`. The sidecar imports a copy, `agent/src/taskto-tools.json`, which
`go generate ./internal/agenttools` writes and a test keeps identical; it runs read-only
tools again after a crash (`replay: 'safe'`), and never a write. Change a tool in
`tools.json`, then run `go generate`.

## The channel

- **An owner-only socket, for the sidecar only.** The supervisor (`agentrt`,
  `SupervisorConfig.Tools`) serves the tools on `<data>/agent/run/tools.sock`, next to the
  sidecar's own socket, in the same mode-700 directory, the socket itself mode 600. It is
  opened after the agent runtime's database lock is taken (so a socket left there is a
  crashed server's, and is replaced) and closed after the last sidecar has exited. The
  sidecar learns its path from `TASKTO_AGENT_TOOLS_SOCKET`; without it, it has no
  knowledge-base or board tools. Nothing of this listens on the network, and it has no
  sessions or cookies.
- **A capability per conversation.** The server issues each conversation a capability,
  `ttc1.<conversation ID>.<MAC>`, an HMAC-SHA256 under a key derived from the server key
  (`serverkey.Key.MAC`) over the conversation API's ID and the agent runtime's, so it
  never fits another conversation that came to have the same ID. Every message sent to the
  conversation carries it to the sidecar (`capability` on `POST /conversations/:id/submit`),
  which keeps the newest in a conversation document of its own (`taskto.tool-access`):
  not part of the conversation's view, so it never reaches a browser; durable, so a run
  resumed after a restart still has it; and empty in a fork, a conversation the server
  never issued one for. A conversation from before this, or after the server key changed,
  gets it with its next message.
- **Every call is checked again.** A call is `POST /tools/<name>` with the tool's
  arguments as a JSON object and `Authorization: Bearer <capability>`. The server looks
  the conversation up: it must exist, not be archived, and its owner must still be a
  member of its workspace (`403` otherwise); then it checks the MAC (`401` otherwise). Each
  space a call names is checked with `access.Checker` for the owner, now, and must be in
  the conversation's workspace: a space they can't read, of another workspace, or that
  doesn't exist is `404 not found` alike. So losing a space's membership, the space being
  archived, the conversation being archived or the owner leaving the workspace takes
  effect from the next call. The capability only says which conversation a call is for;
  the sidecar never says whom it is for.
- **Changes are the owner's, made by the agent.** A document written is committed with
  the owner as author and the agent (`TaskTo.AI agent`) as committer
  (`knowledge.Author.Agent`), so the history shows both. Board changes are
  `boards.Actor{UserID: owner, Agent: true}`: cards, edits and comments say the owner made
  them through the agent. Apps the agent creates or changes say so the same way
  (`apps.Actor`). Over MCP, the committer is the access token instead
  (`<name> (access token)`), and board and app changes carry its name too (`via`). The
  agent builds and changes apps; it doesn't run their actions, which are for the people
  using them. It has exactly its user's roles in an app ([App roles](api.md#app-roles)): a
  user who may only submit records can't have it change the app (`403`), and one who reads
  only their own records gets only those from `list_app_rows` and `query_app_rows`, and totals of those alone. It has no tool to set roles,
  and nothing in a definition grants any.

Refusals are `4xx {"error": "…"}` with a message for the model: `400` invalid arguments,
`403` read-only (a viewer, an archived space or board), `404` not found, `409` a stale
version, a changed document or a full app, `413` too large. The sidecar turns them into error
results, `error: <message>`.

## The tools

| Tool | Arguments | Does |
|---|---|---|
| `list_spaces` | | The spaces the user can read in the conversation's workspace: `id`, `name`, `kind`, `access` (`read`/`write`), `archived`. |
| `search_docs` | `query`, `space_id`?, `limit`? (1–20, default 10), `offset`? | Search one space, or every space the user can read ([api.md](api.md#search)); semantic search only where its space consented. |
| `list_docs` | `space_id`, `folder`?, `after`?, `limit`? (1–200, default 100) | A space's documents and files, by path; `next` is the `after` of the next page. |
| `read_doc` | `space_id`, `path`, `offset`? | A Markdown document, 32 KiB at a time from `offset` (bytes); `next_offset` while there is more. `version` is what `write_doc` needs. |
| `write_doc` | `space_id`, `path`, `content`, `base_version`?, `message`? | Creates (no `base_version`) or replaces (the version read) a Markdown document. |
| `list_boards` … `set_blocked` | | The board tools of [boards-agent-tools.md](boards-agent-tools.md). |
| `app_catalog` | | What an app may be made of: the components and their props, the functions, the field types (time and choice included), screens and navigation, the rules, and the bundled example apps. |
| `list_apps` | `space_id`, `archived`? | A space's apps, without definitions; `archived: true` includes the offline ones. |
| `show_app` | `space_id`, `app_id` | An app with its draft definition, its `version`, and the live release (number and needs, not its definition) with `unpublished_changes`. |
| `list_app_rows` | `space_id`, `app_id` | An app's data: the rows the user may read (all, or only their own: [App roles](api.md#app-roles)), oldest first, each with its `id`, its `_revision` and the fields its live release's collection declares (deleted rows are not listed); an offline app's too. An attachment field (photos) holds a list of `{id, content_type, size, width, height}`: the agent sees what photos a row has, never their bytes, and can't upload or attach one ([Photo attachments](api.md#photo-attachments)). |
| `query_app_rows` | `space_id`, `app_id`, `state`?, `filters`?, `search`?, `sort`?, `dir`?, `limit`? (1–200, default 50), `cursor`?, `aggregates`? | A page of an app's rows with filters, search and an order, and `total` and `aggregates` over **all** the rows that match ([api.md](api.md#get-appsapprecordsquery) has the filters, the order and the paging): the rows the user may read, each as `list_app_rows` has them. `next_cursor` goes back as `cursor` with the same query for the next page; a page too long for one reply is made shorter, and the cursor continues after the last row sent. |
| `create_app` | `space_id`, `name`, `definition` | Saves a new app, as a draft, in a space the user may write, checked strictly against the catalog ([api.md](api.md#personal-apps)); it is launched once a person publishes it. |
| `update_app` | `space_id`, `app_id`, `version`, `name`?, `definition`?, `archived`? | Renames an app, replaces its **draft** definition (the user must hold the design right; the live release, which people use, is untouched until a person publishes the draft, [api.md](api.md#drafts-and-releases)), or takes it offline and back (the publish right). The agent has no tool to publish or roll back. |

Only Markdown documents (`.md`) are read or written; other files are uploads, which only
`list_docs` shows, with `versioned: false` for the unversioned ones (large files, audio
and video, archives and other heavy binaries: [api.md](api.md#unversioned-files)). Unknown arguments are refused, by the sidecar's schemas and again by
the server.

### The browser tools

These drive the conversation owner's own browser ([agent-browser.md](agent-browser.md)).

- **When they are offered.** The sidecar offers them only while the server runs with
  `-browser` (`TASKTO_AGENT_BROWSER=true` in its environment). They are `conversation_only` in
  `tools.json`, so the access tokens' MCP endpoint doesn't list or run them: the hand-over to
  the user happens in a conversation. The endpoint for CLI turns, which is in a conversation,
  offers them while `-browser` is on ([cli-tools.md](cli-tools.md)).
- **Which browser.** Each takes `browser`: `personal` (the default) or `testing`. The
  exception is `browser_help`, which is always the personal browser.

| Tool | Arguments | Does |
|---|---|---|
| `browser_tabs` | `browser`? | The open tabs: `id`, `title`, `url`, `agent` (whether the agent opened it). Doesn't start the browser. |
| `browser_open` | `url`, `browser`? | Opens an http(s) URL or `about:blank` in a new tab, starting the browser if needed. |
| `browser_navigate` | `tab`, `url`, `browser`? | Loads a URL in a tab. |
| `browser_snapshot` | `tab`, `browser`? | The page as text, with a ref (`e12`) for each link, button and field. Password values are never shown, and frames are named, not read. In the personal browser, a page with a password field comes with a `note` telling the model to call `browser_help` rather than ask for the password in the chat or submit the form. |
| `browser_click` | `tab`, `ref`, `browser`? | A real mouse click on the element. |
| `browser_type` | `tab`, `ref`, `text`, `submit`?, `browser`? | Replaces the field's text; `submit` presses Enter. |
| `browser_press` | `tab`, `key`, `browser`? | One of Enter, Tab, Escape, Backspace, Delete, Space, the arrows, PageUp, PageDown, Home, End. |
| `browser_close` | `tab`, `browser`? | Closes a tab the agent opened; the user's own are refused. |
| `browser_help` | `tab`, `reason` (`login`, `captcha`, `two_factor`, `other`), `message` | Brings the tab to the front, and answers `waiting_for_user`, telling the model to end its turn. |

The hand-over works like this:

1. The web app shows a card for a `browser_help` call, with the message and the live view,
   and the buttons Take over, Done and Not now.
2. Done or Not now sends a message to the conversation.
3. The agent goes on from that message, taking a new snapshot first.

A tool call is bounded at 60 s, so the tool doesn't wait for the user itself.

## Bounds

Every result is at most 64 KiB of JSON (`agenttools.MaxResult`), and the sidecar reads at
most 128 KiB of a reply:

- `read_doc` returns 32 KiB pieces, cut at a character boundary;
- `list_cards` cuts each card's body to 500 bytes, `show_card` the body to 16 KiB and each
  comment to 2 KiB (a cut text ends in ` […cut]`); a list that still doesn't fit is
  shortened with `"truncated": true`, a thread to its newest comments with
  `older_comments_omitted`; a page of `list_docs` to fewer paths, its `next` where it
  stopped;
- `search_docs` returns at most 20 results, each passage at most 1000 bytes;
- an app's definition is at most 48 KiB, so `show_app` returns one whole; `list_apps`
  and `list_app_rows` shorten a list that doesn't fit, with `"truncated": true`, and
  `query_app_rows` a page, whose `next_cursor` then continues after the last row sent;
- anything still larger is refused (`413`), so no reply is ever over the bound.

## Conversations on cards

A card links conversations by the conversation API's `id` (`"7"`), never the agent
runtime's. Since conversations are private, a link is checked when it is added: it must
be a conversation of the user adding it, or of the user the agent works for, in the
board's workspace (`internal/boards`, `conversations.Owns`). Whoever may edit the card may
keep or remove it. Following a link is the conversation API, which opens a conversation
only for its owner. Migration 0008 enforces the ID's form and corrects 0005's comment,
which said otherwise.

## Tests

`internal/agenttools/agenttools_test.go` checks capabilities (another conversation's MAC,
another key, none), that a conversation reaches nothing of another user's spaces or of its
owner's other workspace, that access is checked on every call (a viewer, an archived space,
a removed member, an archived conversation, the owner leaving the workspace), that writes
are the owner's made by the agent, and the bounds; `apps_test.go` does the same for the
app tools. `internal/agentrt/tools_it_test.go` runs the real sidecar against a stand-in
for the server's socket (what it sends, the capability kept across a restart), and `cmd/taskto/agenttools_it_test.go` the whole server
with the sidecar and its scripted model (`make agent-it`).
`internal/agenttools/definitions_test.go` checks every tool is defined once and the
sidecar's copy is current, and `internal/server/tokens_test.go` the tools over MCP with
access tokens, through the whole server.
