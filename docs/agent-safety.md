# Agent safety: no root, no shared secrets, permission prompts

Agents read web pages, files and messages that someone else wrote, so any of them can carry
a prompt injection. These rules make sure one injection can't become root on the machine, or
read every secret of a team. They rule out three setups that are common in agent platforms
and that turn one injection into a disaster:

- admins and their agents with passwordless root;
- every agent running with every permission allowed;
- a team password-manager token in every agent's environment.

The rules are in [AGENTS.md](../AGENTS.md) and [architecture.md](architecture.md#agent-safety).
This note says how they are kept.

An **agent environment** is any process where a model's commands can run: a CLI worker's CLI
and everything it starts. The agent sidecar is not one. It runs no shell and no command a
model chooses; its tools are the knowledge-base, board, app and `cli_turn` tools, which call
the server.

## 1. Never root, never sudo

| Process | Rule | Checked by |
|---|---|---|
| The server, with the agent sidecar or `local` CLI workers on | Not root, and no passwordless `sudo` or `doas` | The server at start: it refuses to start them |
| The sidecar | Runs as the server's user | The same check |
| A CLI and its commands (`local` mode) | The server's user, as above; on Linux, `no_new_privs` | The helper refuses to start a CLI as root, and sets `no_new_privs` before it execs the CLI |
| A CLI and its commands (`container` mode) | A Unix user of their own, uid 100000 and up, no groups, no capabilities, `no_new_privs` | The worker refuses to start if the image has a setuid or setgid program; the helper as above |
| The container worker itself | Root, with only `SETUID`, `SETGID`, `CHOWN` and `KILL`, to create the users; it never runs a model's command | Compose (`cap_drop: [ALL]`, `no-new-privileges`), and a test of the Compose file |

- "Passwordless sudo" is checked by running `sudo -n true` (and `doas -n true`) with an empty
  environment. It is what an agent could do on its own. A `sudo` that asks for a password is
  fine; one with a cached password is not, so the server says to run `sudo -k`.
- `no_new_privs` means a setuid program can't raise a CLI's privileges, so even a `sudo` that
  slipped into an image is useless to it.
- The agent browser starts through the same helper, as the user's Unix user, and gets
  the same two guards: it reads web pages, the likeliest place for an injection.
- **The override** is `-unsafe-privileged-agents` (`TASKTO_UNSAFE_PRIVILEGED_AGENTS=true`), on
  `taskto` and on `taskto cli-worker`. It is for development, for example a throwaway VM
  where the developer is root. With it the checks only log a warning. The image and the
  Compose file never set it, and a test checks that.

## 2. No secrets in an agent's environment

- A CLI's environment is built from nothing: `PATH`, `HOME`, `LANG`, `TMPDIR`, the CLI's
  config directory and the switches that turn off updates and telemetry. Its only credential
  is the user's own login, in the user's own config directory, or, on a workspace account
  ([account-switching.md](account-switching.md)), a short-lived token for that one turn, which the server's proxy swaps for the
  workspace's API key ([account-switching.md](account-switching.md#the-proxy)). The key
  itself never reaches the CLI. The other token a CLI holds is for TaskTo's own tools, for
  that turn alone: it acts as the conversation's owner, in that conversation, until the turn
  ends ([cli-tools.md](cli-tools.md#the-token)). A test lists the exact
  variables a CLI gets, and checks that nothing of the server's environment reaches it.
- In `container` mode the worker can't see the server's data directory, `server.key`, the
  database or the sidecar's socket. In `local` mode it can, which is why `local` is allowed
  only with one user.
- **Secrets are used, never handed over.** When an agent needs to act with a secret (a model
  provider's key, a future integration's token), the server does it with a tool that it
  runs for the conversation's owner, checking their access on every call. This is how the
  knowledge-base, board and app tools already work, and how the sidecar's provider calls
  go through the egress policy. No tool returns a secret's value, and no secret goes into
  an agent's environment, a file in its home, or its command line.
- Today no agent tool needs a workspace secret. A future feature that does (a vault, a git
  token, an integration) follows this rule.

## 3. Permission prompts, unless the user turns them off

Each conversation on a CLI has a setting, `cli_permissions`:

- **`ask`** (the default): the CLI asks before it runs a command, edits a file or fetches a
  page. The question appears in the conversation with **Allow** and **Deny**. Reading files
  in the conversation's working directory doesn't ask.
- **`allow_all`**: nothing asks. The user turns this on for one conversation, in the web app,
  and is told what it means first. It applies from the next turn.

Each CLI is driven headless (`-p` / `exec`), so it can't show a prompt. Each one does have
a documented hook that runs our program, `taskto cli-approve`, before a tool runs. That
program asks the worker, and the worker asks the user:

| CLI | Mode in `ask` | The hook | What reaches the user |
|---|---|---|---|
| Claude Code | `--permission-mode default`, `--setting-sources user` | `PermissionRequest`, in `CLAUDE_CONFIG_DIR/settings.json` | Exactly what Claude Code would have asked, by its own rules |
| Codex | `approval_policy = "never"`, the sandbox as before | `PreToolUse`, in `CODEX_HOME/hooks.json` (run with `--dangerously-bypass-hook-trust`) | Every tool call, minus what the worker's read-only policy allows |
| Grok Build | `--permission-mode bypassPermissions` | `PreToolUse`, in `GROK_HOME/config.toml`, `failClosed = true` | As for Codex |

- **Only Claude Code has a hook for "would ask"** (`PermissionRequest`). Codex's
  `PermissionRequest` fires only when its sandbox needs to escalate, which never happens in
  `container` mode, where its sandbox can't start. Grok Build has none. So for Codex and
  Grok the worker gates every tool call itself (`PreToolUse`), with a small read-only policy
  so that reading doesn't ask:
  - the file-reading tools (`Read`, `Glob`, `Grep`, `LS`), on paths inside the working
    directory;
  - a shell command that is one program from a fixed allow-list of readers (`cat`, `ls`,
    `rg`, `grep`, `find`, `head`, …; never an interpreter such as `sh`, `awk`, `sed` or
    `python`, and never `git`, which runs what a repository's own `.git/config` says, even
    for `git status`), with no shell syntax at all outside quotes (no `;`, `|`, `>`, `$`,
    backticks, globs, `~`, backslashes or newlines), none of the flags that make an allowed
    program run or write something (`find -exec`, `rg --pre`, `date -s`, in either
    `--flag value`, `--flag=value` or clustered `-zi` form), and no path outside the working
    directory (absolute, `~` or `..`).
  Everything else asks: a command the policy doesn't recognise costs a question, never an
  unasked action. The policy is in `internal/cliworker/policy.go`, with its tests. It is
  lexical: it doesn't follow symlinks, since the worker never opens a path in a user's home.
  A symlink out of the working directory had to be made by a command the user allowed, or
  brought in by one (a clone).
- **TaskTo's own tools** (the knowledge base, boards, apps and browser, over MCP:
  [cli-tools.md](cli-tools.md#permissions)) are under the same prompts: the read-only ones
  (`list_spaces`, `read_doc`, …) don't ask, as reading the working directory doesn't, and the
  others (`write_doc`, `create_card`, `browser_open`, …) ask the owner, unless they allowed
  everything in the conversation. Claude Code asks about every MCP tool, so the worker answers
  its question about a read-only one at once.
- **The settings files are ours.** The worker writes them before every run, so a command
  that changed them doesn't change the next turn. Claude Code reads no project or local
  settings (`--setting-sources user`) and only our MCP servers (`--strict-mcp-config`), and
  Codex and Grok run no project hooks (they need a trust we never give), so files the agent
  writes in its working directory can't allow anything.
- **The hook fails closed.** If `taskto cli-approve` can't reach the worker, gets no answer
  in 20 minutes, or fails in any way, it denies. The hooks' own timeouts are longer than
  that. A denial is given three ways at once: the hook's JSON reply, exit code 2, and the
  reason on stderr. Codex 0.169 ignores a hook that exits 2 with nothing on stderr, JSON and
  all, and runs the command; the real-CLI test found it, and the fake Codex now does the
  same.
- **How a question travels.** The worker listens on one Unix socket in the layout's root,
  which every CLI user can open. In `container` mode it knows who is asking from the
  socket's peer uid, and it finds the turn by the session ID in the hook's input. It answers
  at once for an `allow_all` turn or a read-only call. Otherwise it adds a `permission`
  event to the turn (the tool and a short form of its input) and waits. The server lists a
  conversation's pending questions and passes the owner's answer on. Only the conversation's
  owner can see or answer them.
- **What this protects, and what it doesn't.** A command the user allowed runs as the CLI's
  Unix user, and can do anything that user can, including changing the CLI's settings for
  the rest of the turn. The prompt stops the *first* step an injection would need. The OS
  boundary (section 1) limits what an allowed command can reach.

This replaces the first version's choice of CLI workers ("turns run with the permission
prompts off, inside the OS boundary"). [cli-workers.md](cli-workers.md#permissions-inside-the-turn) is changed to
match.

## Install checks

- The server and the container worker refuse to start on a setup that breaks section 1, and
  say what to change.
- `cmd/taskto/deploy_test.go` checks the Compose file and the image: the server runs as
  `65532`, the worker keeps only its four capabilities with `no-new-privileges`, the image
  has no setuid programs, and nothing sets the override.
- The CLI worker, where agents' commands and the agent browser run, reaches the internet and
  the per-turn proxy only, not the other services, the host or its LAN
  ([cli-workers.md](cli-workers.md#the-workers-network)); `deploy/worker-network-test.sh`
  proves it in CI, and the install checks it on the live VM.

## Open questions

Defaults taken, still open to change:

1. **Codex and Grok ask more than Claude Code.** Their hook sees every tool call, and the
   read-only policy is deliberately small. A "remember for this conversation" answer, like
   Claude Code's own, could come later.
2. **Grok Build's hook is unverified.** The gateway can't sign Grok Build in, so it was
   checked against the documentation in its binary only. A test with a real login
   should confirm that a `PreToolUse` deny in `GROK_HOME/config.toml` blocks a command in
   `grok -p`, and which tool names Grok uses.
3. **A question waits 20 minutes**, then it is denied and the CLI goes on with the denial.
4. **`allow_all` is per conversation, not per workspace.** An admin can't turn the prompts
   off for everyone.
