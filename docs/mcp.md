# Your own agents: MCP and access tokens

Claude Code, Codex and any other MCP client can use TaskTo.AI's knowledge base, boards and
personal apps: search, read and write documents, list, create, move and comment on cards,
and build apps and read their data. They get
the same tools as the conversation agent ([agent-tools.md](agent-tools.md)), and act as
you, with exactly your access, in one workspace.

Scripts can use the [REST API](api.md#access-tokens) with the same token: the knowledge base,
boards, reading apps and their rows, and running their actions (`POST …/apps/{app}/actions`, what
`run_app_action` does).

## 1. Make an access token

A token belongs to you and to one workspace. Make one per agent or machine, so you can
revoke one alone. In the web app, it's **Settings → Access tokens**: name the token,
choose when it expires (or never), and copy it. The page lists your tokens with when each
was created, last used and expires, revokes one after asking, and shows the setups below
for your server, with the new token filled in until you leave the page.

Without the web app, make one with your browser session, for example from the browser's
console on the TaskTo.AI page (workspace `1` here):

```js
await (await fetch('/api/workspaces/1/tokens', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ name: 'laptop: Claude Code', expires_at: '2027-06-30T00:00:00Z' }),
})).json()
```

The reply's `token` (`ttp_…`) is shown this once: the server keeps only its hash. Keep it
where you keep other secrets, and put it in an environment variable for the agent, not in
a file you commit:

```sh
export TASKTO_TOKEN='ttp_…'
```

`expires_at` is optional. `GET /api/workspaces/1/tokens` lists your tokens with when each
was last used, and `DELETE /api/workspaces/1/tokens/{id}` revokes one at once. Leaving the
workspace ends your tokens there. See [api.md](api.md#tokens).

## 2. Connect your agent

The endpoint is `https://<your server>/mcp`: MCP over Streamable HTTP, authenticated with
`Authorization: Bearer <token>`. Below, `https://taskto.example.com` stands for your server.

### Claude Code

```sh
claude mcp add --transport http --scope user taskto https://taskto.example.com/mcp \
  --header "Authorization: Bearer $TASKTO_TOKEN"
```

This stores the token's value in your Claude Code settings. To keep only a reference to
the variable, put the server in a project's `.mcp.json` instead (Claude Code expands
`${…}` from the environment):

```json
{
  "mcpServers": {
    "taskto": {
      "type": "http",
      "url": "https://taskto.example.com/mcp",
      "headers": { "Authorization": "Bearer ${TASKTO_TOKEN}" }
    }
  }
}
```

### Codex

```sh
codex mcp add taskto --url https://taskto.example.com/mcp --bearer-token-env-var TASKTO_TOKEN
```

or, in `~/.codex/config.toml`:

```toml
[mcp_servers.taskto]
url = "https://taskto.example.com/mcp"
bearer_token_env_var = "TASKTO_TOKEN"
```

Codex reads the token from the variable each time it starts.

### Any other MCP client

- Transport: Streamable HTTP (not the older HTTP+SSE), URL `https://taskto.example.com/mcp`.
- Header: `Authorization: Bearer ttp_…`.
- Many clients take this shape:

  ```json
  {
    "mcpServers": {
      "taskto": {
        "url": "https://taskto.example.com/mcp",
        "headers": { "Authorization": "Bearer ttp_…" }
      }
    }
  }
  ```

The server is stateless: every request is a `POST` of one JSON-RPC message, answered with
JSON (`Content-Type: application/json`, `Accept: application/json, text/event-stream`).
It has no `GET` stream and no session to end (`405`). It offers tools only: no resources,
prompts or sampling.

## What your agent can do

The 40 tools of [agent-tools.md](agent-tools.md#the-tools): `list_spaces`, `list_members` (who a card can be assigned to), `search_docs`, `search_cards` (the cards on boards), `search_records` (the text of an app's records you may read),
`list_docs`, `read_doc` (also plain-text files, and a document as of an older commit), `write_doc`, `move_doc`, `delete_doc`,
`doc_history` and `restore_doc` (a document's history and what brings it back), the board tools of
[boards-agent-tools.md](boards-agent-tools.md), and the app tools: `app_catalog`,
`list_apps`, `list_integrations` (the outside services a space's apps may use, by name, kind and status, and an app's declared slots with the status of their approval: never an account, a key or another space), `show_app`, `list_app_rows` (an app's data) and `query_app_rows` (filtered, sorted pages of it, with totals), `list_app_actions` (what the live release lets
you do to its records, and what your roles allow), `run_app_action` (add, change or delete a record by running one of those
actions), `create_app` and `update_app`, and the template, copy and diagnostic tools:
`list_app_templates`, `install_app_template`, `duplicate_app`, `list_app_effects`, `list_app_releases` and `show_app_roles`.
`run_app_action` runs the app's own action as you, through the code behind the web app's: your roles in the
app, the records you may modify, the collection's rules, the locks of captured values, the record's history and the
action's effects all apply, and it is refused (`403`) where your role doesn't allow it. It changes data, so the
server lists it with `readOnlyHint: false`; whether your MCP client asks you before calling it is up to the client (the protocol doesn't require it, so set your client to confirm tools that change records), separate from the permission prompt of TaskTo's CLI conversations ([cli-tools.md](cli-tools.md)), which
the server doesn't control here. It takes the `expected_release` that `list_app_actions` returned and runs only if that
is still the live release. `create_app` and
`update_app` save the app's draft; publishing it, and rolling back, are for a person who may publish
the app, in the web app ([drafts and releases](api.md#drafts-and-releases)): no tool does either. Read-only tools say so
(`readOnlyHint`), so a client can call them without asking you each time. Results and
refusals are the conversation agent's: a refusal is a tool error (`isError`) saying why
(`not found`, read-only, a stale version), for the model to act on. Their bounds are the
same too: results of at most 64 KiB, documents read 32 KiB at a time.

Start with `list_spaces`, for the tools that take a `space_id`. A card is assigned to a person by the `id` `list_members` returns.

## Who did what

Changes are yours, marked with the token:

- a document's commit has you as its author and `<token name> (access token)` as its
  committer, which the history shows as `agent`;
- cards, comments and apps are by you, with `"agent": true` and `"via": "<token name>"`.

## Security

- **Your access, nothing more.** Every request checks the token again: it must exist, not
  have expired, and you must still be a member of its workspace. Every space a tool names
  is checked with your access, now. A space you can't read, or of another workspace, is
  `not found`.
- **One workspace.** A token never reaches your other workspaces, whose data and model
  providers belong to other people.
- **Not your account.** A token can't log in, accept invites, manage members, spaces,
  provider accounts or search settings, see conversations, or make or revoke tokens:
  those routes answer `403` to a request carrying one ([api.md](api.md#access-tokens)).
- **Not from web pages.** `/mcp` refuses requests a browser marks as cross-origin, and
  never reads a cookie.
- **Hashed.** The server keeps the token's SHA-256 and never logs it. Anyone with the
  token is you in that workspace until it expires or you revoke it: treat it like a
  password.

## In TaskTo's own CLI conversations

A conversation that runs on Claude Code, Codex or Grok Build in TaskTo gets these tools
without an access token: the server gives each turn's CLI an endpoint of its own and a token
for that turn ([cli-tools.md](cli-tools.md)). This page is for agents you run yourself.

## Not yet

- OAuth sign-in for MCP clients, in place of a token you paste.
