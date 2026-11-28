import type { Rule } from './apps/rules'
import { has, type Key, type Params, t } from './i18n'
import { endSession, sameSession, sessionUser } from './session'

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly body?: unknown,
  ) {
    super(message)
  }

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

export const sessionLostFor = (user: number) => new CustomEvent(SESSION_LOST, { detail: { user } })

export const concernsAccount = (e: Event, signedIn: number | null | undefined): boolean => {
  const user = (e as CustomEvent<{ user?: number } | null>).detail?.user
  return user === undefined || signedIn === null || signedIn === undefined || user === signedIn
}

export const EXPECTED_USER_HEADER = 'X-Taskto-Expected-User'

export const forAccount = (user: number): Record<string, string> => ({ [EXPECTED_USER_HEADER]: String(user) })

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
  const method = init.method ?? 'GET'
  const same = sameSession()
  const account = sessionUser()
  const named = Object.keys(init.headers ?? {}).find((h) => h.toLowerCase() === EXPECTED_USER_HEADER.toLowerCase())
  const extra =
    account !== null && method !== 'GET' && method !== 'HEAD' && named === undefined
      ? { ...init.headers, ...forAccount(account) }
      : (init.headers ?? {})
  const res = await fetch(path, {
    method,
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
  const data = await res.json().catch((e) => {
    if (init.signal?.aborted || (e as { name?: string } | null)?.name === 'AbortError') throw e
    return null
  })
  if (!res.ok) {
    if (res.status === 401 && same()) {
      endSession()
      const expected = Number(extra[EXPECTED_USER_HEADER as keyof typeof extra])
      window.dispatchEvent(
        Number.isInteger(expected) && expected > 0 ? sessionLostFor(expected) : new Event(SESSION_LOST),
      )
    }
    if (res.status === 409 && data?.code === ACCOUNT_CHANGED && same()) {
      const key = Object.keys(extra).find((h) => h.toLowerCase() === EXPECTED_USER_HEADER.toLowerCase())
      const expected = Number(key === undefined ? undefined : extra[key as keyof typeof extra])
      window.dispatchEvent(new CustomEvent(ACCOUNT_CHANGED_EVENT, { detail: { expected } }))
    }
    const message = data && typeof data.error === 'string' ? data.error : `${res.status} ${res.statusText}`
    throw new ApiError(res.status, message, data)
  }
  return data as T
}

export function errorText(e: unknown, fieldLabels?: ReadonlyMap<string, string>): string {
  if (e instanceof ApiError) {
    const key = e.code && `errors.${e.code}`
    if (key && has(key)) {
      const params = { ...e.params }
      for (const name of ['field', 'secret'] as const) {
        const code = params[name]
        const label = typeof code === 'string' ? fieldLabels?.get(code) : undefined
        if (label) params[name] = label
      }
      return t(key as Key, params)
    }
    const said = (e.body as { error?: unknown } | undefined)?.error
    if (typeof said !== 'string') return t('errors.http', { status: String(e.status) })
  }
  return e instanceof Error ? e.message : String(e)
}

export function codeText(code: string | undefined, params: Params | undefined, said: string): string {
  const key = code && `errors.${code}`
  return key && has(key) ? t(key as Key, params) : said
}

export type Workspace = { id: number; name: string; role: 'admin' | 'member'; locale?: string }
export type Me = {
  user: { id: number; email: string; name: string; locale?: string; time_zone?: string }
  workspaces: Workspace[]
  can_create_workspace: boolean
  is_operator: boolean
  session?: string
}

export type Conversation = {
  id: number
  workspace_id: number
  title: string
  archived_at: string | null
  created_at: string
  updated_at: string
  cli_permissions?: 'ask' | 'allow_all'
  origin: { kind: 'app' | 'routine'; space_id?: number; app_id?: number; routine_id?: number } | null
}

export type CliQuestion = { id: string; turn: string; cli: string; tool: string; input?: string; asked_at: string }

export type Space = {
  id: number
  workspace_id: number
  kind: 'private' | 'shared'
  name: string
  archived_at: string | null
  role?: 'owner' | 'editor' | 'viewer'
  access: 'write' | 'read' | 'none'
  offline_downloads?: boolean
}

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

export type Author = { id: number; name: string; agent: boolean; via?: string } | null

export type Card = {
  id: number
  board_id: number
  title: string
  body: string
  column: 'backlog' | 'todo' | 'doing' | 'review' | 'done' | 'archived'
  archived_from: Card['column'] | null
  assignee: { kind: 'agent' } | { kind: 'user'; id: number; name?: string } | { kind: 'deleted' } | null
  blocked: boolean
  blocked_reason: string | null
  blocked_by: number[]
  docs: string[]
  conversations: string[]
  due: string | null
  labels: string[]
  version: number
  updated_at: string
  updated_by: Author
}

export type AppRole = 'builder' | 'publisher' | 'user' | 'data_manager' | 'reader'
export type RecordScope = 'none' | 'own' | 'all'

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

export type AppGrant = { user_id: number | null; roles: AppRole[] }

export type DefinitionNeeds = { definition_version: number; requires: string[] }

export type AppRelease = DefinitionNeeds & {
  number: number
  published_at: string
  published_by: Author
  live: boolean
  live_since?: string
  definition?: AppDefinition
}

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
  option_labels?: Record<string, string>
  capture?: 'button' | 'submit'
  format?: string
  editable?: boolean
}

export type AppDefinition = { collection: { fields: AppField[] }; actions: Record<string, { kind: string }> }

export type SchemaChange = { change: string; field: string }

export type AppActionKind = 'insert' | 'update' | 'delete' | 'navigate'
export type AppDraft = DefinitionNeeds & {
  collection?: { fields: AppField[] }
  actions?: Record<string, { kind: AppActionKind; effects?: DraftEffect[] }>
  integrations?: Record<string, { kind: string }>
}

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
  integration?: string
  amount_field?: string
  currency?: string
  currency_field?: string
  description?: string
  url_field?: string
  id_field?: string
}

export type EffectTarget = {
  space_id: number
  space?: string
  space_kind?: 'private' | 'shared'
  board_id?: number
  board?: string
}

export type PreviewRow = { id: number; revision: number; data: Record<string, unknown> }
export type PreviewReply = { messages: unknown[]; rows: PreviewRow[]; effects?: AppEffect[] }

export type App = {
  id: number
  space_id: number
  name: string
  archived_at: string | null
  version: number
  rows: number
  access: AppAccess
  live: AppRelease | null
  unpublished_changes: boolean
  draft?: AppDraft
  effect_targets?: EffectTarget[]
  needs_board?: boolean
}

export type AppTemplate = {
  id: string
  version: number
  name: string
  description: string
  recommended_space: 'private' | 'shared'
  needs_board: boolean
  captures: ('attachment' | 'location' | 'signature' | 'barcode')[]
  roles?: { installer: AppRole[]; members?: AppRole[] }
  widgets: string[]
}

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
    series?: Record<
      string,
      {
        kind: 'count' | 'sum' | 'avg'
        field?: string
        group_by: string
        bucket?: 'day' | 'week' | 'month'
        last?: number
        split_by?: string
        where?: unknown
      }
    >
  }
  messages: unknown[]
  scope: 'all' | 'own'
  records: number
  total: number
  truncated: boolean
  can_insert: boolean
  captured_at: string
  operations?: Record<string, number>
  modifiable?: number[]
}

export type Widget = {
  id: string
  title: string
  kind: 'summaries' | 'screen' | 'shortcut'
  size?: 'small' | 'medium' | 'large'
  summaries?: { summary: string; label: string }[]
  screen?: string
}

export type Pin = { label: string; screen?: string }

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

export type ChecklistStep = 'invite' | 'provider' | 'space' | 'app' | 'email' | 'backups'
export type Checklist = {
  dismissed: boolean
  complete: boolean
  items: { id: ChecklistStep; state: 'done' | 'todo' | 'skipped'; optional: boolean }[]
}

export type DeskTab = { id: string; path: string }
export type DeskLayoutNode = {
  split?: 'row' | 'column'
  sizes?: number[]
  children?: DeskLayoutNode[]
  group?: string
  active?: string
  tabs?: DeskTab[]
}
export type DeskLayout = { v: 1; focus: string; root: DeskLayoutNode | null }
export type Desk = { id: number; name: string; layout: unknown; version: number; updated_at: string }
export type DeskSummary = { id: number; name: string; updated_at: string }
export type DeskList = { desks: DeskSummary[] }
export type DeskLayoutWrite = { version: number; layout: unknown }
export type DeskChanged = { error: string; code: 'desk_changed'; current: Desk }

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

export type RecordSystemAuthor = { kind: string; id: number; provider?: string; name?: string; removed?: boolean }

export type RecordAuthor = { id: number; name: string; agent: boolean; via?: string; system?: RecordSystemAuthor }

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

export type EffectKind = 'create_card' | 'update_card' | 'create_document' | 'open_route' | 'create_payment_link'
export type EffectStatus = 'pending' | 'running' | 'done' | 'failed' | 'simulated'
export type AppEffect = {
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
  detail: { title?: string; column?: string; path?: string; route?: string; amount?: string; currency?: string }
  result?: Record<string, unknown>
  created_at?: string
  updated_at?: string
  done_at: string | null
  can_retry: boolean
}
export type AppEffectList = { effects: AppEffect[]; more?: boolean; active: number; failed: number }

export type RecordAggregate = {
  kind: 'count' | 'sum' | 'min' | 'max' | 'avg'
  field?: string
  value: number | string | null
  count: number
}

export type RecordPage = {
  records: AppRecord[]
  next_cursor: string | null
  total: number
  aggregates: RecordAggregate[]
}

export type RecordVersion = {
  revision: number
  operation: 'insert' | 'update' | 'delete' | 'restore'
  data: Record<string, unknown>
  at: string
  by: RecordAuthor | null
  action?: string
  synthetic?: boolean
}

export type Comment = {
  id: number
  card_id: number
  author: Author
  body: string
  created_at: string
  mentions: { id: number; name: string }[]
}
export type CardWithThread = Card & { comments: Comment[] }

export const ws = (workspace: number) => `/api/workspaces/${workspace}`

export type Member = {
  id: number
  email: string
  name: string
  role: 'admin' | 'member'
  joined_at: string
  disabled?: boolean
  deletion_at?: string | null
  two_factor?: boolean
}
export type AuditEntry = {
  id: number
  at: string
  action: string
  actor_id: number | null
  actor_deleted: boolean
  target_deleted: boolean
  actor: { name: string; email: string } | null
  target_type: string
  target_id: number | null
  target_user: { name: string; email: string } | null
  target_name: string | null
  details: Record<string, string | number | boolean | string[]>
  client_addr: string | null
}
export type AuditPage = { events: AuditEntry[]; next: string }

export type DeletionBlocker = { workspace_id: number; workspace: string; operator: boolean }
export type DeletionStatus = {
  grace_days: number
  method: 'password' | 'fresh_login'
  two_factor: boolean
  fresh: boolean
  blockers: DeletionBlocker[]
}
export type DeletionReply = { purge_at: string }
export type PendingDeletion = {
  user: { id: number; email: string; name: string }
  requested_at: string
  purge_at: string
  purging: boolean
}

export type Invite = { id: number; email: string; role: 'admin' | 'member'; created_at: string; expires_at: string }
export type InviteCreated = Invite & { token: string; email_sent?: boolean }
export type TokenCreated = Token & { token: string }
export type SpaceMember = { id: number; email: string; name: string; role: 'editor' | 'viewer'; added_at: string }

export type Token = {
  id: number
  workspace_id: number
  name: string
  created_at: string
  expires_at: string | null
  last_used_at: string | null
}

export type Session = {
  id: string
  browser: string
  os: string
  created_at: string
  last_seen_at: string
  expires_at: string
  current: boolean
}

export type Model = { id: string; name?: string; context_window?: number; max_tokens?: number; reasoning?: boolean }

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

export type ProviderCheck = { ok: boolean; error?: string; code?: string; params?: Params; missing?: string[] }

export type Integration = {
  id: number
  provider: string
  kind: string
  name: string
  display_name: string
  mode: 'test' | 'live'
  settings: Record<string, string>
  max_amount_minor: number
  currencies: string[]
  secrets: Record<string, string>
  webhook_url: string | null
  webhook_url_changed: boolean
  disabled: boolean
  disabled_at: string | null
  spaces: number[]
  approval_spaces: number[]
  other_spaces: number
  created_at: string
  updated_at: string
  last_event_at: string | null
}

export type IntegrationSecretSpec = { name: string; required: boolean }
export type IntegrationSettingSpec = { name: string; options: string[]; required: boolean }
export type IntegrationProvider = {
  id: string
  kinds: string[]
  secrets: IntegrationSecretSpec[]
  settings: IntegrationSettingSpec[]
}

export type IntegrationRegistry = {
  providers: IntegrationProvider[]
  currencies: string[]
  exponents: Record<string, number>
  max_amount_minor: number
  webhooks_available: boolean
}

export type IntegrationCheck = { ok: boolean; remote: boolean; code?: string; params?: Params }

export type ContractLink = {
  action: string
  action_kind: string
  amount_field: string
  describes: string[]
  describes_id: boolean
  currency?: string
  currency_field?: string
  currency_options?: string[]
  url_field: string
  id_field: string
  when?: Rule
}
export type ContractSet = { field: string; value?: unknown; from?: string }
export type ContractHandler = { event: string; sets: ContractSet[] }
export type ContractRule = { event: string; rule: Rule }
export type ContractWrite = { field: string; values: unknown[]; from: string[] }
export type ContractField = {
  name: string
  type: string
  required?: boolean
  required_when?: Rule
  max_length?: number
  min?: number
  max?: number
  options?: string[]
  format?: string
  editable?: boolean
  max_files?: number
  max_bytes?: number
}
export type IntegrationContract = {
  version: number
  kind: string
  operations: string[]
  sends: string[]
  currencies: string[]
  events: string[]
  writes: ContractWrite[]
  only_if: ContractRule[]
  links: ContractLink[]
  handlers: ContractHandler[]
  fields: ContractField[]
}

export type ApprovalStatus = 'none' | 'requested' | 'approved' | 'rejected' | 'revoked'

export type BoundIntegration = { id: number; name: string; display_name: string; usable: boolean }

export type SlotStatus = {
  slot: string
  kind: string
  contract: IntegrationContract
  contract_hash: string
  status: ApprovalStatus
  approval_id: number | null
  integration: BoundIntegration | null
  live: { contract_hash: string; status: ApprovalStatus; usable: boolean } | null
}
export type IntegrationStatus = { slots: SlotStatus[] }

export type IntegrationApproval = {
  id: number
  app_id: number
  app_name: string
  space_id: number
  space_name: string
  space_kind: string
  slot: string
  kind: string
  contract: IntegrationContract
  contract_hash: string
  status: ApprovalStatus
  live: boolean
  integration: BoundIntegration | null
  space_grant: SpaceGrant
  requested_by: RecordAuthor | null
  requested_at: string
  decided_by: RecordAuthor | null
  decided_at: string | null
}
export type SpaceGrant = 'none' | 'approval' | 'settings'
export type IntegrationApprovalList = { approvals: IntegrationApproval[]; next_cursor: string | null }

export type IntegrationEventState = 'pending' | 'running' | 'applied' | 'ignored' | 'failed'
export type IntegrationEvent = {
  id: number
  kind: string
  source: string
  state: IntegrationEventState
  code?: string
  amount_minor: number
  currency: string
  livemode: boolean
  attempts: number
  occurred_at: string
  received_at: string
  processed_at: string | null
  retriable: boolean
}
export type IntegrationEventList = { events: IntegrationEvent[]; next_before: number }

export type PaymentLinkOutcome = {
  effect_id: number
  app_id: number
  space_id: number
  record_id: number
  amount: string
  currency: string
  since: string
  waiting: boolean
  can_confirm: boolean
  code?: string
  params?: Params
  object_id: string
  attempt_seq: number
  attempt_sends: number
  attempts: PaymentLinkAttempt[]
}
export type PaymentLinkAttempt = {
  reference: string
  kind: 'checkout_session' | 'payment_link'
  object_id: string
  object_verified: boolean
  account_id: string
  mode: string
  first_sent_at: string
}
export type PaymentLinkOutcomes = { payment_links: PaymentLinkOutcome[] }

export type SpaceIntegration = { id: number; name: string; display_name: string; kind: string; status: string }

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

export type CliName = 'claude' | 'codex' | 'grok'

export type CliLoginState = 'starting' | 'waiting' | 'connected' | 'failed' | 'expired' | 'cancelled'

export type CliLogin = {
  cli: CliName
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
  id: string
  cli: CliName
  slot: number
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

export type QueueAccount = {
  kind: 'own' | 'shared'
  id: string
  cli: CliName
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

export const MAX_CLI_ACCOUNTS = 9

export function cliAccountFor(accounts: CliAccount[], cli: CliName): CliAccount | undefined {
  const mine = accounts.filter((a) => a.cli === cli)
  return mine.find((a) => a.state === 'connected') ?? mine[0]
}

export function cliAccountTitle(a: CliAccount): string {
  if (a.label) return `${a.name} · ${a.label}`
  return a.slot > 1 ? t('settings.cliAccounts.accountNumber', { name: a.name, number: a.slot }) : a.name
}

export const CLI_PRODUCTS: Record<CliName, string> = { claude: 'Claude Code', codex: 'Codex', grok: 'Grok Build' }

export const isCliName = (v: unknown): v is CliName => v === 'claude' || v === 'codex' || v === 'grok'

export function cliOfProvider(provider: string | undefined): CliName | null {
  const name = provider?.startsWith('cli-') ? provider.slice(4) : null
  return isCliName(name) ? name : null
}

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

export type UpdateRun = {
  at?: string
  finished?: string
  result: string
  from?: string
  to?: string
  code?: string
  backed_up?: boolean
  requested?: boolean
}
export type UpdateRunning = { since: string; to?: string; requested?: boolean }
export type UpdateRequestRun = { at?: string; finished?: string; result: string; code?: string }
export type UpdateRequestState = { available: boolean; reason?: string; pending_since?: string }
export type UpdateStatus = {
  configured: boolean
  running_version: string
  expects_status?: boolean
  stale?: boolean
  status: {
    version: number
    checked?: string
    channel: string
    hold?: boolean
    recorded_version?: string
    last_run: UpdateRun | null
    last_change: UpdateRun | null
    needs_person: { kind: string; code?: string } | null
    running: UpdateRunning | null
    requests: { supported: boolean; last: UpdateRequestRun | null } | null
    looked?: string
    available: UpdateAvailable | null
  } | null
  request: UpdateRequestState
  notes_url?: string
}
export type UpdateAvailable = {
  version: string
  sha?: string
  published?: string
  manual_upgrade: boolean
  repo?: string
}
export type UpdateRequested = { requested_at: string }

export const updatesPath = (workspace: number) => `${ws(workspace)}/updates`

export type CLIWorkerStatus = {
  mode: string
  reachable: boolean
  authority_lost: boolean
  other_installation: boolean
  boot?: string
  installation?: string
}
export const cliWorkerPath = (workspace: number) => `${ws(workspace)}/cli-worker`
export const updateRequestPath = (workspace: number) => `${ws(workspace)}/updates/request`

export type StorageLimits = { workspace: number; user: number }
export type SpaceUsage = {
  id: number
  kind: 'private' | 'shared'
  name: string
  owner?: { id: number; name: string; email: string }
  knowledge_versioned: number
  knowledge_unversioned: number
  attachments: number
  total: number
  limit: number
}
export type StorageReport = { quota: StorageLimits; total: number; spaces: SpaceUsage[] }
export type MyStorage = { quota: StorageLimits; space: SpaceUsage | null }
export const usagePath = (workspace: number) => `${ws(workspace)}/usage`

export const exportPath = (workspace: number) => `${ws(workspace)}/export`

export const workspaceExportPath = (workspace: number) => `${ws(workspace)}/export/workspace`

export type MailStatus =
  | { configured: false; sends_links?: boolean }
  | {
      configured: true
      sends_links?: boolean
      host: string
      port: number
      security: 'starttls' | 'tls' | 'none'
      from: string
      password: 'set' | 'not set'
    }

export const mailPath = (workspace: number) => `${ws(workspace)}/mail`

export type BrowserKind = 'personal' | 'testing'
export type BrowserTab = { id: string; title: string; url: string; agent: boolean }
export type BrowserState = {
  available: boolean
  running: boolean
  live: boolean
  stops_at?: string
  tabs?: BrowserTab[]
}
export type BrowserStatus = {
  enabled: boolean
  reason?: string
  reason_code?: string
  browsers: Partial<Record<BrowserKind, BrowserState>>
}
export const BROWSER = '/api/me/browser'
