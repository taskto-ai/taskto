import { mkdtempSync, rmSync } from 'node:fs'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { ToolAccess, type ToolSpec, tasktoTools, validCapability } from './taskto-tools.js'
import definitions from './taskto-tools.json' with { type: 'json' }
import { testHooks } from './test-hooks.js'

const specs = definitions as ToolSpec[]

// The tools of the extension the sidecar registers.
function toolsOf(socket: string, options?: { browser?: boolean }) {
  const { tools } = tasktoTools(socket, options)
  if (!tools) throw new Error('the extension has no tools')
  return tools
}

function names(options?: { browser?: boolean }) {
  return toolsOf('/unused.sock', options).map((tool) => tool.name)
}

describe('validCapability', () => {
  it.each(['a', 'tt_abc123-._~', '!~', 'x'.repeat(256)])('accepts %j', (value) => {
    expect(validCapability(value)).toBe(true)
  })

  it.each(['', 'x'.repeat(257), 'has space', 'tab\t', 'new\nline', 'é', '\u0000', undefined, null, 7, {}])(
    'refuses %j',
    (value) => {
      expect(validCapability(value)).toBe(false)
    },
  )
})

describe('the server tools file', () => {
  it('has a unique name, a description and an object schema for every tool', () => {
    expect(specs.length).toBeGreaterThan(0)
    expect(new Set(specs.map((s) => s.name)).size).toBe(specs.length)
    for (const spec of specs) {
      expect(spec.name, 'name').toMatch(/^[a-z][a-z0-9_]*$/)
      expect(typeof spec.description, spec.name).toBe('string')
      expect(spec.description.trim(), spec.name).not.toBe('')
      expect(typeof spec.read_only, spec.name).toBe('boolean')
      expect(spec.input_schema.type, spec.name).toBe('object')
    }
  })

  it('keeps the agent browser tools conversation-only', () => {
    const browser = specs.filter((s) => s.name.startsWith('browser_'))
    expect(browser.length).toBeGreaterThan(0)
    for (const spec of browser) expect(spec.conversation_only, spec.name).toBe(true)
  })
})

describe('tool selection', () => {
  afterEach(() => {
    testHooks.extraToolSpecs = []
  })

  it('offers every tool that is not conversation-only while the browser is off', () => {
    const expected = specs.filter((s) => !s.conversation_only).map((s) => s.name)
    expect(names()).toEqual(expected)
    expect(names({})).toEqual(expected)
    expect(names({ browser: false })).toEqual(expected)
    expect(names().some((n) => n.startsWith('browser_'))).toBe(false)
  })

  it("offers all of them, in the server's order, with the browser on", () => {
    expect(names({ browser: true })).toEqual(specs.map((s) => s.name))
  })

  // That the production entry (index.ts) registers no spike tools is covered by the integration test
  // TestProductionSidecarRefusesThePauseTools (internal/agentrt/tools_it_test.go): this one only
  // checks the hooks' defaults, which the test entry alone changes.
  it('names the extension, and its test hooks are off by default', () => {
    expect(tasktoTools('/unused.sock').name).toBe('taskto-tools')
    expect(testHooks.extraToolSpecs).toEqual([])
    expect(testHooks.spikeTools).toBe(false)
  })

  it("filters the integration tests' fixture tools by the same flag, after the server's", () => {
    const schema = { type: 'object', properties: {}, additionalProperties: false } as ToolSpec['input_schema']
    testHooks.extraToolSpecs = [
      {
        name: 'fixture_flagged',
        description: 'A fixture',
        read_only: true,
        conversation_only: true,
        input_schema: schema,
      },
      { name: 'browser_unflagged', description: 'A fixture', read_only: true, input_schema: schema },
    ]
    const off = names({ browser: false })
    expect(off.slice(-1)).toEqual(['browser_unflagged'])
    expect(off).not.toContain('fixture_flagged')
    const on = names({ browser: true })
    expect(on.slice(-2)).toEqual(['fixture_flagged', 'browser_unflagged'])
  })

  it('replays only the reads after a crash', () => {
    const byName = new Map(toolsOf('/unused.sock', { browser: true }).map((t) => [t.name, t.replay]))
    for (const spec of specs) expect(byName.get(spec.name), spec.name).toBe(spec.read_only ? 'safe' : 'unsafe')
    expect(specs.some((s) => s.read_only)).toBe(true)
    expect(specs.some((s) => !s.read_only)).toBe(true)
  })
})

describe('a tool call', () => {
  let dir: string
  let server: Server
  let socket: string
  let handler: (req: IncomingMessage, res: ServerResponse, body: string) => void
  const seen: { path?: string; auth?: string; body?: string }[] = []

  beforeAll(async () => {
    dir = mkdtempSync(join(tmpdir(), 'tt-agent-'))
    socket = join(dir, 't.sock')
    server = createServer((req, res) => {
      const chunks: Buffer[] = []
      req.on('data', (c: Buffer) => chunks.push(c))
      req.on('end', () => {
        const body = Buffer.concat(chunks).toString('utf8')
        seen.push({ path: req.url, auth: req.headers.authorization, body })
        handler(req, res, body)
      })
    })
    await new Promise<void>((resolve) => server.listen(socket, resolve))
  })

  afterAll(async () => {
    server.closeAllConnections()
    await new Promise((resolve) => server.close(resolve))
    rmSync(dir, { recursive: true, force: true })
  })

  afterEach(() => {
    seen.length = 0
  })

  // The tool as Pi Durable runs it, with a conversation that holds the given capability.
  async function run(name: string, args: unknown, capability: string, abort?: AbortSignal) {
    const tool = toolsOf(socket, { browser: true }).find((t) => t.name === name)
    if (!tool) throw new Error(`no tool ${name}`)
    const api = {
      conversationId: 7,
      snapshot: async (doc: unknown) => {
        expect(doc).toBe(ToolAccess)
        return { capability }
      },
    }
    // biome-ignore lint/suspicious/noExplicitAny: the execution API is Pi Durable's, and only these two members are used
    return (await tool.execute(args as never, api as any, { abortSignal: abort } as never)) as {
      output: { type: string; text: string }[]
      isError?: boolean
    }
  }

  it("posts the arguments to the tool's route with the conversation's capability, and returns the body", async () => {
    handler = (_req, res) => {
      res.writeHead(200, { 'Content-Type': 'application/json' }).end('{"spaces":[]}')
    }
    const result = await run('list_spaces', {}, 'cap-123')
    expect(result.output).toEqual([{ type: 'text', text: '{"spaces":[]}' }])
    expect(result.isError).toBeUndefined()
    expect(seen).toEqual([{ path: '/tools/list_spaces', auth: 'Bearer cap-123', body: '{}' }])
  })

  it('tells the model to wait for the next message while the conversation has no capability', async () => {
    handler = () => {
      throw new Error('the server must not be called without a capability')
    }
    const result = await run('list_spaces', {}, '')
    expect(result.isError).toBe(true)
    expect(result.output[0]?.text).toContain('not available in this conversation yet')
    expect(seen).toEqual([])
  })

  it("returns the server's refusal as an error and nothing else it sent", async () => {
    handler = (_req, res) => {
      res
        .writeHead(403, { 'Content-Type': 'application/json' })
        .end('{"error":"no access","code":"forbidden","internal":"secret detail"}')
    }
    const result = await run('list_spaces', {}, 'cap-123')
    expect(result.isError).toBe(true)
    expect(result.output[0]?.text).toBe('error: no access')
  })

  it('names the status when the refusal is not JSON', async () => {
    handler = (_req, res) => {
      res.writeHead(502).end('<html>bad gateway</html>')
    }
    const result = await run('list_spaces', {}, 'cap-123')
    expect(result.isError).toBe(true)
    expect(result.output[0]?.text).toBe('error: the server answered 502')
  })

  it('says the server is unreachable instead of throwing', async () => {
    handler = (req) => {
      req.socket.destroy()
    }
    const result = await run('list_spaces', {}, 'cap-123')
    expect(result.isError).toBe(true)
    expect(result.output[0]?.text).toBe('the server is unreachable right now: try again shortly')
  })

  it('rejects when the call was aborted, rather than reporting an unreachable server', async () => {
    const controller = new AbortController()
    let arrived!: () => void
    const requestArrived = new Promise<void>((resolve) => {
      arrived = resolve
    })
    handler = () => {
      // Never answers; the abort comes once the request is here.
      arrived()
    }
    const call = run('list_spaces', {}, 'cap-123', controller.signal)
    // Attach the assertion first so the rejection is never unhandled.
    const rejection = expect(call).rejects.toMatchObject({ name: 'AbortError' })
    await requestArrived
    controller.abort()
    await rejection
  })
})
