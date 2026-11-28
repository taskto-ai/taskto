# Account switching and workspace-shared accounts

The design of account switching. It builds on CLI workers ([cli-workers.md](cli-workers.md)): there, each
user connects one login per CLI, and a conversation's turns run on it. This design adds:

1. **Several accounts per CLI** for a user: a personal plan and an employer's seat, say.
2. **Workspace-shared accounts**: an account a workspace offers its members, with a mode per
   person: always shared, own only, or own if connected, else shared.
3. **An ordered queue for automatic switching** across accounts and CLIs when one stops (a
   usage limit, an expired login). It is off until the user turns it on.
4. **Each provider's terms linked next to these settings.**

## The terms, and what they decide

Checked on 2026-11-19, against the pages below.

- **Anthropic** ([Claude Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)):
  - An end user may sign in to the unmodified Claude Code binary with their own subscription,
    also where a platform hosts it.
  - Developers may not "route requests through Free, Pro, Max, or Ultra plan credentials on
    behalf of their users", nor "collect, store, or intermediate Claude.ai credentials or
    session tokens".
  - It "does not restrict how customers provision and manage their own API keys … for use by
    the customer's own authorized users", billed to the key's owner and not resold.
  - Plan limits "assume ordinary, individual usage".
- **OpenAI** ([terms of use](https://openai.com/policies/row-terms-of-use/), for ChatGPT plans;
  the [services agreement](https://openai.com/policies/services-agreement/), for API keys;
  [Codex authentication](https://developers.openai.com/codex/auth)):
  - The terms forbid sharing account credentials or making an account available to others.
  - Codex's guide treats `auth.json` "like a password" and recommends API keys for automation.
  - Codex rotates its refresh token, so one login shared by several people logs them out of
    each other.
- **xAI** ([terms of service](https://x.ai/legal/terms-of-service), for Grok plans; the
  [enterprise terms](https://x.ai/legal/terms-of-service-enterprise), for API keys): the same
  shape. Its pages refuse automated fetches, so its wording still needs a check by hand.

What this decides:

- **A workspace-shared account is an API key, never a subscription login.** "Shared account"
  could be read as one person's Claude, ChatGPT or Grok login used by everyone (a shared,
  writable `auth.json`, or captured `setup-token`s). That is what the terms forbid, and this
  design doesn't offer it. The workspace's key is
  billed to the workspace, for its own members: the case Anthropic's page allows.
- **The worker never sees a shared key** (spike risk 3: whatever a CLI can read, the commands it
  runs can read). The server keeps the key sealed and adds it to the CLI's requests itself,
  through a proxy (below). The CLI holds only a short-lived token that works for its own turn.
- **Several accounts per user are each the user's own login**, connected through the CLI's own
  sign-in. Nothing is copied between users.
- **Switching between subscription logins is the user's choice, with a warning.** Plan limits
  assume individual use, and automatic switching between subscriptions may break Anthropic's
  and OpenAI's rules. Switching is off by default. The setting says, next to the
  terms' links, that each account must be the user's own and that using extra accounts to get
  around a plan's limits may break its terms. Whether to forbid it outright between two
  subscriptions of the same provider is an open question (below).

## Several accounts per CLI

An account is a CLI config directory (a finding of the [CLI spike](spikes/cli-workers.md)): `config/<cli>` for the first, as today,
and `config/<cli>-<n>` for the others (n is 2 to 9). The account's handle in the API is the
same: `claude`, `claude-2`, `codex-3`. The existing routes keep working on the first account.

- `cli_accounts` gets `slot` (1 for the first) and `label` ("Work", "Personal"), unique per
  user, CLI and slot.
- The worker's calls (login, code, cancel, status, logout, turn) take the slot. The helper maps
  it to the config directory. The worker never chooses one itself.
- Adding an account is starting a login on the next free slot. The account exists once its
  login connects, as today. Disconnecting one removes its directory and sessions only.
- Every account shows its own state and usage limits.

### Which account a conversation uses

A session lives in one account's directory, so changing account starts a new session with a
**hand-off** (the conversation so far before the prompt, as for a change of CLI). The first version of CLI workers started
one only when the conversation had no session on the account. That misses a conversation that
went from A to B and back to A, whose session on A doesn't know B's part. So `cli_sessions`
records when each session was last used, and a turn on an account that wasn't the
conversation's last starts a new session with a hand-off, even when it has an old one there.

## Workspace-shared accounts

An admin adds a shared account for a CLI in the workspace's settings:

| Field | |
|---|---|
| CLI | `claude`, `codex` or `grok` |
| Label | "Company Anthropic key" |
| Base URL | the provider's API by default (`https://api.anthropic.com`, `https://api.openai.com`, `https://api.x.ai`); a gateway's otherwise, under the workspace's egress policy |
| API key | sealed with the server key, write-only: the API says `set` or `not set` |

Table `cli_shared_accounts`, one row per account. A workspace may have several per CLI. Members
see the accounts (never the key), admins change them. A **Test** button calls the provider's
model list with the key, through the egress client.

### Per-person modes

Each member has a mode per workspace, which applies to every CLI:

| Mode | A turn in this workspace runs on |
|---|---|
| `own_else_shared` (the default) | the member's own connected account for the CLI; if there is none, the workspace's first shared account for the CLI |
| `own` | the member's own accounts only |
| `shared` | the workspace's shared accounts only |

- The workspace has a default mode, and an admin can set a member's own. Admins decide, since the
  workspace pays for the key and may want company work on company accounts.
- "First" is by the member's queue (below), then the oldest: a member who queued one of the
  workspace's shared accounts gets that one first.
- "Connected" means the account's state is `connected`. An own account that needs a new sign-in
  falls back to the shared one in `own_else_shared`; in `own` the turn asks for the sign-in, as
  today.
- In `shared` with no shared account for the CLI, the turn is refused with a message that says
  so, rather than falling back to a personal login the workspace chose not to use.
- Tables `cli_member_modes` (workspace, user, mode) and `cli_workspace_modes` (the workspace's default).

### The proxy

A turn on a shared account runs the official CLI in the user's own directories, in a config
directory of its own (`config/<cli>-s<id>`), with no login in it. The CLI is pointed at the
server instead of the provider:

| CLI | Settings for the turn |
|---|---|
| Claude Code | `ANTHROPIC_BASE_URL=<proxy>/cli-proxy`, `ANTHROPIC_AUTH_TOKEN=<token>` |
| Codex | a `taskto` model provider in its `config.toml`: `base_url = "<proxy>/cli-proxy/v1"`, `env_key`, `wire_api = "responses"` |
| Grok Build | `GROK_CLI_CHAT_PROXY_BASE_URL=<proxy>/cli-proxy/v1`, `XAI_API_KEY=<token>` |

These are the same settings the CLI workers' gateway tests use, with the server in the gateway's place.

- **The token** is minted for one turn: the shared account, the user and their account's identity
  (`users.cli_identity`), the conversation and its workspace, and an expiry, sealed with the server key
  (`serverkey.Seal`, so the CLI reads none of it) and signed with the server key (`serverkey.MAC`). The expiry is a fixed two hours: the
  worker's longest turn (30 minutes) and a wait for a free place, with room to spare. It is
  stateless, so a server restart in the middle of a turn doesn't break it.
- **Each request** is checked again, so a change takes effect at once: the signature and expiry,
  the user's number is still the account with the token's identity (a restore of the server's data
  gives numbers again, never an identity, so a token from before it reaches no one given the number
  since), the shared account still exists, the user is still a member of its workspace, the conversation is
  still theirs and not archived, and their mode still allows shared accounts.
- **A token stops with its turn.** The server remembers the turns that ended (in memory) and
  refuses their tokens from then on. A restart forgets that list; the expiry then bounds what
  is left.
- **The server** replaces the token with the real key (`x-api-key` for Anthropic,
  `Authorization: Bearer` for OpenAI and xAI) and forwards the request to the account's base URL
  through the workspace's egress client. Only paths under `/v1/` pass. Responses stream back
  unbuffered. The key never leaves the server process.
- **What the token is worth.** A command the model runs can read it (risk 3). It can make model
  calls on the shared account, as that user, while the turn runs: what the user can do anyway by
  sending a message. It can't reveal the key, and it stops working when the turn ends, the user
  leaves the workspace or the account is removed.
- **Where the proxy is.** It has a listener of its own, not the server's public one, so no
  reverse proxy in front of the server can expose it by mistake:
  - `-cli-proxy-listen` (`TASKTO_CLI_PROXY_LISTEN`): where it listens. In `local` mode the
    default is `127.0.0.1` on a free port, this machine only. In `container` mode the Compose
    file sets `unix:/run/taskto-cli/proxy/proxy.sock`, a socket in the volume the server and
    the worker share: the worker's network reaches the internet only, not the server
    ([cli-workers.md](cli-workers.md#the-workers-network)). The worker relays the CLIs'
    connections from `127.0.0.1:8081` to the socket.
  - `-cli-proxy-url` (`TASKTO_CLI_PROXY_URL`): its address as the CLIs reach it. It defaults to
    the listen address in `local` mode; Compose sets it to the worker's relay,
    `http://127.0.0.1:8081`.
  - It serves only `/cli-proxy/v1/…`, and taskto's tools for CLI turns at `/cli-mcp`
    ([cli-tools.md](cli-tools.md)), with no session and no CSRF: a turn's token is the only
    credential of each.
  - Without a proxy, shared accounts are listed, but turns on them are refused and the reason
    is given.
- **Usage limits.** API keys have no plan windows. The proxy records a `429` on the account,
  with its reset time from `retry-after` or the provider's rate-limit headers, as a limit with
  window `api`. That is what the switching queue reads.

## Automatic switching

The user's queue (Settings → CLI accounts): an ordered list of accounts, their own and the
shared accounts of their workspaces, with a switch that is **off by default**.

- Table `cli_switching` (user, enabled) and `cli_switch_queue` (user, position, own account or
  shared account).
- The queue can only hold the user's own accounts and shared accounts of workspaces they are a
  member of. At a turn, entries the conversation's workspace doesn't allow are skipped: a shared
  account of another workspace, or what the member's mode rules out.

**When switching is off**, a turn runs on the account the mode picks, as above: the first own
account (by queue order, then slot) that is connected, or the shared one. A known limit doesn't
move it: choosing another account because one is at its limit is switching, which the user
turns on. The turn ends with the limit's message, as before.

**When switching is on:**

- **At the start of a turn**, the conversation stays on the account it last ran on, if that is
  allowed, connected and not limited. Otherwise it takes the first queue entry for the
  conversation's CLI that is, and otherwise the first entry that is, of any CLI.
- **When the account stops** in the middle of a turn, the turn goes on with the next entry that
  can run. An account stops when:
  - the CLI reports a limit as `rejected` (Claude Code's `rate_limit_event`, its limit message;
    Codex's usage-limit error or a window at 100%), or the proxy recorded a `429`;
  - the login needs renewing (`login_required`; the account still goes to `reconnect`).
  A stop by the user, a time-out or another failure doesn't switch.
- **Limited** means a rejected limit whose reset time is still ahead, or, without a reset time, one
  recorded in the last hour; or a Codex window at 100% until it resets.
- **The switch is a hand-off, not a resume** (see the [CLI spike](spikes/cli-workers.md)): the next account starts a new session with
  the conversation so far, the prompt, and a note that the previous agent stopped part-way, so
  some steps may have run and its files are in the working directory (shared by every account
  of the user, per conversation). The note includes what the stopped turn had answered so far.
- **The conversation shows it**: a notice "Claude Code (Work) hit its usage limit (resets 3 pm):
  continuing on Codex (Personal)". When the next account is another CLI, the server also sets
  the conversation's model to that CLI, so the picker shows what runs it, and the next turn
  starts there.
- **One worker turn per account.** Each run is a worker turn of its own (`c<conversation>-<turn>`,
  then `…-s1`, `…-s2`). The server numbers their events in one sequence, so a sidecar that
  re-attaches reads the whole chain once.
- **Nothing left**: if no other entry can run, the turn ends as it would without switching (the
  limit's message, or the Reconnect wait).
- `/cli/wait` answers once the account the conversation would use is connected, so the
  Reconnect wait also ends when the user connects another account.

## API

Under [api.md](api.md#cli-accounts):

- `/api/me/cli-accounts`: the list gains every account per CLI (`id` like `claude-2`, `slot`,
  `label`); `{account}` replaces `{cli}` in the routes; `PATCH /api/me/cli-accounts/{account}`
  sets the label.
- `/api/workspaces/{workspace}/cli-accounts`: shared accounts (`GET` for members; `POST`,
  `PATCH`, `DELETE`, `POST …/test` for admins); `GET`/`PATCH …/cli-accounts/modes` for the
  default and members' modes (admins change them; a member reads their own).
- `/api/me/cli-switching`: `GET` (the switch, the queue, and the accounts that can go in it),
  `PUT` (the switch and the queue).

Each has tests for a cross-user and a cross-workspace attempt: another user's account in a
queue, a shared account of a workspace the user isn't in, a member changing a shared account or
a mode, a token for one account used for another, a token after the user left.

## Testing without real subscriptions

- No sign-in to any real account. The CLI workers' fake CLIs (`internal/cliworker/clitest`) print a
  rate-limit event, a limit message and an expired login; they gain an echo of the base URL and
  token they were given, so tests check the proxy settings reach them.
- The proxy is tested with a fake provider (`httptest`): the key replaces the token, other
  headers pass, `429`s are recorded, a forged, expired or foreign token is refused, and the key
  never shows in a response, a log line or the CLI's environment.
- With `TASKTO_CLI_IT_BASE_URL` and `TASKTO_CLI_IT_KEY_FILE` (as for the CLI workers), an integration test
  runs the real Claude Code and Codex through the proxy to an OpenAI-compatible gateway: a
  shared account whose base URL is the gateway.
- **What needs real logins to check**: two subscription logins of one CLI side by
  side (separate config directories, and separate Keychain items on macOS); that a real plan
  limit triggers the switch; a real Anthropic, OpenAI or xAI API key through the proxy.

## Slices

1. This note.
2. **Several accounts per CLI**: migration, slots in the worker and its remote API, the API by
   account handle with labels, sessions' last use and the hand-off on an account change, the
   web app's list of accounts.
3. **Workspace-shared accounts**: migration, the admin API and modes, the proxy and its tokens,
   choosing the account by mode, the workspace settings with the terms' links.
4. **The switching queue**: migration, `/api/me/cli-switching`, switching at the start and in the
   middle of a turn, the model change, the web app's queue with the warning and the terms' links.

## Open questions

Defaults taken, still open to change:

1. **Switching between two subscriptions of one provider** is allowed, off by default, with the
   warning above. The stricter option is to switch only to another provider or to an API key.
2. **Modes apply to every CLI**, set by admins, defaulting to `own_else_shared`. Modes per CLI,
   or chosen by members, are possible later.
3. **A shared account is shared with every member** of the workspace. Choosing which members can
   use one (a list per account) can come later.
4. **Switching covers CLI accounts only.** Falling back to a model provider (the agent runtime's
   own API models) changes how the conversation runs and is left out.
5. **The conversation stays on the account it switched to** until that one stops, rather than
   going back once the first one's limit resets, to save hand-offs.
6. **The proxy's token can't be revoked before it expires**, by design (stateless). Removing the
   account or the member stops it at once anyway.
