// What happened to the events an integration's provider sent, for the workspace's admins: each
// event's kind, time, amount and state (applied, ignored or failed, with the reason), a retry for a
// failed one, and the payment links whose outcome at the provider is unknown, which an admin settles
// after looking at the provider's account. The server holds no record, app or space in an event and
// lists only the spaces the admin manages for a payment link, so this screen asks for nothing more:
// one integration of this workspace at a time, its events and its unresolved links. The server checks
// the role and the scope on every route; this only asks.

import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  api,
  codeText,
  errorText,
  type Integration,
  type IntegrationEvent,
  type IntegrationEventList,
  type IntegrationRegistry,
  type PaymentLinkAttempt,
  type PaymentLinkOutcome,
  type PaymentLinkOutcomes,
  type Space,
  ws,
} from '../api'
import { has, type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Select } from '../ui/Select'
import { decimalMoney, money } from './money'
import { CopyButton, Loading, Section, SettingsTab } from './parts'
import './Events.css'

type Filter = 'all' | 'failed' | 'ignored' | 'applied' | 'pending'
const FILTERS: Filter[] = ['all', 'failed', 'ignored', 'applied', 'pending']

// The codes whose event is worth more than its state's word: the payment was real and was not applied,
// so an admin reconciles it at the provider.
const SPECIAL: Record<string, { label: Key; tone: BadgeTone }> = {
  integration_event_second_payment: { label: 'settings.events.state.duplicate', tone: 'warning' },
  integration_amount_mismatch: { label: 'settings.events.state.mismatch', tone: 'warning' },
}

// What a retry that came too late says: the event is not failed any more, or is not there.
const STALE = ['integration_event_not_retriable', 'integration_event_not_found']

export function IntegrationEventsTab({ workspace }: { workspace: number }) {
  const t = useT()
  const registry = useLoad(() => api<IntegrationRegistry>(`${ws(workspace)}/integration-providers`), [workspace])
  const list = useLoad(
    () => api<{ integrations: Integration[] }>(`${ws(workspace)}/integrations`).then((r) => r.integrations),
    [workspace],
  )
  // The spaces the admin manages: the names of what the payment links list by number. Another
  // member's private space is not among them, and neither is it in what the server lists.
  const spaces = useLoad(
    () => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces?all=true`).then((r) => r.spaces),
    [workspace],
  )
  const [chosen, setChosen] = useState<number | null>(null)
  const integration = list.data?.find((i) => i.id === chosen) ?? list.data?.[0]
  const settings = routePath({ workspace, section: 'settings', item: 'integrations' })

  return (
    <SettingsTab title={t('settings.events.title')} help={t('settings.events.intro')}>
      {(registry.error ?? list.error ?? spaces.error) && (
        <Notice tone="danger">{registry.error ?? list.error ?? spaces.error}</Notice>
      )}
      {!list.data && !list.error && <Loading lines={3} />}
      {list.data?.length === 0 && (
        <Panel>
          <EmptyState
            title={t('settings.events.noIntegrations')}
            actions={<Link href={settings}>{t('settings.events.openIntegrations')}</Link>}
          />
        </Panel>
      )}
      {list.data && list.data.length > 1 && (
        <Field label={t('settings.events.integration')}>
          <Select value={String(integration?.id ?? '')} onChange={(e) => setChosen(Number(e.target.value))}>
            {list.data.map((i) => (
              <option key={i.id} value={i.id}>
                {i.display_name}
              </option>
            ))}
          </Select>
        </Field>
      )}
      {integration && registry.data && (
        // One integration's page: switching to another starts it afresh.
        <IntegrationEvents
          key={integration.id}
          workspace={workspace}
          integration={integration}
          exponents={registry.data.exponents}
          spaces={spaces.data ?? []}
        />
      )}
    </SettingsTab>
  )
}

function IntegrationEvents({
  workspace,
  integration,
  exponents,
  spaces,
}: {
  workspace: number
  integration: Integration
  exponents: Record<string, number>
  spaces: Space[]
}) {
  const base = `${ws(workspace)}/integrations/${integration.id}`
  return (
    <>
      <UnresolvedLinks base={base} spaces={spaces} />
      <Events base={base} exponents={exponents} />
    </>
  )
}

// --- The payment links that wait for an admin -------------------------------------------------

function UnresolvedLinks({ base, spaces }: { base: string; spaces: Space[] }) {
  const t = useT()
  const f = useFormat()
  const links = useLoad(
    () => api<PaymentLinkOutcomes>(`${base}/payment-links/unresolved`).then((r) => r.payment_links),
    [base],
  )
  const [confirmDialog, confirm] = useConfirm()
  const [busy, setBusy] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const space = (id: number): string => {
    const s = spaces.find((x) => x.id === id)
    if (!s) return t('settings.events.links.space', { id })
    return s.kind === 'private' ? t('knowledge.spaces.private') : s.name
  }

  const confirmNone = async (link: PaymentLinkOutcome) => {
    const amount = decimalMoney(f, link.amount, link.currency)
    const yes = await confirm({
      title: t('settings.events.links.confirmQuestion', { amount }),
      body: [
        t('settings.events.links.confirmBody', { amount, record: link.record_id, since: f.dateTime(link.since) }),
        t('settings.events.links.confirmWarning'),
      ].join(' '),
      confirmLabel: t('settings.events.links.confirmAction'),
      tone: 'danger',
    })
    if (!yes) return
    setBusy(link.effect_id)
    setError(null)
    setNotice(null)
    try {
      await api(`${base}/payment-links/${link.effect_id}/confirm-no-link`, { method: 'POST', body: {} })
      setNotice(t('settings.events.links.confirmed', { amount }))
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(null)
      // Whatever the answer, what stays is what the server holds now.
      links.reload()
    }
  }

  const rows = links.data ?? []
  return (
    <Section title={t('settings.events.links.title')} help={t('settings.events.links.intro')}>
      {notice && <Notice tone="success">{notice}</Notice>}
      {error && <Notice tone="danger">{error}</Notice>}
      {links.error && <Notice tone="danger">{links.error}</Notice>}
      {!links.data && !links.error && <Loading lines={2} />}
      {links.data && rows.length === 0 && (
        <Panel>
          <EmptyState title={t('settings.events.links.none')} />
        </Panel>
      )}
      {rows.length > 0 && (
        <>
          <Notice tone="warning" title={t('settings.events.links.checkTitle')} live={false}>
            <ol className="events-steps">
              <li>{t('settings.events.links.checkWhere')}</li>
              <li>{t('settings.events.links.checkSession')}</li>
              <li>{t('settings.events.links.checkPaymentLink')}</li>
              <li>{t('settings.events.links.checkValue')}</li>
              <li>{t('settings.events.links.checkFound')}</li>
              <li>{t('settings.events.links.checkNone')}</li>
            </ol>
          </Notice>
          <Panel>
            <Rows label={t('settings.events.links.list')}>
              {rows.map((l) => {
                const amount = decimalMoney(f, l.amount, l.currency)
                return (
                  <Row
                    key={l.effect_id}
                    title={t('settings.events.links.row', { amount })}
                    detail={
                      <span className="events-facts">
                        <span>
                          {t('settings.events.links.detail', {
                            record: l.record_id,
                            app: l.app_id,
                            space: space(l.space_id),
                          })}
                        </span>
                        <span>{t('settings.events.links.since', { when: f.dateTime(l.since) })}</span>
                      </span>
                    }
                    status={
                      l.can_confirm ? (
                        <Badge tone="warning" dot>
                          {t('settings.events.links.needsYou')}
                        </Badge>
                      ) : l.waiting ? (
                        <Badge tone="info">{t('settings.events.links.looking')}</Badge>
                      ) : (
                        <Badge tone="danger" dot>
                          {t('settings.events.links.stopped')}
                        </Badge>
                      )
                    }
                    actions={
                      l.can_confirm && (
                        <Button
                          size="sm"
                          loading={busy === l.effect_id}
                          disabled={busy !== null && busy !== l.effect_id}
                          aria-label={t('settings.events.links.confirmNamed', { amount })}
                          onClick={() => confirmNone(l)}
                        >
                          {t('settings.events.links.confirmAction')}
                        </Button>
                      )
                    }
                  >
                    {!l.can_confirm && !l.waiting && (
                      <Notice tone="warning" title={t('settings.events.links.stoppedTitle')} live={false}>
                        <p>
                          {codeText(l.code, undefined, t('settings.events.links.stoppedFallback'))}{' '}
                          {t('settings.events.links.stoppedDo')}
                        </p>
                      </Notice>
                    )}
                    <Attempts attempts={l.attempts} />
                  </Row>
                )
              })}
            </Rows>
          </Panel>
        </>
      )}
      {confirmDialog}
    </Section>
  )
}

// Each attempt at a payment: the reference to look for at the provider, in the field that kind of object
// carries it in, with any object seen and where it was made. The references are TaskTo's own random
// tokens, never a key of the provider's account.
const REFERENCE_FIELD: Record<PaymentLinkAttempt['kind'], string> = {
  checkout_session: 'client_reference_id',
  payment_link: 'metadata[taskto_ref]',
}

function Attempts({ attempts }: { attempts: PaymentLinkAttempt[] }) {
  const t = useT()
  const f = useFormat()
  if (attempts.length === 0) return null
  return (
    <>
      <p className="events-attempts-note">{t('settings.events.links.objectNote')}</p>
      <ol className="events-attempts" aria-label={t('settings.events.links.attemptsList')}>
        {attempts.map((a, i) => {
          const n = i + 1
          const kind = t(
            a.kind === 'checkout_session'
              ? 'settings.events.links.kindCheckoutSession'
              : 'settings.events.links.kindPaymentLink',
          )
          const mode = a.mode === 'live' || a.mode === 'test' ? t(`settings.events.${a.mode}` as Key) : a.mode
          return (
            <li key={a.reference} className="events-attempt">
              <strong className="events-attempt-title">{t('settings.events.links.attemptTitle', { n, kind })}</strong>
              <div className="events-reference">
                <Field label={t('settings.events.links.reference')}>
                  <Input readOnly size="sm" className="events-reference-field" value={a.reference} />
                </Field>
                <CopyButton size="sm" text={a.reference} name={t('settings.events.links.referenceName', { n })} />
              </div>
              <span className="events-attempt-facts">
                <span>
                  {t('settings.events.links.searchField')}{' '}
                  <code className="events-field-name">{REFERENCE_FIELD[a.kind]}</code>
                </span>
                <span>
                  {a.object_id
                    ? t(
                        a.object_verified
                          ? 'settings.events.links.objectSeenAccepted'
                          : 'settings.events.links.objectSeenNotAccepted',
                        { id: a.object_id },
                      )
                    : t('settings.events.links.objectNone')}
                </span>
                <span>
                  {a.account_id
                    ? t('settings.events.links.account', { account: a.account_id })
                    : t('settings.events.links.accountNone')}
                  {mode && ` · ${mode}`}
                </span>
                <span>
                  {a.first_sent_at
                    ? t('settings.events.links.firstSent', { when: f.dateTime(a.first_sent_at) })
                    : t('settings.events.links.notSent')}
                </span>
              </span>
            </li>
          )
        })}
      </ol>
    </>
  )
}

// --- The events -------------------------------------------------------------------------------

type Page = { items: IntegrationEvent[]; next: number }

function Events({ base, exponents }: { base: string; exponents: Record<string, number> }) {
  const t = useT()
  const f = useFormat()
  const [filter, setFilter] = useState<Filter>('all')
  const [page, setPage] = useState<Page | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [more, setMore] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)
  // The refusals of retries, by event: said under the event they are about.
  const [refused, setRefused] = useState<Record<number, string>>({})
  const [retrying, setRetrying] = useState<number | null>(null)
  // The refusal of a retry that came too late, said in the section: the list is read again and the
  // event may leave it (a failed filter no longer holds an event that isn't failed), so a row is no
  // place to keep it.
  const [late, setLate] = useState<string | null>(null)
  // The newest request for a list is the one that is shown: a slow answer for a filter left behind
  // is dropped.
  const latest = useRef(0)
  const query = filter === 'all' ? '' : `?state=${filter}`

  // biome-ignore lint/correctness/useExhaustiveDependencies: the list is read again for another filter, or when asked
  useEffect(() => {
    const mine = ++latest.current
    setPage(null)
    setError(null)
    api<IntegrationEventList>(`${base}/events${query}`)
      .then((r) => latest.current === mine && setPage({ items: r.events, next: r.next_before }))
      .catch((e: unknown) => latest.current === mine && setError(errorText(e)))
  }, [base, filter, nonce])

  const older = useCallback(async () => {
    if (!page?.next) return
    const mine = latest.current
    setMore(true)
    try {
      const r = await api<IntegrationEventList>(`${base}/events${query ? `${query}&` : '?'}before=${page.next}`)
      if (latest.current === mine) setPage({ items: [...page.items, ...r.events], next: r.next_before })
    } catch (e) {
      if (latest.current === mine) setError(errorText(e))
    } finally {
      setMore(false)
    }
  }, [page, base, query])

  const retry = async (event: IntegrationEvent) => {
    setRetrying(event.id)
    setNotice(null)
    setLate(null)
    setRefused(({ [event.id]: _gone, ...rest }) => rest)
    try {
      await api<IntegrationEvent>(`${base}/events/${event.id}/retry`, { method: 'POST', body: {} })
      setNotice(t('settings.events.retried', { kind: kindText(t, event.kind) }))
      setNonce((n) => n + 1)
    } catch (e) {
      if (e instanceof ApiError && STALE.includes(e.code ?? '')) {
        // The event is not what the list said: what stays is what the server holds now, and the
        // refusal with it, since the row it is about may be gone.
        setLate(
          t('settings.events.retryRefused', {
            kind: kindText(t, event.kind),
            when: f.dateTime(event.occurred_at),
            reason: errorText(e),
          }),
        )
        setNonce((n) => n + 1)
      } else {
        setRefused((r) => ({ ...r, [event.id]: errorText(e) }))
      }
    } finally {
      setRetrying(null)
    }
  }

  return (
    <Section title={t('settings.events.list.title')} help={t('settings.events.list.intro')}>
      <div className="events-filters">
        <SegmentedControl<Filter>
          label={t('settings.events.filter')}
          value={filter}
          onChange={(v) => {
            setNotice(null)
            setLate(null)
            setRefused({})
            setFilter(v)
          }}
          options={FILTERS.map((v) => ({ value: v, label: t(`settings.events.filters.${v}` as Key) }))}
        />
      </div>
      {notice && <Notice tone="success">{notice}</Notice>}
      {late && <Notice tone="danger">{late}</Notice>}
      {error && <Notice tone="danger">{error}</Notice>}
      {!page && !error && <Loading lines={3} />}
      {page && page.items.length === 0 && (
        <Panel>
          <EmptyState title={t(`settings.events.empty.${filter}` as Key)} />
        </Panel>
      )}
      {page && page.items.length > 0 && (
        <Panel>
          <Rows label={t('settings.events.list.label')}>
            {page.items.map((e) => (
              <EventRow
                key={e.id}
                event={e}
                amount={money(f, exponents, e.amount_minor, e.currency)}
                busy={retrying === e.id}
                disabled={retrying !== null && retrying !== e.id}
                refusal={refused[e.id]}
                onRetry={() => retry(e)}
              />
            ))}
          </Rows>
        </Panel>
      )}
      {page && page.next > 0 && (
        <div className="events-more">
          <Button loading={more} onClick={older}>
            {t('settings.events.older')}
          </Button>
        </div>
      )}
    </Section>
  )
}

type Translate = ReturnType<typeof useT>

// A kind as people say it: the closed list of the server's kinds, or the kind itself for a new one.
function kindText(t: Translate, kind: string): string {
  const key = `settings.events.kind.${kind.replace('.', '_')}`
  return has(key) ? t(key as Key) : kind
}

function EventRow({
  event: e,
  amount,
  busy,
  disabled,
  refusal,
  onRetry,
}: {
  event: IntegrationEvent
  amount: string
  busy: boolean
  disabled: boolean
  refusal: string | undefined
  onRetry: () => void
}) {
  const t = useT()
  const f = useFormat()
  const special = e.code ? SPECIAL[e.code] : undefined
  const tone: BadgeTone =
    e.state === 'applied' ? 'success' : e.state === 'failed' ? 'danger' : e.state === 'ignored' ? 'neutral' : 'info'
  const kind = kindText(t, e.kind)
  return (
    <Row
      title={kind}
      status={
        <Badge tone={special?.tone ?? tone} dot>
          {t(special?.label ?? (`settings.events.state.${e.state}` as Key))}
        </Badge>
      }
      detail={
        <span className="events-facts">
          <span>
            {amount}
            {' · '}
            {f.dateTime(e.occurred_at)}
            {' · '}
            {t(e.livemode ? 'settings.events.live' : 'settings.events.test')}
          </span>
          {e.code && <span>{codeText(e.code, undefined, e.code)}</span>}
          {e.state === 'failed' && e.attempts > 1 && (
            <span>{t('settings.events.attempts', { count: e.attempts })}</span>
          )}
        </span>
      }
      actions={
        e.retriable && (
          <Button
            size="sm"
            loading={busy}
            disabled={disabled}
            aria-label={t('settings.events.retryNamed', { kind, when: f.dateTime(e.occurred_at) })}
            onClick={onRetry}
          >
            {t('settings.events.retry')}
          </Button>
        )
      }
    >
      {refusal && <Notice tone="danger">{refusal}</Notice>}
    </Row>
  )
}
