# Spike: Claude Code and Codex as Pi Durable tasks

> **Since then:** CLI workers are built: the server's `internal/cliworker` runs the CLIs,
> and the sidecar's side is `agent/src/cli.ts`; Grok Build is a third CLI beside Claude Code
> and Codex; in container mode a worker of its own runs each user's CLIs as a Unix user of
> their own, which is the OS boundary this spike asked for; every CLI starts through a
> helper that refuses root by default (local development can override with
> `-unsafe-privileged-agents`) and, on Linux only, sets `no_new_privs`; a user can have several accounts per
> CLI, and workspaces share accounts through a per-turn proxy; and CLI turns call taskto's
> own tools. See [cli-workers.md](../cli-workers.md),
> [account-switching.md](../account-switching.md) and [cli-tools.md](../cli-tools.md). The
> spike's `run_cli` code is still only a spike, and the rest of this page is the spike as it
> was.

**Verdict: go for CLI workers, with an OS boundary per user.** Both official CLIs run without a terminal, stream JSON
events, keep their sessions on disk and resume them after a sidecar crash, and a conversation can move from one to
the other through the Pi Durable transcript. What does not hold on its own is isolation: a CLI runs as the server's
OS user, so it can read every other user's files. CLI workers need a separate OS user or container per user.

Versions: Claude Code 2.1.311, Codex CLI 0.169.0, Pi Durable 1.5.2, macOS (Apple silicon).

The spike code is `agent/spike/cli-workers.mjs`: a Pi Durable tool, `run_cli`, driven by the scripted model, that
runs one CLI turn for a user. It is not built, shipped or wired into the server.

## How it was run

No subscription was available, so the CLIs used a self-hosted OpenAI-compatible gateway (free tiers of many providers),
which speaks both wire formats the CLIs need: Anthropic Messages (`/v1/messages`) for Claude Code and OpenAI
Responses (`/v1/responses`) for Codex. Each CLI was configured only through its own process:

- Claude Code: `ANTHROPIC_BASE_URL` (the gateway root, no `/v1`), `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL` and the
  small/fast model set to a gateway model, `CLAUDE_CONFIG_DIR` and `HOME` set to the user's directories.
- Codex: `CODEX_HOME` with a `config.toml` that adds a `model_providers` entry (`wire_api = "responses"`,
  `env_key` naming a variable that holds the key), and `HOME` set to the user's directory.
- The child gets a clean environment: only `PATH`, `HOME`, `LANG` and the variables above. Nothing of the
  sidecar's own environment reaches it.

The gateway's usage counter counted the requests, so they went through the gateway and not through the machine's own
Claude/Codex setup. Nothing was written to the real `~/.claude` or `~/.codex` by the spike runs.

## Findings

### 1. Driving the CLIs without a terminal: yes

- `claude -p <prompt> --output-format stream-json --verbose` prints one JSON event per line: `system/init` (with
  the `session_id`), `assistant` messages (text, thinking, `tool_use`), `user` messages (`tool_result`), and a
  final `result` with the answer.
- `codex exec --json <prompt>` prints `thread.started` (with the `thread_id`), `turn.started`, `item.started` /
  `item.completed` (agent messages, `command_execution` with the command, output and exit code) and
  `turn.completed` with usage, or `turn.failed` with the error.
- Both ran tools for real: Claude Code's `Write` and `Bash` (with `--allowedTools`), Codex's shell, under its
  `workspace-write` sandbox. Files appeared in the user's working directory.
- **Codex waits on stdin** ("Reading additional input from stdin...") until it closes: spawn it with stdin closed.

### 2. Recording the transcript into a Pi Durable conversation: yes

The tool passes every event line to `api.output()`, so watchers of the conversation see the CLI work live, and
returns the CLI's final answer and session id as the tool result, which the conversation keeps:

```
user: tool:run_cli {"cli":"claude","user":"u1","prompt":"Reply with exactly: pong"}
toolResult: claude session 7441dc9b-… exit 0: pong
```

The full transcript stays in the CLI's own session file (`<CLAUDE_CONFIG_DIR>/projects/<dir>/<session>.jsonl`,
`<CODEX_HOME>/sessions/…/rollout-…-<thread>.jsonl`). For CLI workers the events should also be kept in Pi Durable, in a
document or entries of their own rather than in the tool result, which the model reads.

### 3. Resuming after a sidecar restart: yes, once the old CLI is stopped

The tool is replay-safe, so Pi Durable runs it again after a restart. It memoises the session first
(`api.memo`): Claude Code takes a session id up front (`--session-id <uuid>`), and Codex's thread id is saved
from its `thread.started` event. A rerun resumes that session (`claude --resume <id>`, `codex exec resume <id>`)
with "Your previous turn was interrupted by a restart. Continue the task: …".

Tested by `kill -9` of the sidecar while each CLI was running a 40-second shell command, then starting it again:

| | Claude Code | Codex |
|---|---|---|
| CLI after the sidecar's `kill -9` | still running | still running |
| After the restart | old process group stopped; the session shows the killed call as `Exit code 137`; the resumed turn ran the command again and answered `done` | old process group stopped; same thread resumed; command ran again, answered `done` |

- **The CLI outlives the sidecar.** In a first run without cleanup, the old Claude Code process kept going and
  wrote into the same session file as the resumed one, with both turns' entries interleaved. The fix in the spike:
  start each CLI in its own process group (`detached: true`), memoise its PID per attempt, and stop the group (TERM, then KILL)
  before resuming. A real worker must not trust a bare PID after a restart (PIDs are reused): run each turn in a
  cgroup or container it can stop by name.
- **Not exactly once.** The interrupted command ran again on resume. Same rule as for Pi Durable's own tools: side
  effects must be idempotent or the model must be told what was interrupted.
- Each attempt memoises its own PID, so a crash during a resumed run is cleaned up too (tested: two crashes in a
  row). A crash between starting the CLI and memoising its PID still leaves an orphan the spike cannot find.

### 4. Handing a conversation from one agent to another: yes

Claude Code was asked for a codeword ("zup"). Codex was then started on the same conversation with the
conversation's messages (role + text) put before its prompt, and answered "zup". A tool's own conversation handle
cannot read the conversation (`submit`, `abort`, `waitForIdle` only); the spike reads it through the Harness.
For CLI workers the server should build the hand-off from the transcript itself and pass it in.

The hand-off carries what the conversation recorded, not the other CLI's private session (its tool calls,
files read). A summary written for the next agent is likely better than raw messages once conversations are long.

### 5. Each user's CLI with only that user's login and files: partly

What separate directories give:

- Separate `HOME`, `CLAUDE_CONFIG_DIR` and `CODEX_HOME` per user keep sessions, settings, memory, skills and
  history apart: everything the CLIs wrote landed in that user's directories.
- With a key in the environment, neither CLI stores a login. By their docs, Codex keeps a login in
  `CODEX_HOME/auth.json`, and Claude Code in its config directory on Linux and in the Keychain on macOS. **Real
  subscription logins were not tested** (none available): that is CLI workers' first check.

What they do not give:

- **Another user's files are readable.** User `bo`'s Codex, in its `workspace-write` sandbox, ran
  `cat /tmp/…/users/u1/work/private.txt` and printed the contents. Codex's sandbox limits writes, not reads, and
  Claude Code's tools run unsandboxed by default. Both run as the server's OS user, so a user's CLI can read other
  users' files, config dirs (with their logins), the server's data directory and its secrets.
- So a CLI worker needs an OS boundary: a separate Unix user per taskto user (files mode 700), or better a
  container per user or per run with only that user's directories mounted and no access to the server's.

## Go / no-go

- **CLI workers: go**, if workers run behind an OS boundary per user (container or Unix user), with a clean
  environment, stdin closed, a process group or cgroup per turn that a restart can stop, and the session id saved
  before the CLI starts. First check there: the real login flows (`claude` and `codex login`, device-code where
  possible), and where each stores its token in a container.
- **Account switching: go.** An account is a config directory (`CLAUDE_CONFIG_DIR` / `CODEX_HOME`):
  switching is starting the next turn with another directory. A session belongs to the directory it was started
  in, so switching mid-conversation needs a hand-off (finding 4), not a resume.

## Risks

1. **Terms of use.** Running the official CLIs on a user's own subscription from a self-hosted server is a
   question for each provider's terms (personal use, automation, sharing an account in a workspace). Not checked
   here; check before shipping CLI workers, especially for workspace-shared accounts.
2. **Isolation** (finding 5): without an OS boundary, any user's CLI can read everyone's logins.
3. **The CLI's credentials are readable by the commands it runs.** A key in the CLI's environment, or a login in
   its config directory, is one `env` or `cat` away for any shell command the model runs. Even behind an OS
   boundary, only the user's own login (or a low-value key scoped to them) may go into a worker; never a server or
   workspace key. Applies to workspace-shared accounts too: every member's commands can read that login.
   The spike redacts the key from everything it records (events, result, stderr), but the CLI's own session log
   is outside its reach: Codex's rollout file kept the key in clear after a command printed `env`. Treat a
   user's CLI directory as holding their secrets.
4. **Orphans and repeats** (finding 3): a CLI that outlives its sidecar writes into a session someone else
   resumes; a resumed turn repeats interrupted side effects.
5. **The event formats are not a promised API.** Pin the CLI versions shipped with the product, and test each
   upgrade against the events the worker reads.
6. **Background work in the CLI.** Claude Code refused `sleep 45 && …`, ran a long command in the background, and
   reported it in a later turn (`<task-notification>`). A turn can end with work still running in the CLI.

## Model notes (not runtime findings)

The gateway's free tiers are a stand-in, not something to judge the CLIs by:

- `auto` sometimes picked a model that wrote a tool call out as text instead of calling it. Pinning a model that
  calls tools (`glm-5.3`, `deepseek-v4-flash`, `gpt-oss-120b`) fixed that.
- `glm-5.3` ran out of free quota mid-spike (`503 … All models exhausted`); Codex retried five times and failed
  the turn. A Codex thread on `deepseek-v4-flash` got no answer for over ten minutes and was stopped.
- Both CLIs warn about model names they don't know (`unrecognized_model`, "Model metadata … not found") and work
  anyway.
