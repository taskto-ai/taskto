// SPIKE CODE, not part of the product: runs the official Claude Code and Codex CLIs as
// a Pi Durable tool, to see whether they stream, record, resume after a crash and hand over.
// The write-up is docs/spikes/cli-workers.md. Build the agent first (pnpm run build), then:
//
//   SPIKE_DIR=/tmp/s SPIKE_BASE_URL=https://gateway.example SPIKE_KEY_FILE=/path/to/key \
//     node spike/cli-workers.mjs run '{"cli":"claude","user":"u1","prompt":"Reply pong"}'
//   node spike/cli-workers.mjs resume      # after a kill -9: finish what the last process left
//   node spike/cli-workers.mjs transcript
//
// Each user gets their own HOME, CLI config dir and working directory under SPIKE_DIR/users.
// Inference goes to a gateway that speaks Anthropic Messages and OpenAI Responses under /v1
// (SPIKE_BASE_URL, its root); the key is read from SPIKE_KEY_FILE per call and only passed in
// the child's environment, where every command the CLI runs can read it: use a low-value key.
import { spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { createInterface } from 'node:readline'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import { createRegistry, defineExtension, defineTool, Harness } from '@earendil-works/pi-durable'
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node'
import { bool, obj, opt, str } from '@earendil-works/pi-schema'
import { installProviders, loadProviders } from '../dist/providers.js'

const dir = process.env.SPIKE_DIR
const keyFile = process.env.SPIKE_KEY_FILE
const baseUrl = process.env.SPIKE_BASE_URL
if (!dir || !keyFile || !baseUrl) {
  console.error('SPIKE_DIR, SPIKE_BASE_URL and SPIKE_KEY_FILE are required')
  process.exit(2)
}
mkdirSync(dir, { recursive: true, mode: 0o700 })
const context = BACKGROUND_CONTEXT
const stateFile = join(dir, 'state.json')
const state = () => (existsSync(stateFile) ? JSON.parse(readFileSync(stateFile, 'utf8')) : {})
const save = (patch) => writeFileSync(stateFile, JSON.stringify({ ...state(), ...patch }))

// One user's files: nothing of the server's own HOME or config reaches the CLI.
function profile(user) {
  if (!/^[a-z0-9-]+$/.test(user)) throw new Error('bad user name')
  const root = join(dir, 'users', user)
  const p = {
    home: join(root, 'home'),
    claude: join(root, 'claude'),
    codex: join(root, 'codex'),
    work: join(root, 'work'),
  }
  for (const d of Object.values(p)) mkdirSync(d, { recursive: true, mode: 0o700 })
  writeFileSync(
    join(p.codex, 'config.toml'),
    [
      // JSON string escapes are valid TOML basic-string escapes, so quotes or newlines can't break out.
      `model = ${JSON.stringify(process.env.SPIKE_MODEL ?? 'auto')}`,
      'model_provider = "gateway"',
      'approval_policy = "never"',
      'sandbox_mode = "workspace-write"',
      '[model_providers.gateway]',
      'name = "Gateway"',
      `base_url = ${JSON.stringify(`${baseUrl}/v1`)}`,
      'env_key = "GATEWAY_KEY"',
      'wire_api = "responses"',
      '',
    ].join('\n'),
  )
  return p
}

// A clean environment: only what the CLI needs, never the sidecar's own variables.
function childEnv(cli, p) {
  const key = readFileSync(keyFile, 'utf8').trim()
  const base = { PATH: process.env.PATH ?? '/usr/local/bin:/usr/bin:/bin', HOME: p.home, LANG: 'C.UTF-8' }
  if (cli === 'claude') {
    return {
      ...base,
      CLAUDE_CONFIG_DIR: p.claude,
      ANTHROPIC_BASE_URL: baseUrl,
      ANTHROPIC_AUTH_TOKEN: key,
      ANTHROPIC_MODEL: process.env.SPIKE_MODEL ?? 'auto',
      ANTHROPIC_SMALL_FAST_MODEL: 'auto',
      ANTHROPIC_DEFAULT_HAIKU_MODEL: 'auto',
      DISABLE_TELEMETRY: '1',
      DISABLE_AUTOUPDATER: '1',
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
    }
  }
  return { ...base, CODEX_HOME: p.codex, GATEWAY_KEY: key }
}

function argv(cli, prompt, session, fresh) {
  if (cli === 'claude') {
    const s = fresh ? ['--session-id', session] : ['--resume', session]
    return [
      'claude',
      ['-p', prompt, ...s, '--output-format', 'stream-json', '--verbose', '--allowedTools', 'Bash,Write,Read'],
    ]
  }
  const head = ['exec', '--json', '--skip-git-repo-check']
  return ['codex', session ? [...head, 'resume', session, prompt] : [...head, prompt]]
}

// Only "no such process" means gone: EPERM is a group that exists but is not ours to signal.
const alive = (pid) => {
  try {
    process.kill(-pid, 0)
    return true
  } catch (error) {
    return error.code !== 'ESRCH'
  }
}

// Throws when the group survives (or cannot be signalled), so a rerun fails instead of resuming
// a session the old process is still writing to.
async function stopGroup(pid) {
  for (const signal of ['SIGTERM', 'SIGKILL']) {
    if (!alive(pid)) return
    process.kill(-pid, signal)
    for (let i = 0; i < 30 && alive(pid); i++) await new Promise((r) => setTimeout(r, 100))
  }
  if (alive(pid)) throw new Error(`the previous run's process group ${pid} is still running`)
}

// Both streams pass this before they reach the transcript.
const redactKey = (text) => {
  const key = readFileSync(keyFile, 'utf8').trim()
  return key ? text.replaceAll(key, '[redacted]') : text
}

const textOf = (content) =>
  typeof content === 'string' ? content : content.map((part) => (part.type === 'text' ? part.text : '')).join('')

// Runs one CLI turn, streaming its JSON events as the tool's output. Replay-safe on purpose: after
// a crash Pi Durable runs it again, and the memoised session id turns that rerun into a resume of
// the same CLI session instead of a fresh one.
const runCli = defineTool({
  name: 'run_cli',
  description: 'Run one turn of the Claude Code or Codex CLI for a user',
  parameters: obj({ cli: str(), user: str(), prompt: str(), withTranscript: opt(bool()) }),
  replay: 'safe',
  execute: async (args, api) => {
    if (args.cli !== 'claude' && args.cli !== 'codex') throw new Error('cli must be claude or codex')
    const p = profile(args.user)
    // Claude Code takes a session id up front; Codex names its thread in its first event.
    const session =
      args.cli === 'claude' ? await api.memo('session', randomUUID(), context) : await api.memo('session', context)
    // A kill -9 of the sidecar does not stop the CLI: the old process keeps running and writing
    // to the same session. Every attempt memoises its PID, so a rerun stops all earlier groups,
    // also after a second crash. (A real worker must also guard against PID reuse, e.g. by a
    // per-run cgroup or container instead of a bare PID.)
    let attempt = 1
    for (
      let old = await api.memo('pid-1', context);
      old !== undefined;
      old = await api.memo(`pid-${attempt}`, context)
    ) {
      if (process.env.SPIKE_KEEP_ORPHAN !== '1') {
        api.output(`stopping the process group ${old} of attempt ${attempt}\n`)
        await stopGroup(old)
      }
      attempt++
    }
    const rerun = attempt > 1
    let prompt = args.prompt
    if (args.withTranscript && !rerun) {
      // The tool's own conversation handle can submit but not read; the Harness can.
      const conv = await harness.conversation(api.conversationId, context)
      const view = await conv.context(context)
      // Everything before the current request: it is the last user message (after it come this
      // run's assistant tool call and nothing else yet). System prompts are not part of the hand-off.
      const current = view.messages.findLastIndex((m) => m.role === 'user')
      const lines = view.messages
        .slice(0, current)
        .filter((m) => m.role !== 'system')
        .map((m) => `${m.role}: ${textOf(m.content)}`)
      prompt = `Earlier in this conversation another agent worked on the task. Its transcript:\n${lines.join('\n')}\n\n${prompt}`
    }
    if (rerun) {
      prompt = `Your previous turn was interrupted by a restart. Continue the task: ${args.prompt}`
    }
    const [cmd, list] = argv(args.cli, prompt, session, args.cli === 'claude' && !rerun)
    api.output(`$ ${cmd} ${rerun ? '(resume)' : ''} session=${session ?? 'new'}\n`)
    const child = spawn(cmd, list, {
      cwd: p.work,
      env: childEnv(args.cli, p),
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true, // its own process group, so the whole tree can be stopped at once
    })
    // 'error' (e.g. ENOENT for a missing binary) fails the tool instead of throwing unhandled.
    const exited = new Promise((resolve, reject) => {
      child.once('error', reject)
      child.once('close', resolve)
    })
    exited.catch(() => {}) // awaited below, after the output; not an unhandled rejection meanwhile
    // Keep the tail of stderr: login and config errors show up only there.
    let stderr = ''
    child.stderr.on('data', (chunk) => {
      stderr = (stderr + chunk).slice(-2000)
    })
    if (child.pid !== undefined) {
      api.output(`pid=${child.pid}\n`)
      await api.memo(`pid-${attempt}`, child.pid, context)
    }
    let result = ''
    let thread
    for await (const line of createInterface({ input: child.stdout })) {
      api.output(`${redactKey(line)}\n`) // a command that prints its environment shows the key
      let event
      try {
        event = JSON.parse(line)
      } catch {
        continue
      }
      if (event.type === 'thread.started' && !thread) {
        thread = event.thread_id
        await api.memo('session', thread, context) // write-once: a resumed run keeps the first
      }
      if (event.type === 'result') result = event.result ?? ''
      if (event.type === 'item.completed' && event.item?.type === 'agent_message') result = event.item.text
      if (event.type === 'turn.failed') result = `failed: ${event.error?.message ?? 'unknown error'}`
    }
    const code = await exited
    const id = session ?? thread
    let text = `${args.cli} session ${id} exit ${code}: ${result.trim()}`
    if (code !== 0 && stderr) {
      text += `\nstderr: ${stderr}`
    }
    return { output: [{ type: 'text', text: redactKey(text) }] }
  },
})

const models = createModels()
installProviders(models, loadProviders(undefined))
const registry = createRegistry()
registry.install(defineExtension({ name: 'cli-spike', tools: [runCli] }))
const harness = await Harness.open(
  await openNodeSqliteStorage(join(dir, 'agent.db')),
  { models, registry, identity: { attribution: null }, onReport: (r) => console.error(`report: ${r.kind}`) },
  context,
)
harness.resume()
console.error(`sidecar pid=${process.pid}`)

async function transcript() {
  const conv = await harness.conversation(state().conversation, context)
  for (const m of (await conv.context(context)).messages) console.log(`${m.role}: ${textOf(m.content).slice(0, 400)}`)
}

const [mode, json] = process.argv.slice(2)
if (mode === 'run') {
  let id = state().conversation
  if (id === undefined) {
    id = (
      await harness.createConversation(
        { ownership: { kind: 'ownerless' }, agent: { model: { provider: 'script', modelId: 'script-1' } } },
        context,
      )
    ).id
    save({ conversation: id })
  }
  const conv = await harness.conversation(id, context)
  const submission = await conv.submit({ type: 'input', content: `tool:run_cli ${json}` }, context)
  save({ submission: submission.id })
  console.log(JSON.stringify(await submission.wait(context)).slice(0, 300))
  await transcript()
} else if (mode === 'resume') {
  const submission = await harness.submission(state().submission, context)
  console.log(JSON.stringify(await submission.wait(context)).slice(0, 300))
  await transcript()
} else if (mode === 'transcript') {
  await transcript()
}
await harness.close?.(context)
process.exit(0)
