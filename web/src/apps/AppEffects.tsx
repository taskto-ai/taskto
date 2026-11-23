// What an app's actions set going: cards, documents and places to open. They are done by the
// server in the background, as the person who acted, so this only shows how they are getting on
// and, for one that failed, lets the person who acted retry it. The server decides what each
// person sees and may retry: this only asks.
//
// The same list (EffectItems) shows what a preview of a draft's action would set going.

import { useEffect, useState } from 'react'
import { type AppEffect, type AppEffectList, api, codeText, errorText } from '../api'
import { type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { boardItem, documentItem, Link, routePath } from '../router'

// How often the list is asked for again while some effect waits or runs.
const POLL_MS = 3000
// How many effects a page holds.
const PAGE = 50

export function AppEffects({
  workspace,
  base,
  writable,
  reloadKey = 0,
}: {
  workspace: number
  // …/spaces/{space}/apps/{app}
  base: string
  // Whether the space lets the caller write; a retry is a write.
  writable: boolean
  // Changed when an action ran, which may have queued effects.
  reloadKey?: number
}) {
  const [open, setOpen] = useState(false)
  // Only the ones that failed: they stay until retried, however many newer there are.
  const [failedOnly, setFailedOnly] = useState(false)
  // Older pages the person asked for, after the newest page that is kept fresh.
  const [older, setOlder] = useState<{ effects: AppEffect[]; more: boolean } | null>(null)
  const [olderError, setOlderError] = useState<string | null>(null)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const t = useT()
  const query = failedOnly ? 'status=failed&' : ''
  const list = useLoad(() => api<AppEffectList>(`${base}/effects?${query}limit=${PAGE}`), [base, query, reloadKey])
  // Another filter is another list.
  // biome-ignore lint/correctness/useExhaustiveDependencies: a changed filter or app drops the older pages
  useEffect(() => setOlder(null), [base, query])
  const waiting = (list.data?.active ?? 0) > 0
  const { reload } = list
  useEffect(() => {
    if (!waiting) return
    const timer = setInterval(reload, POLL_MS)
    return () => clearInterval(timer)
  }, [waiting, reload])

  const data = list.data
  // An app that never set anything going shows nothing of this.
  if (!data || (data.effects.length === 0 && data.active === 0 && data.failed === 0 && !failedOnly)) return null
  const shown = [...data.effects, ...(older?.effects ?? [])]
  const more = older ? older.more : (data.more ?? false)
  const showOlder = async () => {
    const last = shown[shown.length - 1]
    if (!last) return
    setLoadingOlder(true)
    setOlderError(null)
    try {
      const page = await api<AppEffectList>(`${base}/effects?${query}limit=${PAGE}&before=${last.id}`)
      setOlder({ effects: [...(older?.effects ?? []), ...page.effects], more: page.more ?? false })
    } catch (err) {
      setOlderError(errorText(err))
    }
    setLoadingOlder(false)
  }
  return (
    <div className="tt-effects">
      <button
        type="button"
        className="tt-button"
        aria-expanded={open}
        aria-controls="tt-effects-panel"
        onClick={() => setOpen((o) => !o)}
      >
        {open ? t('apps.effects.close') : t('apps.effects.open')}
        {(data.active > 0 || data.failed > 0) && (
          <span className="tt-effects-badge" data-failed={data.failed > 0}>
            {t('apps.effects.summary', { active: data.active, failed: data.failed })}
          </span>
        )}
      </button>
      {!open && data.failed > 0 && (
        <p role="status" className="tt-error tt-small">
          {t('apps.effects.summary', { active: data.active, failed: data.failed })}
        </p>
      )}
      {open && (
        <div id="tt-effects-panel" className="tt-effects-panel">
          <p className="tt-muted tt-small">{t('apps.effects.intro')}</p>
          {list.error && (
            <p role="alert" className="tt-error">
              {list.error}
            </p>
          )}
          {(data.failed > 0 || failedOnly) && (
            <label className="tt-inline-field">
              <input type="checkbox" checked={failedOnly} onChange={(e) => setFailedOnly(e.target.checked)} />
              {t('apps.effects.failedOnly', { count: data.failed })}
            </label>
          )}
          <EffectItems
            workspace={workspace}
            effects={shown}
            label={t('apps.effects.list')}
            onRetry={writable ? (e) => retry(base, e) : undefined}
            changed={reload}
          />
          {olderError && (
            <p role="alert" className="tt-error">
              {olderError}
            </p>
          )}
          {more && (
            <button type="button" className="tt-button" disabled={loadingOlder} onClick={showOlder}>
              {t('apps.effects.older')}
            </button>
          )}
        </div>
      )}
    </div>
  )
}

async function retry(base: string, effect: AppEffect): Promise<AppEffect> {
  return api<AppEffect>(`${base}/effects/${effect.id}/retry`, { method: 'POST', body: {} })
}

// A list of effects, each with its status, what it makes, why it failed and, for a failed one the
// caller may retry, the retry.
export function EffectItems({
  workspace,
  effects,
  label,
  onRetry,
  changed,
}: {
  // Without one, a done effect's result isn't linked.
  workspace?: number
  effects: AppEffect[]
  label: string
  onRetry?: (e: AppEffect) => Promise<AppEffect>
  changed?: () => void
}) {
  const t = useT()
  const f = useFormat()
  const [busy, setBusy] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const doRetry = async (e: AppEffect) => {
    if (!onRetry) return
    setBusy(e.id)
    setNotice(null)
    try {
      await onRetry(e)
      setError(null)
      setNotice(t('apps.effects.retried'))
    } catch (err) {
      setError(errorText(err))
    }
    setBusy(null)
    changed?.()
  }

  if (effects.length === 0) return <p className="tt-muted">{t('apps.effects.none')}</p>
  return (
    <>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {notice && (
        <p role="status" className="tt-muted tt-small">
          {notice}
        </p>
      )}
      <ul className="tt-effect-list" aria-label={label}>
        {effects.map((e) => (
          <li key={e.id || `${e.kind}-${e.position}`} className="tt-effect" data-status={e.status}>
            <div className="tt-effect-head">
              <strong>{t(`apps.effects.kind.${e.kind}` as Key)}</strong>
              <span className="tt-effect-status" data-status={e.status}>
                {t(`apps.effects.status.${e.status}` as Key)}
              </span>
            </div>
            <div className="tt-effect-detail">{detail(e, t)}</div>
            <div className="tt-muted tt-small">
              {[
                e.id > 0 ? t('apps.effects.recordAction', { id: e.record, action: e.action }) : e.action,
                e.attempts > 0 ? t('apps.effects.tries', { count: e.attempts }) : '',
                e.next_attempt_at ? t('apps.effects.nextTry', { when: f.dateTime(e.next_attempt_at) }) : '',
              ]
                .filter(Boolean)
                .join(' · ')}
            </div>
            {e.error && (
              <p
                className={e.status === 'failed' ? 'tt-error tt-small' : 'tt-muted tt-small'}
                role={e.status === 'failed' ? 'alert' : undefined}
              >
                {e.status === 'simulated'
                  ? t('apps.effects.preview.wouldFail', { error: codeText(e.error.code, e.error.params, e.error.code) })
                  : codeText(e.error.code, e.error.params, e.error.code)}
              </p>
            )}
            {e.status === 'done' && e.result && workspace !== undefined && (
              <ResultLink workspace={workspace} effect={e} />
            )}
            {e.can_retry && onRetry && (
              <div className="tt-effect-actions">
                <button
                  type="button"
                  className="tt-button tt-primary"
                  disabled={busy !== null}
                  aria-label={t('apps.effects.retryNamed', {
                    what: detail(e, t) || t(`apps.effects.kind.${e.kind}` as Key),
                  })}
                  onClick={() => doRetry(e)}
                >
                  {t('apps.effects.retry')}
                </button>
              </div>
            )}
          </li>
        ))}
      </ul>
    </>
  )
}

// What an effect makes, in a line: a card's title, a document's path, where a card goes.
function detail(e: AppEffect, t: ReturnType<typeof useT>): string {
  const d = e.detail
  switch (e.kind) {
    case 'create_card':
      return d.title ?? ''
    case 'update_card':
      return d.column ? t('apps.effects.toColumn', { column: t(`boards.columns.${d.column}` as Key) }) : ''
    case 'create_document':
      return d.path ?? ''
    default:
      return d.path ?? ''
  }
}

// Where a done effect's result is: the web app turns the kind and IDs the server gives into its own
// address (never one the server or a definition sent), in the workspace of the space it is in,
// which may not be the app's own. A card or a document is opened on, in its space.
function ResultLink({ workspace, effect }: { workspace: number; effect: AppEffect }) {
  const t = useT()
  const result = effect.result ?? {}
  const route = (result.route ?? {}) as Record<string, unknown>
  const where = (kind: Record<string, unknown>) => {
    const id = (key: string) =>
      Number.isInteger(Number(kind[key])) && Number(kind[key]) > 0 ? Number(kind[key]) : undefined
    return {
      workspace: id('workspace_id') ?? workspace,
      space: id('space_id'),
      board: id('board_id'),
      card: id('card_id'),
    }
  }
  const boards = (w: ReturnType<typeof where>) =>
    w.space === undefined
      ? null
      : routePath({ workspace: w.workspace, section: 'boards', item: boardItem(w.space, w.board, w.card) })
  const document = (w: ReturnType<typeof where>, path: unknown) =>
    w.space === undefined || typeof path !== 'string' || path === ''
      ? null
      : routePath({ workspace: w.workspace, section: 'knowledge', item: documentItem(w.space, path) })

  if (effect.kind === 'create_card' || effect.kind === 'update_card') {
    const w = where(result)
    const href = boards(w)
    if (!href || w.card === undefined) return null
    return (
      <Link className="tt-small" href={href}>
        {t('apps.effects.result.card', { id: w.card })}
      </Link>
    )
  }
  if (effect.kind === 'create_document') {
    const path = typeof result.path === 'string' ? result.path : ''
    const href = document(where(result), path)
    return href ? (
      <Link className="tt-small" href={href}>
        {t('apps.effects.result.document', { path })}
      </Link>
    ) : null
  }
  const w = where(route)
  if (route.kind === 'document') {
    const href = document(w, route.path)
    return href ? (
      <Link className="tt-small" href={href}>
        {t('apps.effects.result.documentRoute')}
      </Link>
    ) : null
  }
  if (route.kind === 'board' || route.kind === 'card') {
    const href = boards(w)
    return href ? (
      <Link className="tt-small" href={href}>
        {t('apps.effects.result.board')}
      </Link>
    ) : null
  }
  return null
}

// What a preview of an action of the draft would set going; nothing was saved.
export function EffectPreview({ workspace, effects }: { workspace?: number; effects: AppEffect[] }) {
  const t = useT()
  return (
    <section className="tt-effects-preview" aria-label={t('apps.effects.preview.title')}>
      <p role="status">
        <strong>{t('apps.effects.preview.title')}</strong>
      </p>
      <p className="tt-muted tt-small">
        {t(effects.length > 0 ? 'apps.effects.preview.intro' : 'apps.effects.preview.none')}
      </p>
      {effects.length > 0 && (
        <EffectItems workspace={workspace} effects={effects} label={t('apps.effects.preview.title')} />
      )}
    </section>
  )
}
