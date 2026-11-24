import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createModels } from '@earendil-works/pi-ai/models'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import {
  ConfigError,
  hasModel,
  installProviders,
  loadProviders,
  type ProviderConfig,
  parseConfig,
  redact,
} from './providers.js'

// Distinct, obviously-not-real keys: long enough for the sidecar to take them, and built from
// parts so no scanner sees one as a secret.
const key = (name: string) => `${name}-unit-test-key-0000`

const compatible = (extra: Record<string, unknown> = {}) => ({
  id: 'local',
  kind: 'openai-compatible',
  baseUrl: 'http://localhost:11434/v1',
  models: [{ id: 'llama' }],
  ...extra,
})

function messageOf(fn: () => unknown): string {
  try {
    fn()
  } catch (error) {
    expect(error).toBeInstanceOf(ConfigError)
    return (error as Error).message
  }
  throw new Error('expected a ConfigError')
}

describe('parseConfig', () => {
  it('reads a script provider, with its id as the name', () => {
    expect(parseConfig({ providers: [{ id: 'script', kind: 'script' }] })).toEqual([
      { kind: 'script', id: 'script', name: 'script' },
    ])
  })

  it('reads a CLI provider', () => {
    const [p] = parseConfig({
      providers: [
        { id: 'cc', kind: 'cli', name: 'Claude Code', cli: 'claude', models: [{ id: 'opus', name: 'Opus' }] },
      ],
    })
    expect(p).toEqual({
      kind: 'cli',
      id: 'cc',
      name: 'Claude Code',
      cli: 'claude',
      models: [{ id: 'opus', name: 'Opus' }],
    })
  })

  it('reads an OpenAI-compatible provider with the defaults filled in', () => {
    const [p] = parseConfig({ providers: [compatible()] })
    expect(p).toEqual({
      kind: 'openai-compatible',
      id: 'local',
      name: 'local',
      baseUrl: 'http://localhost:11434/v1',
      models: [{ id: 'llama', name: 'llama', contextWindow: 128_000, maxTokens: 8_192, reasoning: false }],
    })
  })

  it('keeps what the config sets: a key source, compat flags, an egress policy, limits', () => {
    const [p] = parseConfig({
      providers: [
        compatible({
          name: 'Local',
          baseUrl: 'https://gateway.example.com/v1///',
          apiKey: { env: 'GATEWAY_KEY' },
          compat: { supportsDeveloperRole: true },
          egress: ['public'],
          models: [{ id: 'm', name: 'M', contextWindow: 32_000, maxTokens: 2_000, reasoning: true }],
        }),
      ],
    })
    expect(p).toMatchObject({
      name: 'Local',
      baseUrl: 'https://gateway.example.com/v1',
      apiKey: { env: 'GATEWAY_KEY' },
      compat: { supportsDeveloperRole: true },
      egress: { public: true, hosts: [], prefixes: [] },
      models: [{ id: 'm', name: 'M', contextWindow: 32_000, maxTokens: 2_000, reasoning: true }],
    })
  })

  it.each([
    [null, 'config must be an object'],
    [[], 'config must be an object'],
    [{}, 'providers must be a non-empty array'],
    [{ providers: [] }, 'providers must be a non-empty array'],
    [{ providers: [], extra: 1 }, 'config has an unknown field "extra"'],
    [{ providers: [7] }, 'providers[0] must be an object'],
    [{ providers: [{ kind: 'script' }] }, "providers[0].id must be 1-64 characters of a-z, 0-9, '.', '_' or '-'"],
    [
      { providers: [{ id: 'Upper', kind: 'script' }] },
      "providers[0].id must be 1-64 characters of a-z, 0-9, '.', '_' or '-'",
    ],
    [{ providers: [{ id: 'a'.repeat(65), kind: 'script' }] }, 'providers[0].id must be 1-64 characters'],
    [{ providers: [{ id: 'a', kind: 'script', name: '' }] }, 'providers[0].name must be a string of 1-256 characters'],
    [{ providers: [{ id: 'a', kind: 'script', baseUrl: 'x' }] }, 'providers[0] has an unknown field "baseUrl"'],
    [{ providers: [{ id: 'a', kind: 'nope' }] }, 'providers[0].kind must be openai-compatible, cli or script'],
    [
      {
        providers: [
          { id: 'a', kind: 'script' },
          { id: 'a', kind: 'script' },
        ],
      },
      'providers[1].id is a duplicate',
    ],
    [
      { providers: [{ id: 'c', kind: 'cli', cli: 'vim', models: [{ id: 'm' }] }] },
      'providers[0].cli must be one of claude, codex, grok',
    ],
    [{ providers: [{ id: 'c', kind: 'cli', cli: 'claude' }] }, 'providers[0].models must be a non-empty array'],
  ])('refuses %j', (raw, message) => {
    expect(messageOf(() => parseConfig(raw))).toContain(message)
  })

  it.each([
    [{ baseUrl: 'ftp://example.com' }, 'providers[0].baseUrl must be an http or https URL'],
    [{ baseUrl: 'not a url' }, 'providers[0].baseUrl must be an http or https URL'],
    [{ baseUrl: 7 }, 'providers[0].baseUrl must be an http or https URL'],
    [{ baseUrl: 'https://user:pw@example.com/v1' }, 'must not contain credentials; use apiKey'],
    [{ models: [] }, 'providers[0].models must be a non-empty array'],
    [{ models: [{}] }, 'providers[0].models[0].id is required'],
    [{ models: [{ id: 'a' }, { id: 'a' }] }, 'providers[0].models[1].id is a duplicate'],
    [{ models: [{ id: 'a', contextWindow: 0 }] }, 'contextWindow must be a positive integer'],
    [{ models: [{ id: 'a', maxTokens: 1.5 }] }, 'maxTokens must be a positive integer'],
    [{ models: [{ id: 'a', reasoning: 'yes' }] }, 'reasoning must be a boolean'],
    [{ models: [{ id: 'a', extra: 1 }] }, 'providers[0].models[0] has an unknown field "extra"'],
    [{ compat: { notASetting: true } }, 'providers[0].compat has an unknown field "notASetting"'],
    [{ egress: [] }, 'providers[0].egress must be a non-empty array of entries'],
    [{ egress: ['any'] }, 'leave providers[0].egress out to allow any address'],
    [{ unknown: 1 }, 'providers[0] has an unknown field "unknown"'],
  ])('refuses an OpenAI-compatible provider with %j', (extra, message) => {
    expect(messageOf(() => parseConfig({ providers: [compatible(extra)] }))).toContain(message)
  })

  describe('API keys', () => {
    it('are named by an absolute file or an environment variable, never held', () => {
      expect(parseConfig({ providers: [compatible({ apiKey: { file: '/run/keys/local' } })] })[0]).toMatchObject({
        apiKey: { file: '/run/keys/local' },
      })
      expect(parseConfig({ providers: [compatible({ apiKey: { env: 'LOCAL_KEY' } })] })[0]).toMatchObject({
        apiKey: { env: 'LOCAL_KEY' },
      })
    })

    it('are refused when written into the file, without quoting them', () => {
      const secret = key('pasted')
      const message = messageOf(() => parseConfig({ providers: [compatible({ apiKey: secret })] }))
      expect(message).toBe('providers[0].apiKey must name a file or env var, not hold the key')
      expect(message).not.toContain(secret)
    })

    it.each([
      [{ file: 'relative/key' }, 'providers[0].apiKey.file must be an absolute path'],
      [{ env: '1BAD' }, 'providers[0].apiKey.env must be a variable name'],
      [{ env: 'A B' }, 'providers[0].apiKey.env must be a variable name'],
      [{ file: '/k', env: 'K' }, 'providers[0].apiKey must have exactly one of file or env'],
      [{}, 'providers[0].apiKey must have exactly one of file or env'],
      [{ value: 'x' }, 'providers[0].apiKey has an unknown field "value"'],
    ])('refuse %j', (apiKey, message) => {
      expect(messageOf(() => parseConfig({ providers: [compatible({ apiKey })] }))).toContain(message)
    })

    it('go over plain http only to this machine', () => {
      const apiKey = { env: 'K' }
      for (const baseUrl of [
        'http://localhost:1/v1',
        'http://127.0.0.1/v1',
        'http://127.9.9.9/v1',
        'http://[::1]:8080/v1',
      ]) {
        expect(() => parseConfig({ providers: [compatible({ baseUrl, apiKey })] }), baseUrl).not.toThrow()
      }
      for (const baseUrl of [
        'http://gateway.example.com/v1',
        'http://10.0.0.5/v1',
        'http://localhost.example.com/v1',
      ]) {
        expect(
          messageOf(() => parseConfig({ providers: [compatible({ baseUrl, apiKey })] })),
          baseUrl,
        ).toContain('must use https to send an API key to another host')
      }
      // Without a key, plain http anywhere is fine.
      expect(() => parseConfig({ providers: [compatible({ baseUrl: 'http://10.0.0.5/v1' })] })).not.toThrow()
    })
  })
})

describe('loadProviders', () => {
  let dir: string
  beforeAll(() => {
    dir = mkdtempSync(join(tmpdir(), 'tt-providers-'))
  })
  afterAll(() => {
    rmSync(dir, { recursive: true, force: true })
  })

  it('is the scripted model alone without a file', () => {
    expect(loadProviders(undefined)).toEqual([{ kind: 'script', id: 'script', name: 'Scripted (offline)' }])
    expect(loadProviders('')).toEqual(loadProviders(undefined))
  })

  it('reads and validates a file', () => {
    const path = join(dir, 'ok.json')
    writeFileSync(path, JSON.stringify({ providers: [compatible()] }))
    expect(loadProviders(path)).toHaveLength(1)
  })

  it('reports a file it cannot read by its error code', () => {
    const path = join(dir, 'missing.json')
    expect(messageOf(() => loadProviders(path))).toBe(`cannot read providers file ${path}: ENOENT`)
  })

  it('reports a file that is not JSON without quoting it', () => {
    const path = join(dir, 'bad.json')
    writeFileSync(path, '{"providers": [ "oops-this-could-be-a-key-0000" ')
    const message = messageOf(() => loadProviders(path))
    expect(message).toBe(`cannot read providers file ${path}: not valid JSON`)
    expect(message).not.toContain('oops')
  })
})

describe('installProviders and redact', () => {
  let dir: string
  beforeAll(() => {
    dir = mkdtempSync(join(tmpdir(), 'tt-install-'))
    mkdirSync(join(dir, 'keys'))
  })
  afterAll(() => {
    rmSync(dir, { recursive: true, force: true })
  })

  const config = (...providers: Record<string, unknown>[]): ProviderConfig[] => parseConfig({ providers })

  it('lists the scripted model, a CLI and a compatible provider, and defaults to the first model', () => {
    const models = createModels()
    const catalog = installProviders(
      models,
      config(
        { id: 'script', kind: 'script', name: 'Scripted' },
        { id: 'cc', kind: 'cli', cli: 'claude', models: [{ id: 'opus' }] },
        compatible({ models: [{ id: 'llama', name: 'Llama' }] }),
      ),
    )
    expect(catalog.default).toEqual({ provider: 'script', modelId: 'script-1' })
    expect(catalog.providers.map((p) => [p.id, p.kind, p.models.map((m) => m.id)])).toEqual([
      ['script', 'script', ['script-1']],
      ['cc', 'cli', ['opus']],
      ['local', 'openai-compatible', ['llama']],
    ])
    expect(models.getModel('local', 'llama')).toBeDefined()
    expect(models.getModel('script', 'script-1')).toBeDefined()
  })

  it('leaves out of the catalog every URL and key source', () => {
    const catalog = installProviders(
      createModels(),
      config(compatible({ baseUrl: 'https://gateway.example.com/v1', apiKey: { env: 'PATH' } })),
    )
    const text = JSON.stringify(catalog)
    expect(text).not.toContain('gateway.example.com')
    expect(text).not.toContain('PATH')
  })

  it('turns the compat defaults for local servers off, unless the config turns them on', () => {
    const models = createModels()
    installProviders(
      models,
      config(compatible({ id: 'plain' }), compatible({ id: 'chatty', compat: { supportsDeveloperRole: true } })),
    )
    const compat = (id: string) => (models.getModel(id, 'llama') as { compat?: Record<string, unknown> }).compat
    expect(compat('plain')).toMatchObject({ supportsDeveloperRole: false, supportsReasoningEffort: false })
    expect(compat('chatty')).toMatchObject({ supportsDeveloperRole: true, supportsReasoningEffort: false })
  })

  it('refuses an empty config', () => {
    expect(messageOf(() => installProviders(createModels(), []))).toBe('no models configured')
  })

  it('reads every key at the start, so a missing one stops it before anything is registered', () => {
    const models = createModels()
    const path = join(dir, 'keys', 'missing')
    expect(
      messageOf(() =>
        installProviders(
          models,
          config({ id: 'first', kind: 'script' }, compatible({ id: 'keyed', apiKey: { file: path } })),
        ),
      ),
    ).toBe(`API key of provider keyed: cannot read file ${path}: ENOENT`)
    expect(models.getProvider('first')).toBeUndefined()
    expect(models.getProvider('keyed')).toBeUndefined()
  })

  it('refuses an unset, empty or short key, naming where it was and never its value', () => {
    const empty = join(dir, 'keys', 'empty')
    writeFileSync(empty, '  \n')
    const short = join(dir, 'keys', 'short')
    writeFileSync(short, 'abc1234\n')
    const install = (apiKey: unknown) => () => installProviders(createModels(), config(compatible({ id: 'k', apiKey })))
    expect(messageOf(install({ env: 'TASKTO_UNIT_TEST_UNSET_VARIABLE' }))).toBe(
      'API key of provider k: env var TASKTO_UNIT_TEST_UNSET_VARIABLE is empty or unset',
    )
    expect(messageOf(install({ file: empty }))).toBe(`API key of provider k: file ${empty} is empty or unset`)
    const message = messageOf(install({ file: short }))
    expect(message).toBe(`API key of provider k: file ${short} is under 8 characters`)
    expect(message).not.toContain('abc1234')
  })

  it('removes the providers a reload no longer has, and only on success', () => {
    const models = createModels()
    const before = installProviders(models, config({ id: 'one', kind: 'script' }, compatible({ id: 'two' })))
    expect(models.getProvider('two')).toBeDefined()

    // A refused config leaves what was registered as it was.
    expect(() =>
      installProviders(
        models,
        config(compatible({ id: 'three', apiKey: { env: 'TASKTO_UNIT_TEST_UNSET_VARIABLE' } })),
        before,
      ),
    ).toThrow(ConfigError)
    expect(models.getProvider('one')).toBeDefined()
    expect(models.getProvider('two')).toBeDefined()
    expect(models.getProvider('three')).toBeUndefined()

    const after = installProviders(models, config({ id: 'one', kind: 'script' }), before)
    expect(models.getProvider('one')).toBeDefined()
    expect(models.getProvider('two')).toBeUndefined()
    expect(after.providers.map((p) => p.id)).toEqual(['one'])
  })

  it('knows which models the catalog lists', () => {
    const catalog = installProviders(createModels(), config({ id: 'one', kind: 'script' }, compatible()))
    expect(hasModel(catalog, { provider: 'local', modelId: 'llama' })).toBe(true)
    expect(hasModel(catalog, { provider: 'one', modelId: 'script-1' })).toBe(true)
    expect(hasModel(catalog, { provider: 'local', modelId: 'other' })).toBe(false)
    expect(hasModel(catalog, { provider: 'nope', modelId: 'llama' })).toBe(false)
  })

  it('redacts a key it has read as it is, JSON-escaped and URL-encoded', () => {
    // Characters that JSON and URLs write differently.
    const secret = `${key('redact')}"/+ ü`
    const path = join(dir, 'keys', 'redact')
    writeFileSync(path, `${secret}\n`)
    installProviders(createModels(), config(compatible({ id: 'redact', apiKey: { file: path } })))

    expect(redact(`bad key ${secret} given`)).toBe('bad key [redacted] given')
    expect(redact(`{"error":${JSON.stringify(secret)}}`)).toBe('{"error":"[redacted]"}')
    expect(redact(`?key=${encodeURIComponent(secret)}`)).toBe('?key=[redacted]')
    expect(redact(`${secret} and ${secret}`)).toBe('[redacted] and [redacted]')
    expect(redact('nothing to hide here')).toBe('nothing to hide here')
  })

  it('does not redact a key that was never read', () => {
    expect(redact(key('never-read'))).toBe(key('never-read'))
  })
})

// A fake OpenAI-compatible server that refuses every request and echoes the key it was sent, as
// some upstreams do, to check the error that reaches the transcript.
describe('a request that fails upstream', () => {
  let server: Server
  let baseUrl: string
  let dir: string
  const seen: string[] = []

  beforeAll(async () => {
    dir = mkdtempSync(join(tmpdir(), 'tt-upstream-'))
    server = createServer((req, res) => {
      seen.push(String(req.headers.authorization))
      res.writeHead(401, { 'Content-Type': 'application/json' })
      res.end(
        JSON.stringify({ error: { message: `Incorrect API key provided: ${req.headers.authorization?.slice(7)}` } }),
      )
    })
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
    baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`
  })

  afterAll(async () => {
    server.closeAllConnections()
    await new Promise((resolve) => server.close(resolve))
    rmSync(dir, { recursive: true, force: true })
  })

  afterEach(() => {
    seen.length = 0
  })

  const ask = (models: ReturnType<typeof createModels>, provider: string) => {
    const model = models.getModel(provider, 'm')
    if (!model) throw new Error('no model')
    return models.completeSimple(model, { messages: [{ role: 'user', content: 'hi', timestamp: Date.now() }] })
  }

  it('never lets the key it sent reach the error message', async () => {
    const secret = key('echoed')
    const path = join(dir, 'echoed')
    writeFileSync(path, `${secret}\n`)
    const models = createModels()
    installProviders(
      models,
      parseConfig({
        providers: [
          { id: 'up', kind: 'openai-compatible', baseUrl, apiKey: { file: path }, models: [{ id: 'm' }], compat: {} },
        ],
      }),
    )
    const reply = await ask(models, 'up')
    expect(seen.at(0)).toBe(`Bearer ${secret}`)
    expect(reply.stopReason).toBe('error')
    expect(reply.errorMessage).toBeTruthy()
    expect(reply.errorMessage).not.toContain(secret)
    expect(reply.errorMessage).toContain('[redacted]')
  })

  it('reads the key file on every request, so a rotated key takes effect without a restart', async () => {
    const path = join(dir, 'rotated')
    const first = key('first')
    const second = key('second')
    writeFileSync(path, `${first}\n`)
    const models = createModels()
    installProviders(
      models,
      parseConfig({
        providers: [{ id: 'rot', kind: 'openai-compatible', baseUrl, apiKey: { file: path }, models: [{ id: 'm' }] }],
      }),
    )
    await ask(models, 'rot')
    writeFileSync(path, `${second}\n`)
    await ask(models, 'rot')
    expect(seen).toEqual([`Bearer ${first}`, `Bearer ${second}`])
  })

  it('says what the egress policy refused instead of a connection error', async () => {
    // The sidecar logs each refusal; keep the test's output clean and check it did.
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const models = createModels()
    installProviders(
      models,
      parseConfig({
        providers: [{ id: 'walled', kind: 'openai-compatible', baseUrl, models: [{ id: 'm' }], egress: ['public'] }],
      }),
    )
    const reply = await ask(models, 'walled')
    expect(seen).toEqual([])
    expect(reply.stopReason).toBe('error')
    expect(reply.errorMessage).toMatch(/egress policy doesn't allow this endpoint: 127\.0\.0\.1 is a loopback address/)
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('provider walled: '))
    warn.mockRestore()
  })

  it('goes through when the policy allows the address', async () => {
    const models = createModels()
    installProviders(
      models,
      parseConfig({
        providers: [{ id: 'open', kind: 'openai-compatible', baseUrl, models: [{ id: 'm' }], egress: ['127.0.0.0/8'] }],
      }),
    )
    const reply = await ask(models, 'open')
    expect(seen).toHaveLength(1)
    expect(reply.errorMessage).not.toMatch(/egress policy/)
  })
})
