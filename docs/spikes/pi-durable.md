# Spike: Pi Durable as a sidecar for the Go server

> **Since then:** the sidecar is `agent/src/` (about 2,000 lines, not 250), still pinned to
> Pi Durable 1.5.2, and the Go server supervises it (`internal/agentrt/supervisor.go`). It
> grew beyond the protocol below: a models catalog and a providers file the server
> rewrites and has it reload, an egress policy for providers' endpoints, the agent's
> language, the knowledge-base, board and app tools that call back into the server for the
> conversation's owner, and the runner of CLI turns. Its integration tests are still
> `make agent-it`. See [architecture.md](../architecture.md), [agent-tools.md](../agent-tools.md) and
> [agent-providers.md](../agent-providers.md). The rest of this page is the spike as it was.

**Verdict: go.** Pi Durable (`@earendil-works/pi-durable` 1.5.2) runs as a Node sidecar that only the Go server talks to.
Everything the spike set out to check works, with no changes to Pi Durable and about 250 lines of sidecar.

## What was checked

All of these are automated in `internal/agentrt/sidecar_test.go`. The tests start the real sidecar with a scripted
offline model (`agent/src/model.ts`, built on pi-ai's faux provider), so they need no keys or network. Run them with
`make agent-it`; CI runs them in the "Agent runtime integration" job.

| Check | Result |
|---|---|
| Submit from Go, wait for the answer | Works. The same `requestId` returns the original submission instead of a second one. |
| Stream a conversation to Go while it runs | Works: the view first, then the operations of each commit, including streamed tool output. |
| Two clients watching one conversation | Both receive the same run, live. |
| Steer from Go while a tool runs | The steer joins the running work after the tool round; the run's answer replies to it. |
| `kill -9` during a **replay-safe** tool call, then restart | The call runs again and the original submission settles `done` with the full result. |
| `kill -9` during a **run-once** tool call, then restart | The model gets an error result: "Tool pause_once was interrupted and may have partially run". The run still settles `done`. |
| A second sidecar on the same socket | It exits instead of taking the socket over; the first keeps running. |
| Rebuild the view from the stream | A view rebuilt from operations with `applyImmutable()` (`@earendil-works/chord/delta`) is identical to a fresh snapshot. |

Measured on an M-series Mac with Node 26: the sidecar starts in well under a second and uses about 90 MB of memory.
A conversation with three runs (one with a streamed tool call) produced 21 operation batches (7 KB) against a 3.6 KB
snapshot.

## The wire protocol

HTTP with JSON bodies over a Unix socket (`agent/src/index.ts`):

```
GET  /healthz
POST /conversations                    → { id }
POST /conversations/:id/submit         { content, whenBusy?, requestId? } → { submissionId }
POST /submissions/:id/wait             → { status: "done" | "unanswered", ... }
GET  /conversations/:id/messages       → [{ role, text }]
GET  /conversations/:id/watch          → NDJSON: { type: "value", value }, then { type: "ops", ops } per commit
```

- **The Go server forwards watch streams without understanding them.** The browser keeps its view with
  `applyImmutable(view, ops)` from `@earendil-works/chord/delta`. Go checks who may read the conversation, then copies
  lines. Go never has to implement Chord's operations.
- **IDs are numbers**, not strings. Our own database maps users and workspaces to Pi conversation IDs.
- **Reconnects** start from the current view (the newest 50 entries). A client that needs older entries pages them;
  `ConversationTranscript` in Pi Durable does that and can run in the browser.

## Risks and how we handle them

1. **The API is experimental.** Pi Durable says it "changes without notice between releases". The Pi packages are
   pinned to exact versions, and the integration tests are the guard for upgrades. If an upgrade breaks badly, the plan
   is to stay on the old version and reconsider a Go port of the ideas.
2. **One process per storage.** Pi Durable has no cross-process locking. The sidecar refuses to start while another
   one answers on its socket (tested), and the Go server must run exactly one per database file and restart it if
   it dies: the supervisor does (`internal/agentrt/supervisor.go`).
3. **No auth in the sidecar.** It relies on the socket being reachable only by the Go server: the socket is created
   owner-only (mode 600, tested); the Go server should also put it in an owner-only directory.
4. **Isolation between users** is the Go server's job: one Harness holds every conversation. If we want a harder
   boundary later, Pi Durable supports one storage per workspace (separate files, or a table `prefix` in one SQLite
   database).
5. **Side effects and crashes.** Tools with side effects must be `replay: "unsafe"` (the default) or idempotent. The
   interrupted-tool error is clear enough for the model to recover.
6. **Dependency weight.** pi-ai brings every provider SDK (about 90 packages). Two install scripts (`@google/genai`,
   `protobufjs`) are denied in `pnpm-workspace.yaml`; neither is needed.
7. **Node 22.19+** is required by Pi Durable. `engines` says so.
8. **Attribution.** By default pi-ai tells some providers (OpenRouter, NVIDIA NIM, OpenCode) the requests come from pi.
   The sidecar sends no attribution for now; it should send taskto.ai's once we have a public URL.

## Not checked here

- A crash during model streaming. Pi Durable documents it as a failed attempt that is retried, but the spike only
  crashed during tool calls.
- Real providers, compaction, subagents, and many concurrent conversations (load).
- Running the official CLIs (Claude Code, Codex) as Pi Durable tasks: the next spike, [cli-workers.md](cli-workers.md).

## Follow-ups for the agent runtime

- The Go server starts and supervises the sidecar: one per database, restart with backoff, stop on shutdown.
  Done in `internal/agentrt/supervisor.go` (`Supervisor`), which also holds a lock on the database (`<db>.lock`)
  so a second server can't start a second sidecar on it. The lock isn't passed to the sidecar, whose tool
  subprocesses would inherit it; a sidecar orphaned by a crashed server is caught by the next sidecar's startup
  probe. `cmd/taskto` runs it when the agent runtime is on (`-agent`), with its database and socket under
  `<data>/agent/`.
- Create the socket in an owner-only directory (the socket itself is already mode 600). Done: the supervisor
  creates the directory with mode 0700 and refuses an existing one that is looser or not this user's.
- The sidecar should exit when the Go server is gone, so a server killed with `kill -9` doesn't leave it holding
  the database until someone stops it. Done: its stdin is a pipe only the server holds, and with
  `TASKTO_AGENT_EXIT_ON_EOF=1` it shuts down when the pipe closes; tested by killing the server binary.
- Store which user and workspace own each conversation, and check it on every call. Done:
  `internal/conversations`, the conversation API in [api.md](../api.md#conversations).
- Proxy watch streams to the browser over SSE or WebSocket, with the browser applying operations.
  Done: Server-Sent Events, each watch line passed on unchanged; `cmd/taskto/conversations_it_test.go`
  rebuilds the view from them with `applyImmutable()`.
- Replace the scripted model with real providers, keeping it for tests.
