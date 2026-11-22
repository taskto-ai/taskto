// Model providers: read from the JSON file named by TASKTO_AGENT_PROVIDERS and registered with
// pi-ai, at startup and again on POST /providers/reload. The schema is in docs/agent-providers.md.
// Without the file, only the scripted model runs.
//
// API keys are never in the file: it names a file or an environment variable holding each key.
// Every key the sidecar reads is remembered, and redact() removes it from anything the sidecar
// writes out (responses, logs, model errors), in case an upstream echoes it back.
import { readFileSync } from 'node:fs'
import { isAbsolute } from 'node:path'
import {
  type AssistantMessage,
  type AssistantMessageEventStream,
  createAssistantMessageEventStream,
  createProvider,
  type Model,
  type OpenAICompletionsCompat,
  type ProviderStreams,
} from '@earendil-works/pi-ai'
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy'
import type { createModels } from '@earendil-works/pi-ai/models'
import { OpenAICompletionsCompatSchema } from '@earendil-works/pi-ai/providers/compat-schema'
import { compile } from '@earendil-works/pi-schema'
import { CLI_NAMES, type CliProviderConfig, installCliDriver } from './cli.js'
import { type EgressRefused, egressFetch, type Policy, parsePolicy } from './egress.js'
import { installScriptModel, SCRIPT_MODEL_ID } from './model.js'

export type ModelRef = { provider: string; modelId: string }

type KeySource = { file: string } | { env: string }

type ModelConfig = { id: string; name: string; contextWindow: number; maxTokens: number; reasoning: boolean }

export type ProviderConfig =
  | { kind: 'script'; id: string; name: string }
  | CliProviderConfig
  | {
      kind: 'openai-compatible'
      id: string
      name: string
      baseUrl: string
      apiKey?: KeySource
      compat?: OpenAICompletionsCompat
      models: ModelConfig[]
      // Where its requests may connect (egress.ts); none means anywhere.
      egress?: Policy
    }

/** What GET /models returns: no base URLs, no key sources. */
export type Catalog = {
  providers: { id: string; name: string; kind: ProviderConfig['kind']; models: { id: string; name: string }[] }[]
  default: ModelRef
}

export class ConfigError extends Error {}

const DEFAULT_CONFIG: ProviderConfig[] = [{ kind: 'script', id: 'script', name: 'Scripted (offline)' }]

/** Reads and validates the providers file; no path means the scripted model only. */
export function loadProviders(path: string | undefined): ProviderConfig[] {
  if (!path) return DEFAULT_CONFIG
  let raw: unknown
  try {
    raw = JSON.parse(readFileSync(path, 'utf8'))
  } catch (error) {
    // The file holds no secrets, but a parse error quotes it: report only what went wrong.
    const reason = error instanceof SyntaxError ? 'not valid JSON' : (error as NodeJS.ErrnoException).code
    throw new ConfigError(`cannot read providers file ${path}: ${reason}`)
  }
  return parseConfig(raw)
}

// Validation messages name fields, never their values: a misplaced key must not end up in a log.
export function parseConfig(raw: unknown): ProviderConfig[] {
  const top = object(raw, 'config')
  only(top, ['providers'], 'config')
  if (!Array.isArray(top.providers) || top.providers.length === 0) {
    throw new ConfigError('providers must be a non-empty array')
  }
  const ids = new Set<string>()
  return top.providers.map((entry, i) => {
    const at = `providers[${i}]`
    const p = object(entry, at)
    const id = p.id
    if (typeof id !== 'string' || !/^[a-z0-9][a-z0-9._-]{0,63}$/.test(id)) {
      throw new ConfigError(`${at}.id must be 1-64 characters of a-z, 0-9, '.', '_' or '-'`)
    }
    if (ids.has(id)) throw new ConfigError(`${at}.id is a duplicate`)
    ids.add(id)
    const name = optionalString(p.name, `${at}.name`) ?? id
    if (p.kind === 'script') {
      only(p, ['id', 'kind', 'name'], at)
      return { kind: 'script', id, name }
    }
    if (p.kind === 'cli') {
      only(p, ['id', 'kind', 'name', 'cli', 'models'], at)
      if (typeof p.cli !== 'string' || !(CLI_NAMES as readonly string[]).includes(p.cli)) {
        throw new ConfigError(`${at}.cli must be one of ${CLI_NAMES.join(', ')}`)
      }
      return {
        kind: 'cli',
        id,
        name,
        cli: p.cli as CliProviderConfig['cli'],
        models: models(p.models, `${at}.models`).map((m) => ({ id: m.id, name: m.name })),
      }
    }
    if (p.kind !== 'openai-compatible') throw new ConfigError(`${at}.kind must be openai-compatible, cli or script`)
    only(p, ['id', 'kind', 'name', 'baseUrl', 'apiKey', 'compat', 'models', 'egress'], at)
    const url = baseUrl(p.baseUrl, `${at}.baseUrl`)
    if (p.apiKey !== undefined && !keySafeOver(url)) {
      throw new ConfigError(`${at}.baseUrl must use https to send an API key to another host`)
    }
    return {
      kind: 'openai-compatible',
      id,
      name,
      baseUrl: url,
      ...(p.apiKey === undefined ? {} : { apiKey: keySource(p.apiKey, `${at}.apiKey`) }),
      ...(p.compat === undefined ? {} : { compat: compat(p.compat, `${at}.compat`) }),
      models: models(p.models, `${at}.models`),
      ...(p.egress === undefined ? {} : { egress: egress(p.egress, `${at}.egress`) }),
    }
  })
}

function egress(value: unknown, at: string): Policy {
  try {
    return parsePolicy(value, at)
  } catch (error) {
    throw new ConfigError((error as Error).message)
  }
}

function object(value: unknown, at: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new ConfigError(`${at} must be an object`)
  }
  return value as Record<string, unknown>
}

// Unknown fields are refused, so a key pasted into the wrong place is never silently kept.
function only(value: Record<string, unknown>, fields: string[], at: string): void {
  for (const field of Object.keys(value)) {
    if (!fields.includes(field)) throw new ConfigError(`${at} has an unknown field "${field.slice(0, 64)}"`)
  }
}

// pi-ai's own schema for these settings allows extra fields; refuse them like everywhere
// else, so a typo fails at startup instead of being silently ignored.
const compatFields = Object.keys(OpenAICompletionsCompatSchema.properties as Record<string, unknown>)
const compatValidator = compile(OpenAICompletionsCompatSchema)

function compat(value: unknown, at: string): OpenAICompletionsCompat {
  const settings = object(value, at)
  only(settings, compatFields, at)
  const [first] = compatValidator.errors(settings)
  if (first) throw new ConfigError(`${at}${first.path}: ${first.message}`)
  return settings as OpenAICompletionsCompat
}

function optionalString(value: unknown, at: string): string | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'string' || value === '' || value.length > 256) {
    throw new ConfigError(`${at} must be a string of 1-256 characters`)
  }
  return value
}

// A key may go over plain http only to this machine (a local Ollama or gateway); anywhere
// else it would cross the network in cleartext.
function keySafeOver(url: string): boolean {
  const { protocol, hostname } = new URL(url)
  if (protocol === 'https:') return true
  return hostname === 'localhost' || hostname === '[::1]' || /^127\.\d+\.\d+\.\d+$/.test(hostname)
}

function baseUrl(value: unknown, at: string): string {
  let url: URL
  try {
    url = new URL(String(value))
  } catch {
    throw new ConfigError(`${at} must be an http or https URL`)
  }
  if (typeof value !== 'string' || (url.protocol !== 'http:' && url.protocol !== 'https:')) {
    throw new ConfigError(`${at} must be an http or https URL`)
  }
  if (url.username || url.password) throw new ConfigError(`${at} must not contain credentials; use apiKey`)
  return value.replace(/\/+$/, '')
}

function keySource(value: unknown, at: string): KeySource {
  if (typeof value === 'string') throw new ConfigError(`${at} must name a file or env var, not hold the key`)
  const source = object(value, at)
  only(source, ['file', 'env'], at)
  if (typeof source.file === 'string' && source.env === undefined) {
    if (!isAbsolute(source.file)) throw new ConfigError(`${at}.file must be an absolute path`)
    return { file: source.file }
  }
  if (typeof source.env === 'string' && source.file === undefined) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(source.env)) throw new ConfigError(`${at}.env must be a variable name`)
    return { env: source.env }
  }
  throw new ConfigError(`${at} must have exactly one of file or env`)
}

function models(value: unknown, at: string): ModelConfig[] {
  if (!Array.isArray(value) || value.length === 0) throw new ConfigError(`${at} must be a non-empty array`)
  const ids = new Set<string>()
  return value.map((entry, i) => {
    const m = object(entry, `${at}[${i}]`)
    only(m, ['id', 'name', 'contextWindow', 'maxTokens', 'reasoning'], `${at}[${i}]`)
    const id = optionalString(m.id, `${at}[${i}].id`)
    if (!id) throw new ConfigError(`${at}[${i}].id is required`)
    if (ids.has(id)) throw new ConfigError(`${at}[${i}].id is a duplicate`)
    ids.add(id)
    if (m.reasoning !== undefined && typeof m.reasoning !== 'boolean') {
      throw new ConfigError(`${at}[${i}].reasoning must be a boolean`)
    }
    return {
      id,
      name: optionalString(m.name, `${at}[${i}].name`) ?? id,
      contextWindow: positiveInt(m.contextWindow, `${at}[${i}].contextWindow`) ?? 128_000,
      maxTokens: positiveInt(m.maxTokens, `${at}[${i}].maxTokens`) ?? 8_192,
      reasoning: m.reasoning === true,
    }
  })
}

function positiveInt(value: unknown, at: string): number | undefined {
  if (value === undefined) return undefined
  if (!Number.isSafeInteger(value) || (value as number) < 1) throw new ConfigError(`${at} must be a positive integer`)
  return value as number
}

// Keys seen so far. A key shorter than this would make redaction mangle ordinary text.
const secrets = new Set<string>()
const MIN_KEY_LENGTH = 8

/**
 * Replaces every key the sidecar has read with [redacted]: as is, JSON-escaped and URL-encoded.
 * Best effort by nature: an upstream that echoes a key truncated or re-encoded otherwise isn't
 * caught, but our own code never puts a key anywhere but the request's auth header.
 */
export function redact(text: string): string {
  for (const secret of secrets) {
    text = text
      .replaceAll(secret, '[redacted]')
      .replaceAll(JSON.stringify(secret).slice(1, -1), '[redacted]')
      .replaceAll(encodeURIComponent(secret), '[redacted]')
  }
  return text
}

function describe(source: KeySource): string {
  return 'file' in source ? `file ${source.file}` : `env var ${source.env}`
}

// Read on every request, so a rotated key file takes effect without a restart.
function readKey(provider: string, source: KeySource): string {
  let key: string | undefined
  if ('file' in source) {
    try {
      key = readFileSync(source.file, 'utf8').trim()
    } catch (error) {
      throw new ConfigError(
        `API key of provider ${provider}: cannot read ${describe(source)}: ${(error as NodeJS.ErrnoException).code}`,
      )
    }
  } else {
    key = process.env[source.env]?.trim()
  }
  if (!key) throw new ConfigError(`API key of provider ${provider}: ${describe(source)} is empty or unset`)
  if (key.length < MIN_KEY_LENGTH) {
    throw new ConfigError(`API key of provider ${provider}: ${describe(source)} is under ${MIN_KEY_LENGTH} characters`)
  }
  secrets.add(key)
  return key
}

function failure(model: Model<string>, errorMessage: string): AssistantMessage {
  const zero = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }
  return {
    role: 'assistant',
    content: [],
    api: model.api,
    provider: model.provider,
    model: model.id,
    usage: { ...zero, totalTokens: 0, cost: { ...zero, total: 0 } },
    stopReason: 'error',
    errorMessage,
    timestamp: Date.now(),
  }
}

// pi-ai streams report failures as an error event carrying the upstream's message. Redact it
// before Pi Durable stores it, so a key an upstream echoes never reaches the transcript.
//
// With an egress policy, every request goes through a fetch that connects only where it
// allows. A refused connection fails the request like any other (the OpenAI client calls it a
// "Connection error"), so the error then says what the policy refused instead.
function redacting(api: ProviderStreams, policy?: Policy): ProviderStreams {
  const fetcher = policy ? egressFetch(policy) : undefined
  const wrap = (
    source: AssistantMessageEventStream,
    model: Model<string>,
    refusal: () => EgressRefused | undefined,
  ): AssistantMessageEventStream => {
    const out = createAssistantMessageEventStream()
    void (async () => {
      for await (const event of source) {
        const refused = event.type === 'error' ? refusal() : undefined
        if (event.type === 'error' && refused) {
          console.warn(`provider ${model.provider}: ${refused.message}`)
          out.push({ ...event, error: { ...event.error, errorMessage: refused.message } })
        } else if (event.type === 'error' && event.error.errorMessage) {
          out.push({ ...event, error: { ...event.error, errorMessage: redact(event.error.errorMessage) } })
        } else {
          out.push(event)
        }
      }
      out.end()
    })().catch((error: unknown) => {
      // A stream that throws instead of reporting an error event: fail the request visibly,
      // with a redacted message, rather than ending as if the answer were complete.
      const message = refusal()?.message ?? redact(error instanceof Error ? error.message : String(error))
      console.error(`provider stream failed: ${message}`)
      out.push({ type: 'error', reason: 'error', error: failure(model, `provider stream failed: ${message}`) })
      out.end()
    })
    return out
  }
  // Each request gets a fetch of its own, to hear of its own refusals.
  const guard = <T extends { fetch?: typeof globalThis.fetch }>(options: T | undefined) => {
    let refused: EgressRefused | undefined
    const guarded = fetcher ? { ...options, fetch: fetcher((r) => (refused = r)) } : options
    return { options: guarded as T | undefined, refusal: () => refused }
  }
  return {
    stream: (model, context, options) => {
      const g = guard(options)
      return wrap(api.stream(model, context, g.options), model, g.refusal)
    },
    streamSimple: (model, context, options) => {
      const g = guard(options)
      return wrap(api.streamSimple(model, context, g.options), model, g.refusal)
    },
  }
}

/**
 * Registers the providers with pi-ai and returns what GET /models lists. Each key is read once
 * here, so a missing key stops the start instead of failing every request later.
 *
 * On a reload, previous is the catalog in use: providers it has and config doesn't are removed.
 * Every check (keys, at least one model) runs before anything is registered, so a refused config
 * leaves the registered providers as they were.
 */
export function installProviders(
  models: ReturnType<typeof createModels>,
  config: ProviderConfig[],
  previous?: Catalog,
): Catalog {
  // parseConfig refuses an empty list and a provider without models; this guards other callers.
  if (config.length === 0) throw new ConfigError('no models configured')
  for (const p of config) {
    if (p.kind === 'openai-compatible' && p.apiKey) readKey(p.id, p.apiKey)
  }
  const catalog: Catalog['providers'] = []
  for (const p of config) {
    if (p.kind === 'script') {
      installScriptModel(models, p.id)
      catalog.push({ id: p.id, name: p.name, kind: p.kind, models: [{ id: SCRIPT_MODEL_ID, name: 'Scripted' }] })
      continue
    }
    if (p.kind === 'cli') {
      installCliDriver(models, p)
      catalog.push({ id: p.id, name: p.name, kind: p.kind, models: p.models })
      continue
    }
    const { apiKey } = p
    const list: Model<'openai-completions'>[] = p.models.map((m) => ({
      id: m.id,
      name: m.name,
      api: 'openai-completions',
      provider: p.id,
      baseUrl: p.baseUrl,
      reasoning: m.reasoning,
      input: ['text'],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: m.contextWindow,
      maxTokens: m.maxTokens,
      // Local servers (Ollama, vLLM, SGLang) commonly lack these; the config can turn them back on.
      compat: { supportsDeveloperRole: false, supportsReasoningEffort: false, ...p.compat },
    }))
    models.setProvider(
      createProvider({
        id: p.id,
        name: p.name,
        baseUrl: p.baseUrl,
        auth: {
          apiKey: {
            name: `${p.name} API key`,
            // A keyless server (a local Ollama) still needs a placeholder: the OpenAI client insists on one.
            resolve: async () => ({ auth: { apiKey: apiKey ? readKey(p.id, apiKey) : 'unused' } }),
          },
        },
        models: list,
        api: redacting(openAICompletionsApi(), p.egress),
      }),
    )
    catalog.push({ id: p.id, name: p.name, kind: p.kind, models: p.models.map((m) => ({ id: m.id, name: m.name })) })
  }
  for (const old of previous?.providers ?? []) {
    if (!config.some((p) => p.id === old.id)) models.deleteProvider(old.id)
  }
  const first = catalog[0]?.models[0]
  if (!first || !catalog[0]) throw new ConfigError('no models configured')
  return { providers: catalog, default: { provider: catalog[0].id, modelId: first.id } }
}

/** Whether the catalog lists the model; GET /models and model choices agree by construction. */
export function hasModel(catalog: Catalog, ref: ModelRef): boolean {
  return catalog.providers.some((p) => p.id === ref.provider && p.models.some((m) => m.id === ref.modelId))
}
