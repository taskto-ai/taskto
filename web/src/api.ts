// The server's JSON API (docs/api.md). Requests with a body send JSON, which the CSRF guard
// requires, or a file's bytes as application/octet-stream to the one route that takes them;
// the session is the cookie the browser keeps, so nothing here holds a token.

import type { Rule } from './apps/rules'
import { has, type Key, type Params, t } from './i18n'

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    // The whole answer, for the errors that carry more (a board's 409 has `current`).
    readonly body?: unknown,
  ) {
    super(message)
  }

  // The error's stable code ("invite_wrong_email"), which the web app translates as
  // errors.<code>; its params fill the message's placeholders.
  get code(): string | undefined {
    const code = (this.body as { code?: unknown } | undefined)?.code
    return typeof code === 'string' ? code : undefined
  }

  get params(): Params | undefined {
    const params = (this.body as { params?: unknown } | undefined)?.params
    return params && typeof params === 'object' ? (params as Params) : undefined
  }
}

export const SESSION_LOST = 'taskto:session-lost'

// The account a request is meant for (docs/api.md): what is sent on behalf of an account that may
// not be the one the browser's session belongs to any more (the offline queue) says whose it is,
// and the server refuses it for anyone else, with `session_account_changed`.
export const EXPECTED_USER_HEADER = 'X-Taskto-Expected-User'

export const forAccount = (user: number): Record<string, string> => ({ [EXPECTED_USER_HEADER]: String(user) })

// A request made for an account was refused because the session is another one's: said to the page
// (detail.expected is the account the request was for), whose offline view of that account stops.
export const ACCOUNT_CHANGED_EVENT = 'taskto:account-changed'
const ACCOUNT_CHANGED = 'session_account_changed'

export type RequestInit = {
  method?: string
  body?: unknown
  bytes?: Blob
  headers?: Record<string, string>
  signal?: AbortSignal
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const extra = init.headers ?? {}
  const res = await fetch(path, {
    method: init.method ?? 'GET',
    ...(init.signal ? { signal: init.signal } : {}),
    ...(init.bytes !== undefined
      ? { headers: { ...extra, 'Content-Type': 'application/octet-stream' }, body: init.bytes }
      : init.body !== undefined
        ? { headers: { ...extra, 'Content-Type': 'application/json' }, body: JSON.stringify(init.body) }
        : Object.keys(extra).length > 0
          ? { headers: extra }
          : {}),
  })
  if (res.status === 204) return undefined as T
  // An answer that isn't JSON is no data; a body that was cut short because the request was
  // aborted is not an answer at all, and says so (a caller that aborts never takes it for one).
  const data = await res.json().catch((e) => {
    if (init.signal?.aborted || (e as { name?: string } | null)?.name === 'AbortError') throw e
    return null
  })
  if (!res.ok) {
    // A session that ended (logged out elsewhere, expired) sends the app back to the login.
    if (res.status === 401) window.dispatchEvent(new Event(SESSION_LOST))
    if (res.status === 409 && data?.code === ACCOUNT_CHANGED) {
      const expected = Number(extra[EXPECTED_USER_HEADER])
      window.dispatchEvent(new CustomEvent(ACCOUNT_CHANGED_EVENT, { detail: { expected } }))
    }
    const message = data && typeof data.error === 'string' ? data.error : `${res.status} ${res.statusText}`
    throw new ApiError(res.status, message, data)
  }
  // T is the caller's word for what the server sends. The types that api.contract.ts lists are
  // checked against the Go structs that make the replies, so a renamed field of one fails the
  // build; the others are not.
  return data as T
}

// What went wrong, for people: an API error in the user's language when the web app knows its
// code, else the server's own words, else its status.
//
// fieldLabels says what a form calls its fields (by their names in the app): an error about a
// field names it as the person sees it ("Notes (needed when it fails)"), not as the app's data does.
export function errorText(e: unknown, fieldLabels?: ReadonlyMap<string, string>): string {
  if (e instanceof ApiError) {
    const key = e.code && `errors.${e.code}`
    if (key && has(key)) {
      const field = e.params?.field
      const label = typeof field === 'string' ? fieldLabels?.get(field) : undefined
      return t(key as Key, label ? { ...e.params, field: label } : e.params)
    }
    const said = (e.body as { error?: unknown } | undefined)?.error
    if (typeof said !== 'string') return t('errors.http', { status: String(e.status) })
  }
  return e instanceof Error ? e.message : String(e)
}

// A code the server sends in an answer that isn't an error (a test's outcome, why something
// is off), in the user's language when the web app knows it, else the server's own words.
export function codeText(code: string | undefined, params: Params | undefined, said: string): string {
  const key = code && `errors.${code}`
  return key && has(key) ? t(key as Key, params) : said
}

// `locale`: the workspace's default language, a BCP 47 tag, or "" for English.
export type Workspace = { id: number; name: string; role: 'admin' | 'member'; locale?: string }
// `user.locale`: the language they picked, or "" for their browser's.
// `can_create_workspace`: whether this server lets them make a workspace (-workspace-creation); older
// servers don't send it.
// `is_operator`: whether they run the server (an admin of the workspace setup made), who alone see what
// belongs to the whole server: its backups and its outgoing email.
export type Me = {
  user: { id: number; email: string; name: string; locale?: string }
  workspaces: Workspace[]
  can_create_workspace: boolean
  is_operator: boolean
}

export type Conversation = {
  id: number
  workspace_id: number
  title: string
  archived_at: string | null
  created_at: string
  updated_at: string
  // On a CLI worker: whether the CLI asks before it acts (docs/agent-safety.md). Older
  // servers don't send it.
  cli_permissions?: 'ask' | 'allow_all'
}

// A CLI's question to the conversation's owner: may it use this tool?
export type CliQuestion = { id: string; turn: string; cli: string; tool: string; input?: string; asked_at: string }

export type Space = {
  id: number
  workspace_id: number
  kind: 'private' | 'shared'
  name: string
  archived_at: string | null
  role?: 'owner' | 'editor' | 'viewer'
  access: 'write' | 'read' | 'none'
  // Whether the space's apps may be downloaded for offline use (docs/offline.md); an admin turns
  // it off for a shared space. Older servers don't send it.
  offline_downloads?: boolean
}

// `versioned` is false for large files and audio and video: only their current bytes are kept,
// and they come without content (download them with ?raw=true).
export type Doc = {
  path: string
  version: string
  size: number
  versioned: boolean
  content?: string
  content_base64?: string
}

export type Board = {
  id: number
  space_id: number
  kind: 'personal' | 'team'
  name: string
  archived_at: string | null
  version: number
}

// Who made a change: `agent` when the agent made it for them, `via` an access token's name.
export type Author = { id: number; name: string; agent: boolean; via?: string } | null

export type Card = {
  id: number
  board_id: number
  title: string
  body: string
  column: 'backlog' | 'todo' | 'doing' | 'review' | 'done' | 'archived'
  archived_from: Card['column'] | null
  assignee: { kind: 'agent' } | { kind: 'user'; id: number; name?: string } | null
  blocked: boolean
  blocked_reason: string | null
  blocked_by: number[]
  docs: string[]
  // Conversation IDs in decimal: only a conversation's owner can add one, or open it.
  conversations: string[]
  // A calendar date, YYYY-MM-DD, with no time or time zone; null for none.
  due: string | null
  // Free-text labels: lower case, sorted.
  labels: string[]
  version: number
  updated_at: string
  updated_by: Author
}

// The roles of an app, inside its space: who may build, publish and use it, and read its records.
export type AppRole = 'builder' | 'publisher' | 'user' | 'data_manager' | 'reader'
export type RecordScope = 'none' | 'own' | 'all'

// What the caller may do in an app (the server works it out: the space's rules, then the roles).
export type AppAccess = {
  roles: AppRole[]
  restricted: boolean
  view: boolean
  design: boolean
  publish: boolean
  submit: boolean
  manage_roles: boolean
  read_records: RecordScope
  modify_records: RecordScope
}

// Roles of one user (or, with a null user_id, of every member without roles of their own).
export type AppGrant = { user_id: number | null; roles: AppRole[] }

// What a client needs to draw a definition: the version of the definition's format and the catalog
// capabilities its screen uses ("component.Stat", "function.formatDate").
export type DefinitionNeeds = { definition_version: number; requires: string[] }

// One published, immutable definition of an app. The app serves its live release.
export type AppRelease = DefinitionNeeds & {
  number: number
  published_at: string
  published_by: Author
  live: boolean
  live_since?: string
  // In a single app's reply, for the live release.
  definition?: AppDefinition
}

// A field of an app's collection, as its definition declares it.
export type FieldType =
  | 'string'
  | 'number'
  | 'boolean'
  | 'date'
  | 'time'
  | 'choice'
  | 'attachment'
  | 'location'
  | 'signature'
  | 'barcode'
export type AppField = {
  name: string
  type: FieldType
  required?: boolean
  max_length?: number
  min?: number
  max?: number
  options?: string[]
  // A choice's labels, by option: how a stored option reads to people (the option itself where it has none).
  option_labels?: Record<string, string>
  // A location field: when it is taken ('button' or 'submit'); a barcode's format; and whether a stored
  // location or signature can be changed (otherwise it is evidence).
  capture?: 'button' | 'submit'
  format?: string
  editable?: boolean
}

// What the records explorer needs of an app's definition: its fields, and which actions change records.
export type AppDefinition = { collection: { fields: AppField[] }; actions: Record<string, { kind: string }> }

// A change of a collection a publish must acknowledge, by kind and field.
export type SchemaChange = { change: string; field: string }

// The parts of an app's definition the builder tells people about: what its rows hold (AppField)
// and what its buttons may do to them. Nothing else of a definition is read here.
export type AppActionKind = 'insert' | 'update' | 'delete' | 'navigate'
export type AppDraft = DefinitionNeeds & {
  collection?: { fields: AppField[] }
  actions?: Record<string, { kind: AppActionKind; effects?: DraftEffect[] }>
}

// An effect an action of a definition declares (internal/apps/effects.go): what a change sets going
// on a board or in the documents of a space. Its target is by ID; the names are the app's
// `effect_targets`.
export type DraftEffect = {
  kind: EffectKind
  name?: string
  when?: Rule
  space_id?: number
  board_id?: number
  column?: string
  card?: string
  path?: string
  route?: 'board' | 'card' | 'document'
}

// The space, and board, an effect of the draft acts on, with the names the caller may be told (none
// for a space they can't read: docs/api.md).
export type EffectTarget = {
  space_id: number
  space?: string
  space_kind?: 'private' | 'shared'
  board_id?: number
  board?: string
}

// One sample row of a draft's preview: it lives in the browser, never on the server's disk.
export type PreviewRow = { id: number; revision: number; data: Record<string, unknown> }
// effects: what the action would have set going on boards and documents, simulated on the same rows.
export type PreviewReply = { messages: unknown[]; rows: PreviewRow[]; effects?: AppEffect[] }

export type App = {
  id: number
  space_id: number
  name: string
  archived_at: string | null
  // The app's version for optimistic changes: bumped by every change, a publish included.
  version: number
  // Rows the caller may read.
  rows: number
  access: AppAccess
  // The release people use; null until the app's first publish.
  live: AppRelease | null
  // The draft differs from the live release (told only to those who may design or publish).
  unpublished_changes: boolean
  // The definition being edited, in a single app's reply, for those who may design or publish.
  draft?: AppDraft
  // The spaces and boards the draft's effects act on, by name when the caller may read them; with the draft.
  effect_targets?: EffectTarget[]
  // The draft has effects with no board chosen yet (a template's, or a copy's whose boards its copier
  // can't write): it can't be published until one is. Told to those who may design or publish.
  needs_board?: boolean
}

// A starter app the server comes with (docs/templates.md).
export type AppTemplate = {
  id: string
  version: number
  name: string
  description: string
  recommended_space: 'private' | 'shared'
  needs_board: boolean
  // The kinds of field that use the phone's own hardware.
  captures: ('attachment' | 'location' | 'signature' | 'barcode')[]
  // The role assignments an install makes in a shared space: the installer's, and the other members'.
  roles?: { installer: AppRole[]; members?: AppRole[] }
  widgets: string[]
}

// What GET …/apps/{app}/offline answers (docs/api.md): the live release as a client needs it to
// use the app without the server, and the records the caller may read.
export type OfflineApp = {
  app_id: number
  space_id: number
  name: string
  release: number
  definition: {
    definition_version: number
    requires: string[]
    collection: { fields: AppField[] }
    entry?: string
    screens?: { name: string; record?: boolean }[]
    actions: Record<string, { kind: AppActionKind; screen?: string; then?: string }>
    summaries?: Record<string, { kind: 'count' | 'sum'; field?: string; where?: unknown }>
  }
  messages: unknown[]
  scope: 'all' | 'own'
  records: number
  total: number
  truncated: boolean
  can_insert: boolean
  captured_at: string
  // The caller's operation keys that made downloaded records: key → record id.
  operations?: Record<string, number>
  // Which of the downloaded records the caller may change or delete now.
  modifiable?: number[]
}

// A widget an app offers for Home (docs/api.md, Home): a bounded view, never markup of its own.
export type Widget = {
  id: string
  title: string
  kind: 'summaries' | 'screen' | 'shortcut'
  size?: 'small' | 'medium' | 'large'
  summaries?: { summary: string; label: string }[]
  screen?: string
}

// What an app's navigation pin shows: its label, and the screen it opens the app on.
export type Pin = { label: string; screen?: string }

// A widget on someone's Home or a pin in their navigation, as the server works it out now. `id` is the
// person's own placement, `default_id` the space default it is. An `unavailable` item has no widget
// or label, only the app's name and why (`offline`, `unpublished`, `removed`).
export type HomeItem = {
  id?: number
  default_id?: number
  source: 'personal' | 'default'
  kind: 'widget' | 'pin'
  space_id: number
  app_id: number
  app_name: string
  widget_id?: string
  widget?: Widget
  label?: string
  screen?: string
  state: 'ready' | 'unavailable'
  reason?: 'offline' | 'unpublished' | 'removed'
  release?: { number: number; definition_version: number; requires: string[] }
  can_manage?: boolean
}

export type HomeView = {
  widgets: HomeItem[]
  pins: HomeItem[]
  hidden: HomeItem[]
  limits: { widgets: number; pins: number }
}

// The first-run checklist an admin sees on Home (internal/checklist): each step is done by what exists
// on the server, or skipped by an admin because it doesn't apply.
export type ChecklistStep = 'invite' | 'provider' | 'space' | 'app' | 'email' | 'backups'
export type Checklist = {
  dismissed: boolean
  // Every step that isn't optional is done or skipped.
  complete: boolean
  items: { id: ChecklistStep; state: 'done' | 'todo' | 'skipped'; optional: boolean }[]
}

// An app the person may place on Home or pin, with what it offers.
export type AvailableApp = {
  space_id: number
  space_name: string
  space_kind: 'private' | 'shared'
  app_id: number
  name: string
  widgets: Widget[]
  pin: Pin | null
  can_set_defaults: boolean
  defaults: { id: number; kind: 'widget' | 'pin'; widget?: string }[]
}

// Who made a change to an app's record, through the agent or an access token if so.
export type RecordAuthor = { id: number; name: string; agent: boolean; via?: string }

// One record of an app, with its revision and who made, changed and deleted it. `data` holds
// the fields the app's collection declares.
export type AppRecord = {
  id: number
  revision: number
  data: Record<string, unknown>
  created_at: string
  created_by: RecordAuthor | null
  updated_at: string
  updated_by: RecordAuthor | null
  deleted_at: string | null
  deleted_by: RecordAuthor | null
}

// An effect an action of an app set going, as the server's outbox lists it: what it makes, how it is
// getting on, why it failed, and what it made if the caller may read where. `simulated` is of a
// preview only.
export type EffectKind = 'create_card' | 'update_card' | 'create_document' | 'open_route'
export type EffectStatus = 'pending' | 'running' | 'done' | 'failed' | 'simulated'
export type AppEffect = {
  // 0 for a simulated one.
  id: number
  operation: string
  position: number
  kind: EffectKind
  name?: string
  action: string
  record: number
  record_revision: number
  status: EffectStatus
  attempts: number
  next_attempt_at: string | null
  error: { code: string; params?: Params } | null
  detail: { title?: string; column?: string; path?: string; route?: string }
  result?: Record<string, unknown>
  created_at?: string
  updated_at?: string
  done_at: string | null
  // Whether the caller may retry it: it failed and they are who acted.
  can_retry: boolean
}
// more: older effects than the page's last match too (ask again with before=<its id>).
export type AppEffectList = { effects: AppEffect[]; more?: boolean; active: number; failed: number }

// One value over every record a query matches: its count is how many records it was computed from.
export type RecordAggregate = {
  kind: 'count' | 'sum' | 'min' | 'max' | 'avg'
  field?: string
  value: number | string | null
  count: number
}

// A page of an app's records, with the totals of all that match the query (…/records/query).
export type RecordPage = {
  records: AppRecord[]
  next_cursor: string | null
  total: number
  aggregates: RecordAggregate[]
}

// One revision of a record: what was done, by whom, and the data it left.
export type RecordVersion = {
  revision: number
  operation: 'insert' | 'update' | 'delete' | 'restore'
  data: Record<string, unknown>
  at: string
  by: RecordAuthor | null
  action?: string
  // The one version of a record that existed before revisions were kept.
  synthetic?: boolean
}

// Mentions are the people the body mentions that the reader may see in the space, by ID, with the
// name the server has for them: what a mention shows, never the label typed into the text.
export type Comment = {
  id: number
  card_id: number
  author: Author
  body: string
  created_at: string
  mentions: { id: number; name: string }[]
}
// A card as the card route sends it: with its comments, oldest first.
export type CardWithThread = Card & { comments: Comment[] }

export const ws = (workspace: number) => `/api/workspaces/${workspace}`

// two_factor is the admins' view only: whether the member has two-factor login on.
export type Member = {
  id: number
  email: string
  name: string
  role: 'admin' | 'member'
  joined_at: string
  two_factor?: boolean
}
// One event of a workspace's audit log, with the names read when the log was (docs/audit.md).
export type AuditEntry = {
  id: number
  at: string
  action: string
  actor_id: number | null
  // The actor and the user an event is about, while members of the workspace; null for an
  // anonymous event, a deleted user and one who has left.
  actor: { name: string; email: string } | null
  target_type: string
  target_id: number | null
  target_user: { name: string; email: string } | null
  target_name: string | null
  details: Record<string, string | number | boolean | string[]>
  client_addr: string | null
}
export type AuditPage = { events: AuditEntry[]; next: string }

export type Invite = { id: number; email: string; role: 'admin' | 'member'; created_at: string; expires_at: string }
// The reply that makes an invite or an access token: the record, with its secret, shown only now.
export type InviteCreated = Invite & { token: string }
export type TokenCreated = Token & { token: string }
export type SpaceMember = { id: number; email: string; name: string; role: 'editor' | 'viewer'; added_at: string }

// An access token, without the token itself: the server returns that only when it makes one.
export type Token = {
  id: number
  workspace_id: number
  name: string
  created_at: string
  expires_at: string | null
  last_used_at: string | null
}

export type Model = { id: string; name?: string; context_window?: number; max_tokens?: number; reasoning?: boolean }

// A model provider account. api_key is only ever "set" or "not set": the API never returns it.
export type ProviderAccount = {
  id: number
  scope: 'workspace' | 'user'
  provider: string
  name: string
  display_name: string
  kind: string
  base_url: string
  api_key: 'set' | 'not set'
  models: Model[]
  created_at: string
  updated_at: string
}

// `code` and `params` are the failure's, for codeText().
export type ProviderCheck = { ok: boolean; error?: string; code?: string; params?: Params; missing?: string[] }

export type SearchSettings = {
  embeddings: { account: number; provider: string; model: string } | null
  documents: number
  pending: number
}

export type ConsentEndpoint = { account: number; provider: string; model: string; base_url: string }
export type Consent = {
  enabled: boolean
  endpoint: ConsentEndpoint | null
  given_by: number | null
  given_at: string | null
}

// CLI workers (docs/cli-workers.md): the signed-in user's own logins to Claude Code, Codex and
// Grok Build. The CLI keeps its credentials on the server's worker; the API never returns them.
export type CliName = 'claude' | 'codex' | 'grok'

export type CliLoginState = 'starting' | 'waiting' | 'connected' | 'failed' | 'expired' | 'cancelled'

// A login in progress through the CLI's own flow: the URL to sign in at (a link only when
// url_trusted), the one-time code to enter there, or, with needs_code, a code to paste back.
export type CliLogin = {
  cli: CliName
  // The account's slot on the worker: absent for the first, '2' … '9' for the others.
  slot?: string
  state: CliLoginState
  url?: string
  url_trusted: boolean
  code?: string
  needs_code: boolean
  message?: string
  started_at: string
}

export type CliLimit = {
  window: string
  used_percent?: number
  window_minutes?: number
  resets_at?: string
  status?: string
  detail?: string
}

export type CliAccount = {
  // The account's handle: the CLI for the first, `claude-2` … `claude-9` for the others.
  id: string
  cli: CliName
  slot: number
  // The user's name for it; empty for none.
  label: string
  name: string
  available: boolean
  state: 'connected' | 'reconnect' | 'disconnected'
  auth_method: string
  limits: CliLimit[]
  limits_at: string | null
  login: CliLogin | null
}

export type CliAccounts = {
  enabled: boolean
  mode: string
  reason?: string
  reason_code?: string
  accounts: CliAccount[]
}

export const CLI_ACCOUNTS = '/api/me/cli-accounts'

// A workspace's shared CLI account (docs/account-switching.md): an API key its members' turns
// use through the server's proxy. The key never comes back: api_key is "set".
export type SharedCliAccount = {
  id: number
  cli: CliName
  name: string
  label: string
  base_url: string
  model: string
  api_key: 'set'
  limits: CliLimit[]
  limits_at: string | null
  created_at: string
}

// Which accounts a member's turns in a workspace use.
export type CliAccountMode = 'own_else_shared' | 'own' | 'shared'

export type SharedCliAccounts = {
  enabled: boolean
  reason?: string
  reason_code?: string
  mode: CliAccountMode
  default_mode: CliAccountMode
  accounts: SharedCliAccount[]
}

export type CliMemberModes = {
  default_mode: CliAccountMode
  members: { user_id: number; name: string; email: string; mode: CliAccountMode | null }[]
}

// An account in, or for, the user's switching queue: one of their own (id is its handle) or a
// workspace's shared one (id is its number).
export type QueueAccount = {
  kind: 'own' | 'shared'
  id: string
  cli: CliName
  // In English; label, number (an own account's, for its CLI) and workspace_name name it in
  // the user's language.
  title: string
  label?: string
  number?: number
  workspace?: number
  workspace_name?: string
  limited: boolean
  limited_until?: string
}

export type CliSwitching = { enabled: boolean; queue: QueueAccount[]; available: QueueAccount[] }

export const CLI_SWITCHING = '/api/me/cli-switching'

// How many accounts a user may connect per CLI.
export const MAX_CLI_ACCOUNTS = 9

// The account a conversation's turn on cli runs on, as the server picks it: the first that is
// connected, or else the first (which needs a new sign-in).
export function cliAccountFor(accounts: CliAccount[], cli: CliName): CliAccount | undefined {
  const mine = accounts.filter((a) => a.cli === cli)
  return mine.find((a) => a.state === 'connected') ?? mine[0]
}

// An account's title: the product, with the user's label or its number after the first.
export function cliAccountTitle(a: CliAccount): string {
  if (a.label) return `${a.name} · ${a.label}`
  return a.slot > 1 ? t('settings.cliAccounts.accountNumber', { name: a.name, number: a.slot }) : a.name
}

// The CLIs' product names, for what the server doesn't name (a conversation's tool calls).
export const CLI_PRODUCTS: Record<CliName, string> = { claude: 'Claude Code', codex: 'Codex', grok: 'Grok Build' }

export const isCliName = (v: unknown): v is CliName => v === 'claude' || v === 'codex' || v === 'grok'

// The CLI a model provider runs on: `cli-claude` → claude; null for any other provider.
export function cliOfProvider(provider: string | undefined): CliName | null {
  const name = provider?.startsWith('cli-') ? provider.slice(4) : null
  return isCliName(name) ? name : null
}

// Dictation (docs/dictation.md). The API key is only ever "set" or "not set".
export type Credit = { name: string; by: string; url: string; licence: string; licence_url: string; note?: string }

export type DictationAddon = {
  id: string
  name: string
  vendor: string
  kind: string
  licence_name: string
  licence_url: string
  notice: string
  attribution: Credit
  installed: boolean
}

export type LocalModelState = 'unavailable' | 'not_downloaded' | 'downloading' | 'ready' | 'failed'

export type DictationStatus = {
  // What a dictation uses now ('api', 'local', an add-on's id), or '' for nothing.
  engine: string
  reason?: string
  reason_code?: string
  reason_params?: Params
  settings: {
    engine: string
    language: string
    api: { base_url: string; model: string; api_key: 'set' | 'not set' }
    accepted_notice: string
    updated_at?: string
  }
  local: {
    model: string
    state: LocalModelState
    progress?: number
    error?: string
    error_code?: string
    error_params?: Params
  } | null
  addons: DictationAddon[]
  credits: Credit[]
  max_seconds: number
}

export const dictationPath = (workspace: number) => `${ws(workspace)}/dictation`

// The server's scheduled backups (docs/install.md#backups), for admins. Times are UTC, and
// absent when there is none yet. `health` is 'none' when no schedule has run on the server.
export type BackupHealth = 'none' | 'pending' | 'ok' | 'failing' | 'late'
export type BackupStatus = {
  health: BackupHealth
  at?: string
  retries?: number
  retry_every_seconds?: number
  last_success?: string
  last_snapshot?: string
  last_attempt?: string
  last_error?: string
  last_error_at?: string
  next_attempt?: string
  due_by?: string
}

export const backupPath = (workspace: number) => `${ws(workspace)}/backup`

// Storage quotas and what each space stores (docs/quotas.md), in bytes. A limit of 0 is none.
export type StorageLimits = { workspace: number; user: number }
export type SpaceUsage = {
  id: number
  kind: 'private' | 'shared'
  name: string
  // For a private space: whose it is.
  owner?: { id: number; name: string; email: string }
  knowledge_versioned: number
  knowledge_unversioned: number
  attachments: number
  total: number
  // The quota of the space itself: the private space's, 0 for a shared one.
  limit: number
}
// What admins read: every space of the workspace.
export type StorageReport = { quota: StorageLimits; total: number; spaces: SpaceUsage[] }
// What a member reads: their own private space (null if they have none here).
export type MyStorage = { quota: StorageLimits; space: SpaceUsage | null }
export const usagePath = (workspace: number) => `${ws(workspace)}/usage`

// Starts an export of what the caller owns in the workspace (docs/api.md#export-my-data): a POST that
// answers `{download, expires_in}`, the address to go to for the zip, which works once.
export const exportPath = (workspace: number) => `${ws(workspace)}/export`

// The server's outgoing email (docs/install.md#email), for admins: where it sends through, never the
// account or its password. `password` says whether the server signs in.
export type MailStatus =
  | { configured: false }
  | {
      configured: true
      host: string
      port: number
      security: 'starttls' | 'tls' | 'none'
      from: string
      password: 'set' | 'not set'
    }

export const mailPath = (workspace: number) => `${ws(workspace)}/mail`

// The agent browser (docs/agent-browser.md): the signed-in user's own.
export type BrowserKind = 'personal' | 'testing'
export type BrowserTab = { id: string; title: string; url: string; agent: boolean }
export type BrowserState = {
  available: boolean
  running: boolean
  live: boolean
  stops_at?: string
  tabs?: BrowserTab[]
}
// `reason` says why it can't be used, in English, and `reason_code` as an error code (errors.<code>).
export type BrowserStatus = {
  enabled: boolean
  reason?: string
  reason_code?: string
  browsers: Partial<Record<BrowserKind, BrowserState>>
}
export const BROWSER = '/api/me/browser'
