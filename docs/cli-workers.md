# CLI workers: Claude Code, Codex and Grok on the user's own login

A conversation can run on a **CLI worker**: the official Claude Code, Codex or Grok Build CLI,
signed in with the user's own subscription, instead of a model API. This note is the design.
It builds on the spike ([spikes/cli-workers.md](spikes/cli-workers.md)), which
showed the CLIs stream headless, record into Pi Durable, resume after `kill -9` and hand a
conversation over, and set the conditions this design meets:

| Spike condition | Where it is met |
|---|---|
| An OS boundary per user | [Process and OS model](#process-and-os-model): a Unix user per taskto user, in a worker container that can't see the server's data |
| Something per turn a restart can reliably stop | [Stopping and resuming](#stopping-and-resuming): a process group per turn, a sweep of every CLI user's processes when the worker starts, no bare PIDs |
| A clean environment | [The turn](#the-turn): the child's environment is built from nothing |
| The session id saved before the CLI starts | [Sessions](#sessions) |
| Only that user's own credentials in a worker | [Logins](#logins): the worker only ever holds the user's own CLI login, never a server or workspace key |

Account modes (own, organisation, own-if-connected) and switching accounts are not here:
this design connects one personal login per CLI per user. Their design is
[account-switching.md](account-switching.md). taskto's own tools in these conversations (the
knowledge base, boards, apps and the agent browser) are [cli-tools.md](cli-tools.md).

## Choices

### Headless modes, not ACP adapters

Each CLI is driven through its own documented headless mode, one process per turn:

| CLI | Turn | Events | New session with our id | Resume |
|---|---|---|---|---|
| Claude Code | `claude -p` | `--output-format stream-json --verbose` | `--session-id <uuid>` | `--resume <id>` |
| Codex | `codex exec --json` (stdin closed) | `thread.started`, `item.*`, `turn.*` | no: the thread id is the first event | `codex exec resume <id>` |
| Grok Build | `grok -p` | `--output-format streaming-messages-json` (Claude Code's stream-json shape) | `-s <uuid>` | `-r <id>` |

Grok Build's `streaming-messages-json` is Claude Code's stream-json shape, so one parser reads
both. Its `streaming-json` is the xAI-native alternative if that shape drifts.

ACP (Agent Client Protocol), which some agent platforms use, was considered. It was not
chosen, for now:

- **Terms.** Anthropic's legal and compliance page allows a user to sign in to the
  *unmodified Claude Code binary* with their own subscription, also where a platform hosts
  it. It says products built on the Agent SDK should use API keys. `claude-agent-acp` is
  such a product. Driving `claude` itself is the reading that holds up.
- **Not the official binaries, and two more moving parts.**
  - `codex-acp` was a separate build of Codex's crates (Zed's, archived). It is now a
    TypeScript adapter over `codex app-server` that expects a newer Codex than the one
    pinned.
  - `claude-agent-acp` pins an Agent SDK version that has to match the CLI's.
  - The adapters are Apache-2.0, which is compatible with MIT. Still, every CLI bump would
    become a two-package upgrade.
  - Grok's `grok agent stdio` is native ACP, but `grok -p` is just as official.
- **Crash recovery.** One process per turn, resumed by session id, is what makes a crash
  recoverable: stop whatever is left, then resume the session. A long-lived process per
  loaded conversation (several hundred MB each) has to be supervised, capped and
  restarted anyway.
- **What ACP adds isn't needed here.** ACP's addition is answering the CLI's permission
  requests from the UI. Each CLI's documented hooks do that in headless mode too (see
  [Permissions inside the turn](#permissions-inside-the-turn)).
- **No command lines with secrets.** With `CLAUDE_CODE_EXECUTABLE`, the Claude adapter puts
  MCP servers' env and headers on the CLI's command line, where other local users can read
  them.

The worker's events follow ACP's `session/update` vocabulary where they overlap:

| Worker event | ACP `session/update` |
|---|---|
| `text` | `agent_message_chunk` |
| `thinking` | `agent_thought_chunk` |
| `tool` | `tool_call` / `tool_call_update`, with statuses `in_progress`, `completed`, `failed` |

So an ACP driver (OpenCode's `opencode acp`, or Grok's) can be added later behind the same
interface without changing the sidecar or the web app.

Tool approvals in the UI go through the CLIs' hooks, not through a bidirectional protocol
(see [Permissions inside the turn](#permissions-inside-the-turn)). Grok's `grok agent stdio`
(ACP), Codex's `codex app-server` and Claude Code's `--input-format stream-json` control
messages remain the alternatives, if a hook stops being enough.

**One process per turn, not one per conversation.** Keeping a CLI process per loaded
conversation and letting it sleep after an hour idle is the other common design. Here a
turn's process exits when the turn ends, which is that policy with no idle time:

- Only Claude Code has a multi-turn stdin mode. Codex's (`app-server`) and Grok's (ACP) are
  the bidirectional protocols above, so a resident process would need them.
- Resuming by session id costs a CLI start, a second or two. The provider's prompt cache is
  on its side, so a resumed session keeps it.
- No memory is held between turns.

A resident Claude Code process for busy conversations can come later, behind the same
interface.

### Go owns the processes; the sidecar records

The CLI processes are managed by Go (`internal/cliworker`). The sidecar records the turn in
the conversation:

```
browser ─▶ Go server ─▶ agent sidecar (Pi Durable)
               ▲            │ cli_turn tool (replay-safe)
               │            ▼
               └── tools socket: POST /cli/turn  (capability = the conversation)
               │
               ▼
          CLI worker (Go: in the server, or `taskto cli-worker` in its own container)
               │  one process group per turn, as the user's Unix user
               ▼
          claude / codex / grok
```

- The Go server already knows who owns a conversation, which account the owner has
  connected, and holds every secret. The sidecar knows none of that and stays that way.
- The drivers could live in the sidecar, in `agent/`. They are in Go because the parts
  around them have to be in Go anyway:
  - in `container` mode the worker is a separate privileged process that creates Unix users,
    and it must not be a Node service (AGENTS.md);
  - logins, status checks and usage limits don't involve a conversation, so they don't go
    through the sidecar.
  One implementation serves both modes.
- The sidecar's part is small. A conversation whose model is a CLI (`cli-claude`,
  `cli-codex`, `cli-grok`) gets a **driver model** that makes no model call. It turns the
  user's new input into one `cli_turn` tool call, and the tool's result into the final
  answer. The tool calls the server on the existing tools socket with the conversation's
  capability (as the knowledge-base tools do). It passes the server's NDJSON events to
  `api.output()`, so the conversation shows the CLI's work live.
- The server never trusts the tool's arguments for *whose* login to use: the account is
  always the conversation owner's, looked up from the capability.

## Process and OS model

### Worker modes

`-cli-workers` selects one of three modes:

**`off`** (default): no CLI models are offered.

**`container`**: the production mode, used by the Compose install.

- The worker is a second service, `taskto cli-worker`, running the same binary in its own
  image: Debian slim with bash, git, coreutils, ripgrep and the pinned CLIs. The CLIs'
  tools need a shell, so the distroless server image can't host them anyway.
- It mounts only its own volume (`cli-homes`) and a socket volume. It never sees the
  server's data directory, `server.key`, the database or the sidecar's socket.
- It runs as root with every capability dropped except `SETUID`, `SETGID`, `CHOWN` and `KILL`. It needs those to create users, create their homes and run each turn as the
  user.
- **Each taskto account gets a Unix user** in the worker container, by its home number
  ([below](#accounts-and-their-homes)), never its user number:
  - uid `-uid-base` (100000 by default) plus the home number, a group of its own, no
    supplementary groups;
  - an `/etc/passwd` entry (`th<home>`), since Node's `os.userInfo()` needs one;
  - home `/homes/h<home>`, mode 700, owned by that uid. It holds the CLIs' config directories
    (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`), which hold the login, and a working
    directory per conversation.
- Every login, status check and turn runs as that uid, through a small helper
  (`taskto cli-helper`, the same binary): the worker starts it with the user's credentials,
  and it prepares the user's directories and the CLI's settings and execs the CLI. The worker
  never opens a path inside a home, where the user could have put a symlink. The exec drops
  every capability (the user isn't root and the binaries have none), and the image has no
  setuid program. So:
  - a user's CLI can't read another user's home (the spike's `cat` of another user's file
    fails with `EACCES`);
  - it can't signal or trace another user's processes;
  - it can't reach the worker's socket, which is in a root-only directory and handed to the
    server's uid alone.
- The worker kills a turn's leftovers while the CLI is still unreaped (`waitid` with
  `WNOWAIT`), so the process group's ID can't have been reused. Its PID 1 is `tini`, which
  reaps what outlives a turn.
- The worker also runs each user's agent browser, as the same Unix user, through the same
  helper ([agent-browser.md](agent-browser.md)); its routes are under `/browser/` on the same
  socket.
- The server reaches the worker over HTTP on that Unix socket. It is the worker's only
  client, and the worker does no auth of its own, like the sidecar. The server never reads a
  user's home: login status, login flows and usage limits are worker calls, answered by the
  helper running as the user (the rule: the server never reads the person's home).
- Its network is its own, and reaches the internet only ([below](#the-workers-network)).

**`local`**: development, macOS, and a personal install without the worker container.

- The worker runs inside the server process. The CLIs run as the server's own OS user, with
  a home per taskto account under `<data>/cli/h<home>/` (mode 700) and the same clean
  environment.
- **There is no OS boundary between taskto users in this mode.** Any user's CLI, or a command
  it runs, can read every other user's CLI login, the server's data directory with
  `server.key` and the database, and signal the server. Separate directories only keep
  sessions and settings apart, not secrets.
- So the server allows local mode only on an instance with **exactly one user**. A
  personal install is the CLI owner's own machine and data, so there is nothing to
  separate. When a second user exists, CLI turns and logins are refused with a message that
  names `container` mode.
  `-cli-workers=local-unsafe` lifts that check, for development and tests only, and the server
  logs a warning at start.
- macOS has no per-user Unix users for us to create, so local mode is what runs there. Claude
  Code keeps its login in the macOS Keychain rather than in `CLAUDE_CONFIG_DIR`, so on macOS
  the login is the Keychain item of the server's user (see [Open questions](#open-questions)).

### The worker's network

A CLI, a command it runs and the agent browser can be steered by text someone else wrote, so
the worker reaches the internet (the providers, and whatever a turn or the browser opens) and
the server's per-turn proxy, and nothing else. Not the server, Ollama, a database or another
Compose service, not the host, its LAN or a cloud's metadata service.

- **Its own network.** In `compose.yaml` the worker is alone on `worker`, a bridge named
  `taskto-worker` with no IPv6. No other service is on it, and Docker keeps its networks
  apart, so the other services' addresses don't answer and their names don't resolve.
- **A firewall on the host**, `deploy/worker-firewall.sh`: what comes in from that bridge for
  an address that isn't public (the same ranges as the egress policies, `internal/egress`) is
  dropped in the `raw` table, before Docker's NAT; what comes in from it for the host itself
  is dropped in `INPUT`; IPv6 from it is dropped entirely. A port published on the host's
  public address is reached as anyone on the internet would. The rules are in tables Docker
  doesn't manage, so the install's unit puts them in before Docker starts and they hold
  across restarts ([deploy/INSTALL.md](../deploy/INSTALL.md), step 8). Without them the worker
  still can't reach the other services, but it can reach the host and its LAN.
- **The proxy over the socket volume.** The server listens for the proxy on a Unix socket,
  `/run/taskto-cli/proxy/proxy.sock` (`-cli-proxy-listen unix:…`), in the volume the two
  containers share. The worker makes its directory, the server's (mode 711), and relays the
  CLIs' connections from `127.0.0.1:8081` to it (`taskto cli-worker -proxy-socket …
  -proxy-listen …`), byte for byte. So the worker's network needs no route to the server,
  and the firewall no exception. The socket is mode 666, since the worker's root has no
  `CAP_DAC_OVERRIDE`; a CLI user who opens it directly gets what the relay gives them, and
  the per-turn token is the proxy's only credential, as before.
- **DNS.** The worker asks public resolvers (`dns:` in `compose.yaml`): the host's may be on
  its LAN, which the firewall closes. Behind a network that allows only its own resolver, set
  it in `compose.override.yaml`; a resolver on a private address also needs an exception in
  the firewall, which it doesn't have.
- **The proof** is `deploy/worker-network-test.sh`, a CI job: in a throwaway privileged
  Docker-in-Docker host, with the real `compose.yaml`, the real server and worker binaries
  and the install's firewall, a CLI's user in the worker reaches the internet and the proxy,
  and doesn't reach the server, Ollama, the host (through its gateway or its own address) or
  a machine on the host's LAN. Each refusal has a control: the same listener answers from
  another container, and from the worker once the firewall is taken out. Step 13 of the
  install checks it again on the live VM.

### The turn

1. The server resolves the conversation's owner and their connected account for the CLI. A
   missing or expired login answers "connect" / "reconnect" without starting anything.
2. The server records the session (below), then asks the worker to start the turn: the user,
   CLI, model, session id (new or resume), working directory (`work/c<conversation>`, fixed
   per conversation, since Claude Code finds sessions by working directory) and prompt.
3. The worker starts the CLI:
   - in its own process group, stdin closed;
   - with a clean environment built from nothing: `PATH`, `HOME`, `LANG`, `TMPDIR` (inside
     the home), the CLI's config directory, and the switches that turn off auto-update,
     telemetry and non-essential traffic (`DISABLE_AUTOUPDATER`,
     `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `GROK_DISABLE_AUTOUPDATER`; Codex's
     `check_for_update_on_startup = false` in its `config.toml`).
   - Nothing of the server's or the worker's environment reaches it, and never a key: the
     only credential is the login in the user's own config directory (or, on a workspace's
     account, the proxy's token for the turn).
   - taskto's own tools, over MCP: the helper writes each CLI's MCP settings for the server's
     endpoint for turns, and the turn's token for it is in `TASKTO_TOOLS_TOKEN`
     ([cli-tools.md](cli-tools.md)).
4. The worker turns the CLI's events into one small event stream:
   - `session`: the id;
   - `text` and `thinking`: deltas;
   - `tool`: name, short input, status;
   - `usage`: tokens and rate limits;
   - `permission`: the CLI asks to use a tool, and the answer;
   - `login_required`;
   - `error`;
   - `done`: the final answer and exit code.
   It keeps a bounded log of them per turn, so a caller that lost the stream can re-attach
   by turn id and read on from where it was.
5. When the turn ends, whatever is left in its process group is stopped (risk 6 of the spike:
   a CLI's background work doesn't outlive the turn).

Limits: one turn per conversation at a time (Pi Durable runs one), at most 3 running turns per
user and a worker-wide cap (`-cli-max-turns`, default 8); a turn over the cap waits. Each turn
has a wall-clock limit on the CLI process (`-cli-turn-timeout`, default 30 minutes). Waiting
for a Reconnect (below) happens between CLI runs, with no process running, so that limit
doesn't apply to it.

CPU and memory:

- The worker container has Compose limits (`cpus`, `mem_limit`), so busy CLIs can't starve
  the server, which runs in another container.
- A cgroup per turn, for per-turn limits, needs a cgroup v2 subtree delegated to the
  container. Docker doesn't delegate one to an unprivileged container by default. The worker
  uses one when it is writable (`/sys/fs/cgroup` delegated, or a systemd install) and
  otherwise relies on the container's limits.
- Stopping a turn never depends on a cgroup: it uses the process group and the uid sweep
  below.

### Permissions inside the turn

Turns run with the CLIs' permission prompts on, answered by the conversation's owner in the
web app, unless the owner turns them off for that conversation (the design is
[agent-safety.md](agent-safety.md#3-permission-prompts-unless-the-user-turns-them-off)).
The first version ran every turn with the checks bypassed; that is now only the
per-conversation `allow_all` setting.

- **Asking headless.** Each CLI runs our hook program, `taskto cli-approve`, before a tool
  runs. It asks the worker on the worker's approval socket, and the worker adds a
  `permission` event to the turn and waits for the owner's answer (20 minutes at most, then
  it denies):
  - Claude Code: `--permission-mode default`, with a `PermissionRequest` hook. It fires
    only for what Claude Code itself would ask about.
  - Codex: `approval_policy = "never"`, with a `PreToolUse` hook (`--dangerously-bypass-hook-trust`
    for the hooks file the worker writes).
  - Grok Build: `--permission-mode bypassPermissions`, with a `PreToolUse` hook that fails
    closed.
  For Codex and Grok, a small read-only policy in the worker lets reads in the working
  directory through without asking.
- **`allow_all`** (one conversation, the owner's choice): Claude Code runs with
  `bypassPermissions`, and the worker answers Codex's and Grok's hooks with "allow" at once.
- **Codex's sandbox** stays `workspace-write` where it works. In `container` mode it gets
  `danger-full-access`: the OS boundary is ours there, and Codex's own sandbox can't start
  inside a container. The hook still asks.
- **The OS boundary** is what limits a command the user allowed, so the worker never gives
  a CLI user anything to escalate with:
  - no sudo, and no setuid or setgid program in the image (the worker refuses to start if
    there is one);
  - no capabilities;
  - no supplementary groups;
  - `no_new_privs`, which the helper sets before it execs the CLI.

### Stopping and resuming

The spike's failure was a CLI outliving the sidecar and writing into a session the resumed
run also wrote to. The rules:

- **The worker's own restart stops every CLI.**
  - In `container` mode, the worker's first act is to kill every process owned by a CLI uid
    (walk `/proc`, match the uid range, `SIGKILL`), before it accepts a request. Nothing can
    be legitimately running then. Matching by uid can't hit an unrelated process the way a
    reused PID can. The range (`-uid-base` and up) is used only for CLI users, and the worker
    container runs nothing else: the worker itself is uid 0, outside it.
  - In `local` mode, the worker records each turn's process group with the leader's start
    time (`<home>/turns/<turn>.pid`). At start it kills only groups whose leader still has
    that start time.
- **A sidecar restart doesn't stop the CLI; it re-attaches.** The `cli_turn` tool is
  replay-safe and memoises a turn id (`api.memo`) before it calls the server. After a
  sidecar crash Pi Durable runs it again with the same id. The worker still has that turn,
  running or finished, and the tool reads its events on from the start. The CLI was never
  interrupted, so nothing runs twice.
- **A turn the worker no longer knows** (the worker or the whole server restarted) is
  resumed: the CLI session is resumed by id with "Your previous turn was interrupted by a
  restart. Continue the task: …", after the sweep above has stopped the old process. This
  can repeat a side effect of the interrupted turn (spike risk 4). The prompt says so, so
  the model checks before redoing work. The conversation shows it too: the turn's output
  starts with an "interrupted by a restart, resumed" marker, so the user sees why work may
  have run twice.
- **Stop** (the conversation's stop button) aborts the tool, and the server asks the worker
  to stop that turn's process group (`TERM`, then `KILL` after 5 s).

### Sessions

Table `cli_sessions`: conversation, CLI account, CLI session id, created.

- Claude Code and Grok take our id (`--session-id` / `-s`). The server generates a UUID and
  commits the row **before** it asks the worker to start the CLI.
- Codex names its thread itself, in `thread.started`, its first event, which comes before
  the model is called or a tool runs. The server commits the row when that event arrives,
  before it passes on any other event. A crash before it leaves nothing to resume and
  nothing done, so the next attempt starts a new thread.
- A session belongs to one account's config directory. A conversation whose CLI or account
  changes starts a new session with a **hand-off**: the earlier messages of the
  conversation (role and text, the newest that fit in 32 KB) go before the prompt, as in
  the spike. The sidecar passes them with the turn, and the server uses them only when it
  starts a new session.

## Logins

Each CLI's own login command, run by the worker as the user, in the user's config directory.
The UI shows what the command prints.

| CLI | Command | The UI shows | The user | Stored by the CLI in |
|---|---|---|---|---|
| Claude Code | `claude auth login --claudeai` | the sign-in URL it prints | opens it, signs in, pastes the code back; the server writes it to the command's stdin | `CLAUDE_CONFIG_DIR/.credentials.json` (Linux); the Keychain on macOS |
| Codex | `codex login --device-auth` | the URL and one-time code | opens the URL elsewhere, enters the code; the command polls until done | `CODEX_HOME/auth.json` |
| Grok Build | `grok login --device-auth` | the URL and one-time code | the same | `GROK_HOME/auth.json` |

- A login is a short-lived job in the worker:
  - it is `starting`, then `waiting` (with the URL, the code, and whether a code must be
    pasted), then `connected`, `failed` or `expired`;
  - it is cancelled after 15 minutes or by the user;
  - there is one at a time per user and CLI.
  The server keeps no copy of the credentials. They stay in the user's home, readable by
  that user's processes only, which is risk 3 of the spike accepted: the login is the
  user's own.
- Only the CLI's own interactive login is used, never a command that prints a token for us
  to read off the screen (`claude setup-token`). The credentials are written by the CLI,
  into the user's home, and refreshed by the CLI.
- The URL and code are parsed from the command's output. The URL is the first `https://` URL
  on the CLI's known sign-in host (`claude.ai` / `claude.com` / `anthropic.com`,
  `auth.openai.com`, `auth.x.ai` / `accounts.x.ai`), so a docs or changelog link the CLI
  prints doesn't win. The code is shaped like `XXXX-XXXX`. If no URL is on a known host,
  the first `https://` URL is shown as text with a warning, not as a button.
- **Status** comes from the CLI: `claude auth status --json` (`loggedIn`, `authMethod`),
  `codex login status` (its exit code and text), and for Grok whether `GROK_HOME/auth.json`
  exists, checked by the helper as the user. The server keeps the result in `cli_accounts`
  (below) and never the token.
- **Logout** runs `claude auth logout`, `codex logout` or `grok logout`, then deletes the
  account's sessions. Their files go with the config directory when the user disconnects. Before
  anything goes, the account is closed to new turns, sign-ins and checks (`ErrRemoving`, `409`,
  `cli_account_removing` in the server's API), and what runs on it is stopped and waited for:
  its turns (a CLI that kept running would hold the login it started with and write its session
  back into the directory), its sign-ins, its helper requests (cancelled, not waited out) and the
  process groups they started. Only this account: the user's other accounts, their other CLIs and
  other users go on, and nothing is marked, so the account works again once it is over. If
  something is still running after the wait, nothing is removed (`ErrBusy`, `409`,
  `cli_account_busy`) and the disconnect can be tried again. Removals that cover an account in
  common run one after the other (a disconnect, a second one, a `Forget` of the user): the helper
  requests of one, the sign-out and the removal of the files, never overlap another's. A caller
  that has already given up (its context ended) starts none.
- **Accounts and their homes.** A user number names an account only within one history of the
  server's database: a restore of the server's data goes back in it and gives the numbers after
  the backup again, while the worker's directory (the `cli-homes` volume in container mode) is not
  part of the backup. So the worker keys nothing by a user number. Each account has an identity
  that never changes and is never given to another (`users.cli_identity`, 128 random bits, set
  when the account is made), and every user-scoped request to the worker carries it, with the
  server's boot (a random value taken at each start), in `X-Taskto-Identity` and `X-Taskto-Boot`.
  - The first time the worker sees an identity, it gives it a **home number** from a counter that
    only goes up (`.worker/next-home`), and records the two together
    (`.worker/accounts/<identity>.json`, with the user number for logs and the boot). The counter
    is moved on, and the record written, each as a new file synced and renamed into a synced
    directory, before anything of the account's is made: a crash in between skips a number,
    never gives one twice.
  - The home is `h<home>` under the layout's root; with isolation its Unix user is `-uid-base`
    plus the home number. So everything an account leaves behind (files, temporary files,
    sockets, processes) belongs to a home and a uid no other account is ever given, and a request
    of an account that is gone, however late, can only reach that account's own home.
  - The identity is never in a path, and never reaches a CLI: not its environment, its arguments,
    its working directory or a file in its home. The helper is told the home number only.
  - What the worker holds in memory (sign-ins, turns, permission questions, the agent browsers and
    their refusals) is keyed by identity too, and a turn is found, read and stopped only for the
    account that started it: the worker's API answers none of another account's, the number's
    earlier owner's included.
  - Home numbers start at 2^30: a worker from before homes accepted user numbers below 2^30 only,
    so under the same `-uid-base` no home's uid is one an older worker used. There are 2^30 of
    them (about a billion accounts per worker volume). When they run out the worker refuses new
    accounts (`ErrFull`, `503` with the code `full` over the worker's API, an internal error in the
    server's) and every existing one goes on. An operator near that point (the counter in
    `.worker/next-home` near 2147483648) starts a fresh worker volume: everyone then signs in to
    their CLIs once more. `-uid-base` must leave room for every home below the 32-bit `nobody`:
    the worker refuses to start with one below 65536 or above 2147483646.
- **The volume.** A home number, and so a uid, is never given twice on a volume:
  - one worker per volume: the worker takes an exclusive lock on it (`.worker/lock`, a `flock`
    held while it runs) once it has made its own directories, before it reads the counter or a
    record, sweeps processes, writes anything else or opens a socket. A second worker on the same
    volume doesn't start, and leaves the first's sockets alone;
  - at every start the counter is checked against everything on the volume that names a home:
    the records, the homes' directories and the trash. A counter that is missing, damaged or
    behind (a partial restore of the volume, say) is moved past the highest home, with a warning;
    two records with the same home stop the worker, which says to start it on a new volume. The
    worker then keeps the highest number it has given in memory, with every home the volume has
    (a record, retired or not, or a directory): a counter file lost or put back while it runs
    never sends it lower, and none of those homes is given again, whatever the file says;
  - with isolation, the UID base is the volume's: pinned the first time (`.worker/uid-base`), and
    each record keeps its uid (every home is given in one place, so each record that has a home
    has it, one a census brings back included). A worker started with another `-uid-base` doesn't
    start (it would give a home's uid to another home), nor does one on a volume with homes whose
    base nothing can rebuild (no pin, no record with a uid); changing the base means a new volume;
  - with isolation, a new home is made whole under a temporary name (`.new-h<home>-…`), owned by
    its Unix user, and renamed into place: a stop or a failed `chown` in between leaves a
    temporary directory the next start removes, never a home of the wrong owner;
  - the worker's own directories are made with each new one's parent synced, before anything is
    recorded in them.
- **Server boots.** The worker orders the servers' boots by itself, never by a clock: the first
  census of a boot it applies gives the boot the next number of a counter that only goes up
  (`.worker/boot-seq`, moved on durably first), and a boot with a higher number is a later server.
  The order is on the worker's volume, so a restore of the server's data can't take it back, and a
  clock stepped back changes nothing. Only a census registers a boot: no other request does, so a
  delayed request of an earlier server can't make its boot the latest. Every account record keeps
  the number of the latest boot when it was made and, once retired, when that was. Then:
  - a census from a boot registered before the latest is ignored (`ErrStaleBoot`, `409`,
    `stale_boot`); a boot the worker hasn't registered is the latest from its census on (so the
    first census of a server registers it, whatever its clock says);
  - a census retires only accounts made before it was registered, apart from those its own
    server made: an account made since (by a later server, before its census reached the worker)
    is kept, so an earlier server's census, a retry or a delayed one, never retires it;
  - a census takes back only retirements made before it was registered: a deletion since (a purge
    running now) stays;
  - a deletion (`Forget`, and the agent browsers' purge in the worker, `FenceDeletion`) is taken
    only from the latest registered boot, and retires the account before it stops or removes
    anything. The server sends its boot with every deletion, the purge's too;
  - the list of boots (`.worker/boots.json`, the last 4096) only names the latest one, for
    deletions: what a census retires is decided by the records' numbers, never by the list. With
    the list lost, damaged or past its bound, no account is taken for a later server's; deletions
    wait for the next census, which sets the latest boot again;
  - censuses and deletions run one at a time, so no boot is registered between a decision and
    what it decides, and one whose caller has given up retires nothing more. The server stops
    trying a census the worker calls stale (a later server has spoken to the worker), and logs it.
- **Retired accounts.** An account that is gone (`Forget`, `Reconcile`) is **retired**: its record
  says so, durably, and keeps its home number, so the number is never given again. Every request
  for a retired identity is refused (`ErrForgotten`, `410 Gone` with the code `forgotten` over the
  worker's API, `cli_account_forgotten` in the server's): turns, sign-ins, status checks, helper
  requests and every agent-browser operation, an open browser's state and tabs too. What the worker
  still holds of it (a removal that failed before it dropped it) is refused too: its turns' state
  and events, its sign-ins, its permission questions. None makes its home again. A retirement is taken back only by a census that names the identity as live (a
  restore brought the account back) and was read after it; the account then finds its home as it
  was left, which a removal has usually emptied.
- **Before the first census.** A request that comes before this server's first census reaches the
  worker is served for its own identity only: the worker's record of it, or a new home it is
  given now. It can never be handed another account's home, whatever its number, so nothing waits
  for the census.
- **Removing an account's home** (`Forget`, `Reconcile`) closes everything of the account's to new
  starts, then, in order:
  - stops its agent browsers and waits for them (the profile is in the home);
  - stops what runs for it and waits for it: its turns (the ones waiting for a place too), its
    sign-ins, its helper requests and the process groups those started. In container mode it then
    kills every process of its Unix user and waits until none is left (five seconds at most: a
    removal that doesn't finish is tried again), one a CLI detached into a group or session of
    its own too. The browser processes are included;
  - takes the home away from its path at once, into the worker's own `.worker/trash/`, where no
    user can reach it. In container mode it first makes the directory the worker's (`CAP_CHOWN`),
    since moving a directory writes its `..`;
  - deletes it there as the worker: each directory is taken over before it is read, so a
    read-only part is no obstacle, and a link is removed, never followed;
  - drops what the worker remembers of the account: its sign-ins, and its finished turns with the
    permission questions those held.

  If something doesn't end in time, nothing more is removed (`ErrBusy`), and the removal can be
  tried again; the account stays retired meanwhile. A process a CLI detached is not tracked in
  local mode, which has no Unix user per person.

  The processes of a Unix user are ended through a pidfd: the uid is read while the reference is
  held, and the signal goes through it, so a pid given again to another process is never
  signalled. There is no fallback to a signal by pid: a worker that isolates accounts checks at
  start that pidfds work (Linux 5.3) and doesn't start without them. A thread group's leader that
  exited while other threads of its run still counts as running. The start-up sweep of every CLI
  user's processes works the same way, and a worker whose sweep couldn't look at every process
  doesn't start. Only a full look at the processes counts as "none left": one that can't be made,
  or a process whose state can't be read, fails the removal.

  While a removal runs, no helper is made for the account, and a live view's connection is a
  request of the account's from its first step to its close: a removal cancels it and waits for
  it, like a status check, and a cancel that comes as the connection is handed out still closes
  it. The testing profile's cleanup is such a request too. A browser start that raced the removal
  finds the account retired (or its gate closed) and starts nothing; one that got through before
  is stopped with the others, and its processes are its home's uid, which the removal kills. A
  deleted account's purge names the account its steps are for (its identity, kept with the steps
  when the purge began, `purge_cleanups.cli_identity`), so its steps act on that account's home
  only, never on the home of an account a restore gave the number to meanwhile. A caller that has
  given up does nothing.
- **Forget** (`POST /forget` on the worker's API, `Worker.Forget`) is for a deleted account. It
  first retires the identity, then removes its home as above. If that fails, the purge tries again
  (it gives each attempt two minutes), and the account stays retired. A purge's own browser step
  (`browser-purge`) runs under the account's gate. It is done when the home is already gone.
- **Reconcile** (`POST /reconcile` on the worker's API, `Worker.Reconcile`) is tidying up, not what
  keeps accounts apart. At every start the server sends the worker a census: each live account's
  number and identity, its boot, and when it read them (`Service.StartReconcile`, retried in the
  background until it succeeds, with a warning at the first failure and every tenth). The worker
  then:
  - takes back the retirement of a live account made before the census was read;
  - keeps an account the census doesn't name if this server made it since it read its database
    (the record's boot is the census's);
  - retires every other account it has a record of and removes its home as above;
  - removes the directories a worker from before homes made (`u<number>`), in container mode: nothing
    can tell an upgrade from a restore made by any version, so they are never handed to anyone
    ([install.md](install.md#upgrading-to-account-homes)). In local mode the first account of a
    number to make a request adopts its old directory instead (below);
  - deletes what an earlier removal left in its trash, and takes back the agent browsers' refusals
    of the live accounts.

  An account a census retires now may have work the worker admitted just before (a first turn, a
  sign-in, a browser) whose helper hasn't made its home yet: the census closes its gate and stops
  that work whether or not the home is there, so nothing makes the home afterwards. One retired
  already, whose home is gone, has nothing left to do.

  A home that can't be removed yet (a process of it doesn't end in time) is kept, its account
  retired, and the answer is `ErrIncomplete` (`409`, `incomplete`): the server tries again later.
- **Local mode's directories from before homes.** A local install's directories are in its data
  directory, so a backup brings back the ones that go with its database. The first request of an
  account whose number has a `u<number>` directory moves it to the account's new home, once: the
  record says a move is due before the rename and stops saying so after it, so a stop in between
  is finished by the next request, and a later account of the number finds nothing to adopt. A
  local install keeps its CLI sign-ins, working files and browser profiles. A conversation's
  working directory moves too, and Claude Code finds a session by it: the next turn may start a
  new session with the conversation so far rather than resume the one from before.
- **Outages.** While the worker can't be reached, at a start or later, the server's API answers
  `cli_worker_not_ready` (`503`) first: before it says whether a CLI is installed, in the settings'
  list, a sign-in and its check, a turn (before its stream starts, and before a resumed turn says
  it resumes) and a turn's stop. The agent browser's routes, the live view's handshake and the
  agent's browser tools give the same answer, with its code, and a CLI turn's tool calls through
  `/cli-mcp` say it in the error they return (`(code: cli_worker_not_ready)`).

Table `cli_accounts`, one row per user, CLI and account (a user may connect several per CLI,
[account-switching.md](account-switching.md)):

- the state: `connected`, `reconnect` or `disconnected`;
- the auth method and plan the CLI reports;
- the last usage limits, as JSON, and when they were read;
- created and updated.

API, all under `/api/me/cli-accounts`, for the signed-in user only, like `/api/me/providers`:

- `GET`;
- `POST /{cli}/login`, `POST /{cli}/login/code`, `GET /{cli}/login`, `DELETE /{cli}/login`;
- `DELETE /{cli}`.

Every route has a test that another user can't see, start, answer or cancel it.

## Usage limits

Shown per account in Settings and in the conversation header, "as of" the time they were read:

- **Claude Code** reports them in its event stream, as `rate_limit_event` (`rate_limit_info`:
  `status` `allowed` / `allowed_warning` / `rejected`, `rateLimitType` `five_hour` /
  `seven_day` / …, `resetsAt`, `utilization`). The worker passes the newest of each type
  on.
  - Checked in the 2.1.311 binary: the stream-json path emits these events, with the plan
    windows `five_hour` and `seven_day`.
  - They arrive only from Anthropic's API on a subscription, so they are unverified until
    a test with a real login. A gateway sends none.
  - Fallback: a turn that fails with Claude Code's limit message ("Usage limit reached …",
    "You've reached your … limit", "You've hit your … limit") marks the account's limit as
    `rejected`, with the reset text the message gives.
  - If the events turn out to be missing on a real subscription, that is the main reason to
    look at the SDK or ACP path again. The terms above still apply to any change.
- **Codex** writes them to its session file (`token_count` events carry `rate_limits`:
  `primary` and `secondary`, each with `used_percent`, `window_minutes`, `resets_at`). After
  each turn the helper, as the user, reads the last one from that session's rollout file.
  An on-demand refresh without a turn can use `codex app-server`'s `account/rateLimits/read`
  later.
- **Grok Build** reports tokens and cost per turn but no subscription limits; the UI says
  "not reported by Grok Build".

A turn that ends on a limit (Claude's `rejected`, Codex's usage-limit error) fails with the
reset time, and the account shows it.

## Expired login and Reconnect

- **Detection.** The worker looks for a failed login in the events and stderr:
  - Claude Code: an assistant message with `error: "authentication_failed"`, or a `result`
    with `is_error` and status 401 or "/login";
  - Codex: `turn.failed` or `error` with 401, "refresh token" or "log in";
  - Grok: `error` with 401 or "sign in", or exit 1 with the same on stderr;
  - all three: the status check failing at the start of a turn.
  It emits `login_required` and stops the turn.
- **The account** goes to `reconnect`. The conversation shows a **Reconnect** banner, as
  Settings does. The web app knows to show it from the conversation's model (a `cli-…`
  provider), the account's state (`/api/me/cli-accounts`), and the turn's `⚠` line, on
  which it reads the accounts again. The button runs the same login flow, in place.
- **The turn waits rather than fails.** The `cli_turn` tool doesn't return. It long-polls
  the server (`POST /cli/wait`, 60 s at a time) until the account is `connected` again, then
  resumes the CLI session with the same prompt. The conversation carries on by itself after
  the Reconnect, with no message to resend.
  - Stop ends the wait.
  - After 24 hours the tool gives up, with "sign in again and resend".
  - A sidecar restart in the meantime replays the tool, which waits again.

## Versions and updates

- The CLIs are pinned in `deploy/cli-workers.json`: each CLI's version, and per platform
  (`linux/amd64`, `linux/arm64`) the download URL and its sha256. The worker image's build
  downloads each one and checks the sha256 before it installs it. A test checks the
  manifest is complete and the Dockerfile reads it. Where each checksum comes from:
  - **Claude Code:** Anthropic's release manifest
    (`…/claude-code-releases/<version>/manifest.json`, `platforms[<platform>].checksum`),
    the same check its installer makes. It comes from the same origin as the binary, so it
    catches corruption, not a compromised origin.
  - **Codex:** the exact npm version, `@openai/codex`, whose tarball has an integrity hash
    in the registry. We record the sha256 of the platform tarball we install.
  - **Grok Build:** xAI publishes no checksum, so the sha256 is ours, taken when the
    version is bumped. That is trust on first use.
- The CLIs can't update themselves:
  - every auto-update switch is off (above);
  - they are installed root-owned, so no CLI user can write over them.
- **They are updated with the product.** A release bumps the manifest; `deploy/cli-workers.sh
  bump` fills in the new versions and checksums. Since the CLIs' event formats aren't a
  promised API (spike risk 5), the drivers have fixture tests recorded from each pinned
  version, and a bump re-records them against the gateway and fails on a changed event.
- The worker reports the installed versions (`GET /versions`). Admins see them in the
  workspace settings.
- In `local` mode the CLIs on the `PATH` are used, whatever their version. The server
  logs a warning when one differs from the manifest.

## Testing without real subscriptions

- **Fake CLIs** (small Go programs built by the tests) print what the real ones print:
  - the login output, with URL and code;
  - status;
  - the event streams of a turn, a resume, an expired login and a rate-limit event.
  The login flows, Reconnect and the isolation tests run against them.
- **Real CLIs on a gateway.** Integration tests run the real CLIs against an
  OpenAI-compatible gateway, as the spike did, with no subscription involved. They need
  `TASKTO_CLI_IT_BASE_URL`, `TASKTO_CLI_IT_KEY_FILE` and `TASKTO_CLI_IT_MODEL`, and are
  skipped without them. The key reaches the CLI only through its environment, from the
  file.
- **Real CLIs on a scripted model.** With `TASKTO_CLI_REAL_TOOLS=1`, the installed Claude Code
  and Codex run against a stand-in for Anthropic's and OpenAI's APIs on this machine, which makes
  them call taskto's tools ([cli-tools.md](cli-tools.md)) on a stand-in endpoint: no account, key
  or network. It checks the settings the worker writes, the names their permission hook asks
  under, and the token they send.
- **Isolation in `container` mode** is tested as root on Linux. They run in CI when its job
  is root, and locally through `docker run`. One user's turn tries to read, signal
  and connect to another user's files, processes and the worker socket, and must fail.
- **What needs real logins to check**:
  - each login flow end to end;
  - where each CLI keeps its token in the container;
  - that an expired or revoked login gives the events the detector looks for;
  - that Claude's and Codex's rate-limit fields appear on a subscription.

## Slices

Each is one pull request with its tests:

1. **This note.**
2. **The worker core** (`internal/cliworker`):
   - the three drivers (argv, environment, event parsing into the common stream, login
     and limit detection);
   - running a turn in `local` mode, with process groups, the turn log and re-attach, and
     the start-up sweep;
   - the fake CLIs, and the gateway integration test.
3. **Accounts and logins**:
   - migration (`cli_accounts`, `cli_sessions`) and store;
   - the login jobs, status and logout in the worker;
   - `/api/me/cli-accounts`;
   - `-cli-workers` with the one-user rule for `local`;
   - cross-user tests.
4. **Conversations on a CLI worker**:
   - the sidecar's driver model and `cli_turn` tool;
   - the server's `/cli/turn` and `/cli/wait` on the tools socket, sessions, hand-off,
     usage and Reconnect;
   - CLI models in the model list;
   - integration tests with the real sidecar and the fake CLIs.
5. **The OS boundary**:
   - `taskto cli-worker` (`container` mode): Unix users, homes, the exec helper, the uid
     sweep, the socket and the server's client;
   - the worker image with the pinned CLIs (`deploy/cli-workers.json`) and the Compose
     service;
   - the isolation tests as root.
6. **The web app**:
   - Settings → CLI accounts: connect with URL and code, Reconnect, usage limits;
   - CLI models in the model picker;
   - the Reconnect banner in a conversation.

## Open questions

Defaults taken, still open to change:

1. **Terms of use** (spike risk 1). The case built here is the one Anthropic's terms allow:
   a user signs in to the unmodified `claude` binary, hosted by a platform, with their own
   subscription, and the sign-in completes in Anthropic's own flow.
   - We never store the credential: it stays in the CLI's config directory in the user's home.
   - One step is a judgment call. For Claude Code, the one-time authorization code
     Anthropic's page shows passes through our UI to the CLI's stdin. That code is not a
     credential or a session token, but it does pass through us. Codex and Grok use device
     codes, which the user enters on the provider's own page.
   - OpenAI's terms forbid sharing an account. Nothing here shares one: every login is one
     user's, used only by that user's turns.
   - The UI links each CLI's terms next to Connect, as it does for model providers.
2. **A login per user, not per workspace.** A CLI account belongs to the user across all their
   workspaces (`/api/me/…`). Workspace accounts are separate. They must never be one subscription
   login shared by several people: that is against the terms, and Codex's refresh-token
   rotation logs out everyone sharing one. Nothing in this design needs a shared login file.
   Workspace accounts are API keys the server adds through a proxy
   ([account-switching.md](account-switching.md)).
3. **Permissions.** Settled in [agent-safety.md](agent-safety.md): turns ask, through each
   CLI's hooks, and the conversation's owner can allow everything for one conversation ([Permissions inside the turn](#permissions-inside-the-turn)).
   Its own open questions are in [agent-safety.md](agent-safety.md#open-questions).
4. **Claude Code on macOS** keeps its login in the Keychain. With `CLAUDE_CONFIG_DIR` set per
   user, separate users should get separate Keychain items. Not verified without a real
   login.
5. **`local` on Linux with several users** is refused rather than degraded. An operator who
   wants CLI workers for a team runs the worker container.
6. **Backups.** In `container` mode the CLI homes (logins and sessions) live on the worker's own
   volume, which `taskto backup` doesn't cover. Losing it means users sign in again and
   conversations start new CLI sessions with a hand-off. Nothing in the server's data is lost.
