import { useEffect, useRef, useState } from 'react'
import {
  ApiError,
  type App,
  type ApprovalStatus,
  api,
  errorText,
  type Integration,
  type IntegrationApproval,
  type IntegrationStatus,
  type SlotStatus,
  type Space,
  ws,
} from '../api'
import { type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'
import { Loading } from '../settings/parts'
import { spaceLabel } from '../spaces'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Select } from '../ui/Select'
import { ContractSummary } from './ContractSummary'
import './Integrations.css'

export function useSlots(base: string, app: App) {
  const declares = Object.keys(app.draft?.integrations ?? {}).length > 0
  const slots = useLoad(
    () => (declares ? api<IntegrationStatus>(`${base}/integrations`) : Promise.resolve(null)),
    [base, declares, app.version],
  )
  const { reload } = slots
  const waiting = slots.data?.slots.some((s) => s.status === 'requested') ?? false
  useEffect(() => {
    if (!waiting) return
    const again = () => document.visibilityState !== 'hidden' && reload()
    window.addEventListener('focus', again)
    document.addEventListener('visibilitychange', again)
    return () => {
      window.removeEventListener('focus', again)
      document.removeEventListener('visibilitychange', again)
    }
  }, [waiting, reload])
  return slots
}

export const usable = (s: SlotStatus) => s.status === 'approved' && s.integration?.usable === true

export const undecided = (s: SlotStatus) => s.status !== 'approved'

const TONE: Record<ApprovalStatus, BadgeTone> = {
  none: 'neutral',
  requested: 'warning',
  approved: 'success',
  rejected: 'danger',
  revoked: 'danger',
}

export function ApprovalBadge({ status, usable: on = true }: { status: ApprovalStatus; usable?: boolean }) {
  const t = useT()
  if (status === 'approved' && !on)
    return (
      <Badge tone="warning" dot>
        {t('apps.integrations.status.unusable')}
      </Badge>
    )
  return (
    <Badge tone={TONE[status]} dot>
      {t(`apps.integrations.status.${status}` as Key)}
    </Badge>
  )
}

export function SlotsSection({
  workspace,
  base,
  slots,
  error,
  mayRequest,
  admin,
  reload,
}: {
  workspace: number
  base: string
  slots: IntegrationStatus | null | undefined
  error: string | null | undefined
  mayRequest: boolean
  admin: boolean
  reload: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState<string | null>(null)
  const [failed, setFailed] = useState<string | null>(null)

  const request = async (s: SlotStatus) => {
    setBusy(s.slot)
    setFailed(null)
    try {
      await api<IntegrationApproval>(`${base}/integrations/${encodeURIComponent(s.slot)}/request`, {
        method: 'POST',
        body: { contract_hash: s.contract_hash },
      })
    } catch (e) {
      setFailed(errorText(e))
    } finally {
      setBusy(null)
      reload()
    }
  }

  if (error) return <Notice tone="danger">{error}</Notice>
  if (!slots) return <Loading lines={2} />
  const queue = routePath({ workspace, section: 'settings', item: 'integration-approvals' })
  return (
    <div className="apps-slots">
      <p>{t('apps.integrations.intro')}</p>
      {slots.slots.map((s) => (
        <section key={s.slot} className="apps-slot" aria-label={t('apps.integrations.slotNamed', { slot: s.slot })}>
          <div className="apps-slot-head">
            <Heading level={4} size="subsection">
              {t('apps.integrations.slotNamed', { slot: s.slot })}
            </Heading>
            <ApprovalBadge status={s.status} usable={s.integration?.usable ?? true} />
          </div>
          <ContractSummary contract={s.contract} />
          {s.integration && s.status === 'approved' && (
            <p className="apps-note">
              {t(s.integration.usable ? 'apps.integrations.connectedTo' : 'apps.integrations.connectedUnusable', {
                name: s.integration.display_name,
              })}
            </p>
          )}
          {s.live && !s.live.usable && <Notice tone="warning">{t('apps.integrations.liveUnusable')}</Notice>}
          {s.status === 'requested' && (
            <p className="apps-note">
              {admin ? <Link href={queue}>{t('apps.integrations.openQueue')}</Link> : t('apps.integrations.waiting')}
            </p>
          )}
          {(s.status === 'rejected' || s.status === 'revoked') && (
            <p className="apps-note">{t(`apps.integrations.was.${s.status}` as Key)}</p>
          )}
          {mayRequest && (s.status === 'none' || s.status === 'rejected' || s.status === 'revoked') && (
            <div className="apps-actions">
              <Button loading={busy === s.slot} disabled={busy !== null} onClick={() => request(s)}>
                {t(s.status === 'none' ? 'apps.integrations.request' : 'apps.integrations.requestAgain')}
              </Button>
            </div>
          )}
        </section>
      ))}
      {failed && <Notice tone="danger">{failed}</Notice>}
    </div>
  )
}

export function ApproveAndPublishDialog({
  workspace,
  base,
  space,
  slots,
  onClose,
  onApproval,
  onApproved,
  onFailed,
  onContractChanged,
}: {
  workspace: number
  base: string
  space: Space
  slots: SlotStatus[]
  onClose: () => void
  onApproval: (slot: string) => void
  onApproved: (done: string[]) => Promise<string | null>
  onFailed: (done: string[]) => void
  onContractChanged: (done: string[]) => void
}) {
  const t = useT()
  const f = useFormat()
  const list = useLoad(() => api<{ integrations: Integration[] }>(`${ws(workspace)}/integrations`), [workspace])
  const [chosen, setChosen] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [done, setDone] = useState<string[]>([])
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const candidates = (s: SlotStatus) => (list.data?.integrations ?? []).filter((i) => i.kind === s.kind && !i.disabled)
  const pick = (s: SlotStatus): Integration | undefined => {
    const options = candidates(s)
    const id = chosen[s.slot] ?? (options.length === 1 ? String(options[0].id) : '')
    return options.find((i) => String(i.id) === id)
  }
  const nothingLeft = slots.length === 0
  const ready = list.data !== undefined && !nothingLeft && slots.every((s) => pick(s) !== undefined)
  const allowing = slots.filter((s) => {
    const i = pick(s)
    return i !== undefined && !i.spaces.includes(space.id)
  })

  const confirm = async () => {
    setBusy(true)
    setError(null)
    const approved = [...done]
    try {
      for (const s of slots) {
        const integration = pick(s)
        if (!integration) return
        let id = s.status === 'requested' ? s.approval_id : null
        if (id === null) {
          id = (
            await api<IntegrationApproval>(`${base}/integrations/${encodeURIComponent(s.slot)}/request`, {
              method: 'POST',
              body: { contract_hash: s.contract_hash },
            })
          ).id
        }
        await api(`${ws(workspace)}/integration-approvals/${id}/approve`, {
          method: 'POST',
          body: { integration_id: integration.id, allow_space: !integration.spaces.includes(space.id) },
        })
        approved.push(s.slot)
        onApproval(s.slot)
        setDone([...approved])
      }
      const refused = await onApproved(approved)
      if (refused !== null) setError(refused)
    } catch (e) {
      if (!alive.current) return
      if (
        e instanceof ApiError &&
        (e.code === 'app_integration_contract_changed' || e.code === 'app_integration_slot_unknown')
      ) {
        onContractChanged(approved)
        return
      }
      setError(errorText(e))
      list.reload()
      onFailed(approved)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      size="lg"
      title={t('apps.integrations.approve.title')}
      description={t('apps.integrations.approve.intro')}
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" loading={busy} disabled={!ready} onClick={confirm}>
            {t('apps.integrations.approve.confirm')}
          </Button>
        </>
      }
    >
      <div className="apps-stack">
        {list.error && <Notice tone="danger">{list.error}</Notice>}
        {nothingLeft && !busy && <Notice tone="neutral">{t('apps.integrations.approve.nothingLeft')}</Notice>}
        {slots.map((s) => (
          <section key={s.slot} className="apps-slot" aria-label={t('apps.integrations.slotNamed', { slot: s.slot })}>
            <Heading level={3} size="subsection">
              {t('apps.integrations.slotNamed', { slot: s.slot })}
            </Heading>
            <ContractSummary contract={s.contract} />
            <Field label={t('apps.integrations.approve.connect')} required>
              <Select
                value={pick(s) ? String(pick(s)?.id) : ''}
                disabled={busy || candidates(s).length === 0}
                onChange={(e) => setChosen((c) => ({ ...c, [s.slot]: e.target.value }))}
              >
                <option value="">{t('apps.integrations.approve.choose')}</option>
                {candidates(s).map((i) => (
                  <option key={i.id} value={i.id}>
                    {t('apps.integrations.approve.option', {
                      name: i.display_name,
                      mode: t(`settings.integrations.mode.${i.mode}` as Key),
                    })}
                  </option>
                ))}
              </Select>
            </Field>
            {list.data && candidates(s).length === 0 && (
              <Notice
                tone="warning"
                action={
                  <Link href={routePath({ workspace, section: 'settings', item: 'integrations' })}>
                    {t('apps.integrations.approve.openSettings')}
                  </Link>
                }
              >
                {t('apps.integrations.approve.none', { kind: t(`settings.integrations.kind.${s.kind}` as Key) })}
              </Notice>
            )}
          </section>
        ))}
        {allowing.length > 0 && (
          <p className="apps-note">{t('apps.integrations.approve.allowSpace', { space: spaceLabel(space) })}</p>
        )}
        <p>{t('apps.integrations.approve.consent')}</p>
        {space.kind === 'shared' && <p>{t('apps.builder.publish.confirmShared', { space: spaceLabel(space) })}</p>}
        {error && <Notice tone="danger">{error}</Notice>}
        {error && done.length > 0 && (
          <Notice tone="warning">{t('apps.integrations.approve.kept', { slots: f.list(done) })}</Notice>
        )}
      </div>
    </Dialog>
  )
}
