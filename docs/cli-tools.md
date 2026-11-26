# taskto's tools in CLI conversations

A conversation that runs on Claude Code, Codex or Grok Build ([cli-workers.md](cli-workers.md))
gets the tools the built-in agent has: the knowledge base, the boards, the apps
([agent-tools.md](agent-tools.md)) and the agent browser ([agent-browser.md](agent-browser.md)).
This note is the design.

The design in short:

- the server serves the tools to a turn's CLI over MCP, on the per-turn proxy's listener, not the
  public one;
- the CLI's only credential for them is a token for that one turn, in the pattern of the proxy's
  ([account-switching.md](account-switching.md#the-proxy));
- every call acts for the conversation's owner, in the conversation's workspace, with exactly
  their access, looked up again on every request;
- the worker writes each CLI's MCP settings before every run, as it writes its hooks;
- the permission prompts ([agent-safety.md](agent-safety.md)) cover the tools: the read-only ones
  run without asking, the others ask the owner, unless the owner allowed everything in that
  conversation.

## Choices

### MCP, served by the server

Every CLI speaks MCP over Streamable HTTP, and the server already serves its tools over it to
people's own agents ([mcp.md](mcp.md)), through the same code as the sidecar's
(`agenttools.Service.Call`). A turn gets a second endpoint of the same server, with another way
of finding the caller.

Two other shapes were considered:

- **The access-token endpoint, `/mcp`, with a token made for the turn.** It is on the public
  listener, which the worker's network can't reach, by design
  ([cli-workers.md](cli-workers.md#the-workers-network)). An access token is also the wrong kind of
  credential here: a row in the database that lives until it expires or is revoked, not tied to a
  conversation (archiving the conversation wouldn't stop it), its writes marked as the token's,
  and without the browser tools.
- **A stdio MCP server in the worker** (`taskto` run as the user, started by the CLI). It would
  still have to call the server for every tool, through the same relay, with the same token. It
  adds a process per turn and nothing else.

### The endpoint

- **Where.** `/cli-mcp` on the listener of the proxy for shared accounts (`-cli-proxy-listen`).
  That listener is already where the worker's CLIs reach the server and nothing else is:
  `127.0.0.1` in local mode, and in container mode a Unix socket in the volume the two containers
  share, which the worker relays from `127.0.0.1:8081`. The CLI's URL is the proxy's
  (`-cli-proxy-url`) with `/cli-mcp`. No new port, route out of the worker, or firewall rule.
- **Protocol.** As `/mcp`: stateless, one JSON-RPC message per `POST`, answered with JSON, tools
  only. The same bounds (a reply of at most 64 KiB, arguments of at most 1 MiB).
- **Tools.** The knowledge-base, board and app tools of [agent-tools.md](agent-tools.md), and,
  while the server runs with `-browser`, the browser tools, which `/mcp` leaves out
  (`conversation_only`): a turn is in a conversation, where the hand-over happens.
- **Without a proxy listener** (container mode without `-cli-proxy-listen`), turns get no
  taskto tools, and the server says so in its log at start.

### The token

Minted by the server for each worker turn it starts, whatever account it runs on:

| Claim | |
|---|---|
| user, conversation | whom it acts for |
| turn | the worker turn (`c<conversation>-<turn>`, or a switch's `…-s1`) |
| nonce | tells two tokens of one turn apart: a replay gets a new one, and the old one stops |
| expiry | two hours, as the proxy's: the longest turn and a wait for a place |

- It is `ttm1.<claims>.<MAC>`, signed with the server key (`serverkey.MAC`) under a purpose of
  its own, so a proxy token isn't a tools token or the other way round. It is stateless, so a
  server restart in the middle of a turn doesn't break it.
- **It stops with its turn**, two ways:
  - the worker must still be running the turn the token names: the server asks it on every
    request (in local mode it is in the same process; in container mode it is one call on the
    worker's socket). A turn the worker finished, or no longer knows, refuses the token. The
    worker outlives a restart of the server, and a restart of the worker stops every CLI, so
    neither leaves a window;
  - when the server sees the turn's `done` event, or starts the same turn again (a replay), it
    refuses the turn's earlier tokens, by their nonce, as the proxy does. This list is in memory;
    it covers the replay, which the worker's answer can't tell apart.
- **Every request is checked again**: the signature, the expiry, the nonce list and the
  worker's turn; then the conversation must exist, not be archived, still be the claimed user's, and that user must still
  be a member of its workspace (`conversations.PrincipalOf`). The caller is built from what the
  database says now: the conversation's workspace and owner. The token never names a workspace,
  and nothing the CLI sends says whom a call is for.
- **Each tool** then checks every space it names with `access.Checker` for that user, in that
  workspace, as the sidecar's calls do. A space they can't read, or of another workspace, is
  `not found`.
- **What the token is worth.** A command the model runs can read it (spike risk 3), as it can
  read the proxy's token; and in `ask`, a command runs only once the owner allowed it. It lets
  that command call taskto's tools as the conversation's owner, in that conversation, while the
  turn runs, without the prompt a write would get through the CLI: what the owner's agent can do
  anyway, and what that allowed command could do through the CLI's own login. It never reaches
  another user's data, and it stops when the turn ends, the conversation is archived or the owner
  leaves the workspace.
- **Changes are the owner's, made by the agent**, as the sidecar's: a document's commit has the
  owner as author and `taskto.ai agent` as committer; board and app changes are
  `Actor{Agent: true}`.

### The CLIs' settings

The server puts the endpoint's URL, the token and the read-only tools' names in the turn's
spec. The helper, as the user, writes them into the CLI's settings in the account's config
directory before every run, like the permission hook, so a command that changed them doesn't
change the next run. The token goes in the CLI's environment, `TASKTO_TOOLS_TOKEN`, and the
settings name the variable, not its value:

| CLI | Settings |
|---|---|
| Claude Code | `taskto-mcp.json` (`mcpServers.taskto`: `type: http`, the URL, `Authorization: Bearer ${TASKTO_TOOLS_TOKEN}`), passed with `--mcp-config` and `--strict-mcp-config` |
| Codex | `[mcp_servers.taskto]` in `config.toml`: `url`, `bearer_token_env_var = "TASKTO_TOOLS_TOKEN"`, `default_tools_approval_mode = "approve"` |
| Grok Build | `[mcp_servers.taskto]` in `config.toml`: `url`, `headers = { Authorization = "Bearer ${TASKTO_TOOLS_TOKEN}" }` |

- Checked with the pinned Claude Code (2.1.311) and Codex (0.169.0) against a scripted model:
  both expand the variable, list the tools and call them, and both run their permission hook
  before an MCP tool (Claude Code's `PermissionRequest`, Codex's `PreToolUse`), naming it
  `mcp__taskto__<tool>`.
- **Codex's own approval of MCP tools** refuses a tool without `readOnlyHint` when
  `approval_policy` is `never` ("MCP tool call requires approval"). Its hook is our gate, as for
  every other tool, so the server's tools are set to `approve`: the hook still runs first, and a
  denial there blocks the call (checked).
- **`--strict-mcp-config`** is passed on every Claude Code turn, with tools or without: only the
  servers in our file count, never one from the user's settings or a `.mcp.json` a command wrote
  in the working directory. A turn without tools gets no file, and an old one is removed.
- **Grok Build** reads the settings: with them, the pinned version's (1.0.89) `grok mcp doctor`,
  in a Linux container, expands the header, connects with the token and lists the tools. In a
  turn it names an MCP tool `taskto__<tool>` and may call it through its `use_tool` meta-tool.
  That is from its documentation; the gateway can't sign Grok Build in, so a turn's calls are
  unverified, like its hook ([agent-safety.md](agent-safety.md#open-questions)). If it differs,
  it fails safe: a name the policy doesn't recognise asks the owner (the hook fails closed), and
  a header it doesn't expand is refused by the endpoint.
- **Grok Build also reads a project's `.grok/config.toml`**, without a trust, and its
  `[mcp_servers]` win over the user's. A command the owner allowed could write one in the working
  directory that points `taskto` elsewhere; it would learn nothing that command couldn't read
  already (the token is in its environment), and every call still goes through the hook.

### Permissions

The prompts of [agent-safety.md](agent-safety.md#3-permission-prompts-unless-the-user-turns-them-off)
cover taskto's tools, with no setting of their own:

- **`ask`**: a tool that changes nothing (`read_only` in `tools.json`: `list_spaces`,
  `read_doc`, `list_cards`, `browser_snapshot`, …) runs without asking, as reading the working
  directory does, and so does `browser_help` ([the hand-over](#the-browsers-hand-over)). Every
  other tool (`write_doc`, `create_card`, `browser_open`, …) asks the owner, with the tool and
  its arguments.
- **`allow_all`**: nothing asks.
- The worker recognises a read-only taskto tool by the names the server put in the spec, in each
  CLI's form: `mcp__taskto__<tool>` (Claude Code, Codex), `taskto__<tool>` (Grok Build), and
  Grok Build's `use_tool` with `tool_name` `taskto__<tool>` and its input inline. Its
  `search_tool`, which only lists the tools, is read-only too. Anything else asks.
- This holds for both hook events: Claude Code asks about every MCP tool, read-only or not
  (checked), so its `PermissionRequest` for a read-only taskto tool is answered at once.

### The browser's hand-over

The built-in agent's `browser_help` call shows a card in the conversation (Take over, Done, Not
now). A CLI turn is one `cli_turn` call, so the card needs to come from it:

- the sidecar sees the CLI's `browser_help` call in the turn's tool events (each CLI's form, as
  above: `mcp__taskto__browser_help`, Codex's `taskto.browser_help` with its arguments, which
  the worker now passes on, and Grok Build's `use_tool`), and keeps the latest one's reason and
  message in the `cli_turn` result's details, as `help`;
- the web app shows the card from there, as from the built-in agent's call: from what the model
  asked, whatever the call's result;
- Done or Not now sends a message, which starts the next CLI turn, as before.

An app the CLI saves with taskto's `create_app` or `update_app` is shown the same way, so the app
builder's preview fills in after a CLI turn: the worker reads the app's `id` and `space_id` from
the whole of a finished, successful call's answer (the event's `output` is cut, so it can't be read
from there; each CLI's form of the name is accepted, as above) and sends them in the tool event's
`saved`; the sidecar keeps the turn's saved apps, last 20, in the `cli_turn` result's details, as
`apps`, and the web app launches or links the last one. What the CLI's own answer said plays no
part.

The CLI is told, by the tool's result and the endpoint's instructions, to end its turn after
`browser_help`. It doesn't ask the owner's permission first, unlike the other browser tools
that aren't read-only: it changes nothing but which tab is in front, and asks the user to act,
so a prompt before it would ask them twice.

## Isolation

- **One user's token never reaches another's turn.** It is minted for the user, conversation and
  turn the server resolved; the helper writes it into that user's own config directory (mode
  700, their Unix user's in container mode) and that turn's environment. A second user's turn at
  the same time gets its own.
- **One user's token never reaches another's data.** Whom a call acts for comes from the
  conversation in the token, through the database, never from the request.
- **Local mode** has no OS boundary between users, which is why it is for one user only
  ([cli-workers.md](cli-workers.md#worker-modes)); nothing here changes that.

## Tests

- **Tokens**: a forged, expired or proxy token is refused; a token of a turn that ended, of a turn
  the worker doesn't know (as after a restart, with the server's list empty), or of a replaced
  replay, is refused; a conversation archived, its owner gone from the workspace, or a
  token whose user isn't the owner, is refused.
- **Cross-user**: one user's turn, through its token, reaches nothing of another user's spaces
  or the owner's other workspace; two users' turns get different tokens in their own config
  directories.
- **Settings**: each CLI's files, arguments and environment, with and without tools; a turn
  without tools leaves no file; the worker's API never returns a token.
- **Permissions**: the read-only tools pass in each CLI's form, the others ask, `allow_all`
  allows.
- **End to end**: the whole server with the sidecar and a fake CLI, which reads its MCP settings
  as each real CLI does, asks its hook, and calls a tool through the endpoint: a read, a write the
  owner allows, and a token from the ended turn refused.
- **Real CLIs**: an opt-in test runs the installed Claude Code and Codex against a scripted model
  (no account, no network), and checks that they read the settings, ask their hook, and call a
  tool with the token.

## Slices

1. This note.
2. **The worker**: the turn's tools in its spec, each CLI's settings and environment, the
   permission policy for taskto's tools, and the fake CLIs' MCP calls.
3. **The server**: the token, the endpoint on the proxy's listener, starting turns with tools,
   the cross-user and end-to-end tests, and the docs.
4. **The hand-over**: the sidecar's and the web app's card for a CLI's `browser_help`, and
   Codex's MCP arguments in its tool events.

## Open questions

Defaults taken, still open to change:

1. **Read-only tools don't ask**, as reading the working directory doesn't. Reading the
   knowledge base sends what it reads to the CLI's provider, which the conversation's owner chose.
2. **The browser tools follow `read_only` too**: `browser_tabs` and `browser_snapshot` don't ask,
   nor does `browser_help` (above); the others do. The built-in agent asks for none of them.
3. **Changes are marked as the agent's**, not as the CLI's. Naming the CLI in the history
   (`Claude Code (taskto.ai agent)`) can come later.
4. **The token can't be revoked before it expires** after a server restart, as the proxy's.
   Archiving the conversation or removing the owner from the workspace stops it at once.
