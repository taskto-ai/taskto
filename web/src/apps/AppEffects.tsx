// What an app's actions set going: cards, documents and places to open. They are done by the
// server in the background, as the person who acted, so this only shows how they are getting on
// and, for one that failed, lets the person who acted retry it. The server decides what each
// person sees and may retry: this only asks.
//
// The same list (EffectItems) shows what a preview of a draft's action would set going.

import { useCallback, useEffect, useRef, useState } from 'react'
import { type AppEffect, type AppEffectList, api, codeText, errorText } from '../api'
import { type Format, type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { boardItem, documentItem, Link, routePath } from '../router'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Notice } from '../ui/Notice'
import { Row, Rows } from '../ui/Rows'
import './AppEffects.css'

// How often the list is asked for again while some effect waits or runs.
const POLL_MS = 3000
// How many effects a page holds.
const PAGE = 50
// The most the server lists at once.
const MAX_PAGE = 200

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
  const olderRef = useRef(older)
  olderRef.current = older
  // The older pages loaded are read again as one request (they are the newest first, so what they
  // cover is everything older than their first effect): an effect on one of them that is retried,
  // or that was waiting, is shown as it is now, not as it was when the page was loaded.
  const refreshOlder = useCallback(async () => {
    const kept = olderRef.current
    const first = kept?.effects[0]
    if (!kept || !first) return
    const limit = Math.min(Math.max(kept.effects.length, PAGE), MAX_PAGE)
    try {
      const page = await api<AppEffectList>(`${base}/effects?${query}limit=${limit}&before=${first.id + 1}`)
      setOlder({ effects: page.effects, more: page.more ?? false })
    } catch (err) {
      setOlderError(errorText(err))
    }
  }, [base, query])
  const waiting = (list.data?.active ?? 0) > 0
  const { reload } = list
  const changed = useCallback(() => {
    reload()
    refreshOlder()
  }, [reload, refreshOlder])
  useEffect(() => {
    if (!waiting) return
    const timer = setInterval(changed, POLL_MS)
    return () => clearInterval(timer)
  }, [waiting, changed])

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
  const summary = t('apps.effects.summary', { active: data.active, failed: data.failed })
  return (
    <section className="ui-effects" aria-label={t('apps.effects.open')}>
      <div className="ui-effects-head">
        <Button
          size="sm"
          variant="ghost"
          icon={open ? 'chevron-up' : 'chevron-down'}
          aria-expanded={open}
          aria-controls="ui-effects-panel"
          onClick={() => setOpen((o) => !o)}
        >
          {open ? t('apps.effects.close') : t('apps.effects.open')}
        </Button>
        {(data.active > 0 || data.failed > 0) && (
          <p role="status" className="ui-effects-summary">
            <Badge tone={data.failed > 0 ? 'danger' : 'info'} dot>
              {summary}
            </Badge>
          </p>
        )}
      </div>
      {open && (
        <div id="ui-effects-panel" className="ui-effects-panel">
          <p className="ui-effects-note">{t('apps.effects.intro')}</p>
          {list.error && <Notice tone="danger">{list.error}</Notice>}
          {(data.failed > 0 || failedOnly) && (
            <Checkbox
              label={t('apps.effects.failedOnly', { count: data.failed })}
              checked={failedOnly}
              onChange={(e) => setFailedOnly(e.target.checked)}
            />
          )}
          <EffectItems
            workspace={workspace}
            effects={shown}
            label={t('apps.effects.list')}
            onRetry={writable ? (e) => retry(base, e) : undefined}
            changed={changed}
          />
          {olderError && <Notice tone="danger">{olderError}</Notice>}
          {more && (
            <Button size="sm" loading={loadingOlder} disabled={loadingOlder} onClick={showOlder}>
              {t('apps.effects.older')}
            </Button>
          )}
        </div>
      )}
    </section>
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

  if (effects.length === 0) return <p className="ui-effects-note">{t('apps.effects.none')}</p>
  return (
    <>
      {error && <Notice tone="danger">{error}</Notice>}
      {notice && <Notice tone="success">{notice}</Notice>}
      <Rows label={label}>
        {effects.map((e) => {
          const what = detail(e, t, f)
          const meta = [
            e.id > 0 ? t('apps.effects.recordAction', { id: e.record, action: e.action }) : e.action,
            e.attempts > 0 ? t('apps.effects.tries', { count: e.attempts }) : '',
            e.next_attempt_at ? t('apps.effects.nextTry', { when: f.dateTime(e.next_attempt_at) }) : '',
          ]
            .filter(Boolean)
            .join(' · ')
          return (
            <Row
              key={e.id || `${e.kind}-${e.position}`}
              title={
                <>
                  {t(`apps.effects.kind.${e.kind}` as Key)}
                  {what && <span className="ui-effect-what">{what}</span>}
                </>
              }
              status={
                <Badge tone={STATUS_TONE[e.status] ?? 'neutral'} dot>
                  {t(`apps.effects.status.${e.status}` as Key)}
                </Badge>
              }
              detail={
                <>
                  <span className="ui-effect-line">{meta}</span>
                  {e.error && (
                    <span
                      className="ui-effect-line"
                      data-tone={e.status === 'failed' ? 'danger' : undefined}
                      role={e.status === 'failed' ? 'alert' : undefined}
                    >
                      {e.status === 'simulated'
                        ? t('apps.effects.preview.wouldFail', {
                            error: codeText(e.error.code, e.error.params, e.error.code),
                          })
                        : codeText(e.error.code, e.error.params, e.error.code)}
                    </span>
                  )}
                  {e.status === 'done' && e.result && workspace !== undefined && (
                    <span className="ui-effect-line">
                      <ResultLink workspace={workspace} effect={e} />
                    </span>
                  )}
                </>
              }
              actions={
                e.can_retry && onRetry ? (
                  <Button
                    size="sm"
                    loading={busy === e.id}
                    disabled={busy !== null}
                    aria-label={t('apps.effects.retryNamed', {
                      what: what || t(`apps.effects.kind.${e.kind}` as Key),
                    })}
                    onClick={() => doRetry(e)}
                  >
                    {t('apps.effects.retry')}
                  </Button>
                ) : undefined
              }
            />
          )
        })}
      </Rows>
    </>
  )
}

// The tone of each status: what waits is neutral, what runs informs, what is done succeeded, what
// failed is the danger; a preview's effects are only simulated.
const STATUS_TONE: Record<string, BadgeTone> = {
  pending: 'neutral',
  running: 'info',
  done: 'success',
  failed: 'danger',
  simulated: 'neutral',
}

// What an effect makes, in a line: a card's title, a document's path, where a card goes.
function detail(e: AppEffect, t: ReturnType<typeof useT>, f: Pick<Format, 'number'>): string {
  const d = e.detail
  switch (e.kind) {
    case 'create_payment_link': {
      // The amount is exact decimal text; only its showing is the locale's. An amount the server
      // refused is shown as it is, exactly (a number would round 120.123 to 120.12, which is not what
      // the record holds), and so is one that is not a number.
      const n = Number(d.amount)
      if (!d.amount || !d.currency) return d.title ?? ''
      if (!Number.isFinite(n) || e.error?.code === 'integration_amount_refused') return `${d.amount} ${d.currency}`
      try {
        return f.number(n, { style: 'currency', currency: d.currency })
      } catch {
        return `${d.amount} ${d.currency}`
      }
    }
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
      <Link className="ui-effect-link" href={href}>
        {t('apps.effects.result.card', { id: w.card })}
      </Link>
    )
  }
  if (effect.kind === 'create_document') {
    const path = typeof result.path === 'string' ? result.path : ''
    const href = document(where(result), path)
    return href ? (
      <Link className="ui-effect-link" href={href}>
        {t('apps.effects.result.document', { path })}
      </Link>
    ) : null
  }
  const w = where(route)
  if (route.kind === 'document') {
    const href = document(w, route.path)
    return href ? (
      <Link className="ui-effect-link" href={href}>
        {t('apps.effects.result.documentRoute')}
      </Link>
    ) : null
  }
  if (route.kind === 'board' || route.kind === 'card') {
    const href = boards(w)
    return href ? (
      <Link className="ui-effect-link" href={href}>
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
    <section className="ui-effects-preview" aria-label={t('apps.effects.preview.title')}>
      <Notice tone="info" title={t('apps.effects.preview.title')}>
        {t(effects.length > 0 ? 'apps.effects.preview.intro' : 'apps.effects.preview.none')}
      </Notice>
      {effects.length > 0 && (
        <EffectItems workspace={workspace} effects={effects} label={t('apps.effects.preview.title')} />
      )}
    </section>
  )
}
