// The approvals of apps' use of outside services, for the workspace's admins: what each app asks
// (the contract, in plain words), by whom, in which space, and the decision: approve, choosing
// which of the workspace's integrations it is connected to, reject, or revoke what was approved. The
// queue holds only what the requester submitted and the names of the app and its space: an admin
// reads no app and no record of it, which is why an app in someone's private space is approved the
// same way. The server checks the role on every route; this only asks.

import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  api,
  errorText,
  type Integration,
  type IntegrationApproval,
  type IntegrationApprovalList,
  type Space,
  ws,
} from '../api'
import { ApprovalBadge } from '../apps/AppIntegrations'
import { ContractSummary } from '../apps/ContractSummary'
import { type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Select } from '../ui/Select'
import { Loading, SettingsTab } from './parts'
import './Approvals.css'

type Filter = 'requested' | 'approved' | 'rejected' | 'revoked'
const FILTERS: Filter[] = ['requested', 'approved', 'rejected', 'revoked']

// What a decision on a request that was already decided says: the row on screen is out of date.
const STALE = ['app_integration_not_pending', 'app_integration_not_approved']

type Page = { items: IntegrationApproval[]; next: string | null }

export function ApprovalsTab({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const [filter, setFilter] = useState<Filter>('requested')
  const [page, setPage] = useState<Page | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [more, setMore] = useState(false)
  const [reviewing, setReviewing] = useState<IntegrationApproval | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  // A decision that came too late (someone else decided the request first): said above the list.
  const [problem, setProblem] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)
  // The newest request for a list is the one that is shown: a slow answer for a filter left behind
  // is dropped.
  const latest = useRef(0)

  // biome-ignore lint/correctness/useExhaustiveDependencies: the list is read again for another filter or after a decision
  useEffect(() => {
    const mine = ++latest.current
    setPage(null)
    setError(null)
    api<IntegrationApprovalList>(`${ws(workspace)}/integration-approvals?status=${filter}`)
      .then((r) => latest.current === mine && setPage({ items: r.approvals, next: r.next_cursor }))
      .catch((e: unknown) => latest.current === mine && setError(errorText(e)))
  }, [workspace, filter, nonce])

  const older = useCallback(async () => {
    if (!page?.next) return
    const mine = latest.current
    setMore(true)
    try {
      const r = await api<IntegrationApprovalList>(
        `${ws(workspace)}/integration-approvals?status=${filter}&cursor=${encodeURIComponent(page.next)}`,
      )
      if (latest.current === mine) setPage({ items: [...page.items, ...r.approvals], next: r.next_cursor })
    } catch (e) {
      if (latest.current === mine) setError(errorText(e))
    } finally {
      setMore(false)
    }
  }, [page, workspace, filter])

  const decided = (message: string) => {
    setReviewing(null)
    setProblem(null)
    setNotice(message)
    setNonce((n) => n + 1)
  }
  // The request was decided meanwhile: the dialog closes and the list is read again, so what stays is
  // what the server holds now, never the row that was open.
  const stale = (message: string) => {
    setReviewing(null)
    setNotice(null)
    setProblem(message)
    setNonce((n) => n + 1)
  }

  return (
    <SettingsTab title={t('settings.approvals.title')} help={t('settings.approvals.intro')}>
      <div className="approvals-filters">
        <SegmentedControl<Filter>
          label={t('settings.approvals.filter')}
          value={filter}
          onChange={(v) => {
            setNotice(null)
            setProblem(null)
            setFilter(v)
          }}
          options={FILTERS.map((v) => ({ value: v, label: t(`settings.approvals.filters.${v}` as Key) }))}
        />
      </div>
      {notice && <Notice tone="success">{notice}</Notice>}
      {problem && <Notice tone="danger">{problem}</Notice>}
      {error && <Notice tone="danger">{error}</Notice>}
      {!page && !error && <Loading lines={3} />}
      {page && page.items.length === 0 && (
        <Panel>
          <EmptyState title={t(`settings.approvals.empty.${filter}` as Key)} />
        </Panel>
      )}
      {page && page.items.length > 0 && (
        <Panel>
          <Rows label={t('settings.approvals.list')}>
            {page.items.map((a) => (
              <Row
                key={a.id}
                title={a.app_name}
                onOpen={() => setReviewing(a)}
                detail={t('settings.approvals.detail', {
                  slot: a.slot,
                  space: a.space_kind === 'private' ? t('knowledge.spaces.private') : a.space_name,
                  who: a.requested_by?.name ?? t('settings.approvals.unknownPerson'),
                  when: f.dateTime(a.requested_at),
                })}
                status={<ApprovalBadge status={a.status} usable={a.integration?.usable ?? true} />}
                actions={
                  <Button
                    size="sm"
                    onClick={() => setReviewing(a)}
                    aria-label={t('settings.approvals.reviewNamed', { app: a.app_name })}
                  >
                    {t(a.status === 'requested' ? 'settings.approvals.review' : 'settings.approvals.view')}
                  </Button>
                }
              />
            ))}
          </Rows>
        </Panel>
      )}
      {page?.next && (
        <div className="approvals-more">
          <Button loading={more} onClick={older}>
            {t('settings.approvals.older')}
          </Button>
        </div>
      )}
      {reviewing && (
        <ReviewDialog
          workspace={workspace}
          approval={reviewing}
          onClose={() => setReviewing(null)}
          onDecided={decided}
          onStale={stale}
        />
      )}
    </SettingsTab>
  )
}

// One request, whole: the contract in words, who asked, and what an admin may do with it as it stands.
function ReviewDialog({
  workspace,
  approval: a,
  onClose,
  onDecided,
  onStale,
}: {
  workspace: number
  approval: IntegrationApproval
  onClose: () => void
  onDecided: (message: string) => void
  onStale: (message: string) => void
}) {
  const t = useT()
  const f = useFormat()
  const waiting = a.status === 'requested'
  const approved = a.status === 'approved'
  // A request that waits is decided with an integration, which only it reads.
  const integrations = useLoad(
    () => (waiting ? api<{ integrations: Integration[] }>(`${ws(workspace)}/integrations`) : Promise.resolve(null)),
    [workspace, waiting],
  )
  // The spaces the admin manages: the ones whose grant they can keep in the integration's settings.
  // Another member's private space is not among them, and neither the integrations' lists nor this
  // one say anything of it, so what its approval made is read from the approval itself.
  const spaces = useLoad(
    () =>
      waiting || approved
        ? api<{ spaces: Space[] }>(`${ws(workspace)}/spaces?all=true`).then((r) => r.spaces)
        : Promise.resolve(null),
    [workspace, waiting, approved],
  )
  const [chosen, setChosen] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmDialog, confirm] = useConfirm()

  const options = (integrations.data?.integrations ?? []).filter((i) => i.kind === a.kind && !i.disabled)
  const picked = options.find((i) => String(i.id) === (chosen || (options.length === 1 ? String(options[0].id) : '')))
  // The admin can't open another person's private space to allow the integration there: approving is
  // how they consent to the app using it, so the space is allowed with the approval when it isn't yet.
  const allowing = picked !== undefined && !picked.spaces.includes(a.space_id)
  const space = a.space_kind === 'private' ? t('knowledge.spaces.private') : a.space_name
  // Whether the admin can keep the space's grant, in Settings, Integrations: only a space they manage.
  // Known only once the lookup has succeeded: a failed one is said, and guides neither way.
  const keepable = spaces.data == null ? undefined : spaces.data.some((s) => s.id === a.space_id)

  // The grant of the space the approval made: it ends with the last approval that needs it, unless
  // an admin keeps it in the integration's spaces. The queue says it of this approval's own space, so
  // it is known for a space the admin can't list too. Read again before a revoke is confirmed
  // (`revoke`), since another session may have kept the grant since the list was read.
  const madeGrantOf = (row: Pick<IntegrationApproval, 'status' | 'space_grant'>) =>
    row.status === 'approved' && row.space_grant === 'approval'

  const decide = async (verb: 'approve' | 'reject' | 'revoke', message: string) => {
    setBusy(true)
    setError(null)
    try {
      await api(`${ws(workspace)}/integration-approvals/${a.id}/${verb}`, {
        method: 'POST',
        body: verb === 'approve' ? { integration_id: picked?.id, allow_space: allowing } : {},
      })
      onDecided(message)
    } catch (e) {
      if (e instanceof ApiError && STALE.includes(e.code ?? '')) onStale(errorText(e))
      else {
        setError(errorText(e))
        setBusy(false)
      }
    }
  }

  // The approval as the server holds it now, not as the list said when it was read: the warning the
  // admin confirms must describe what the revoke does, so both what it is live as and the grant it made.
  const refreshed = async (): Promise<IntegrationApproval> => {
    let cursor: string | null = null
    do {
      const r: IntegrationApprovalList = await api(
        `${ws(workspace)}/integration-approvals?status=approved${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      )
      const found = r.approvals.find((x) => x.id === a.id)
      if (found) return found
      cursor = r.next_cursor
    } while (cursor)
    return a
  }

  const revoke = async () => {
    let row: IntegrationApproval = a
    setBusy(true)
    try {
      row = await refreshed()
    } catch {
      // The list's word stands; the server decides the revoke either way.
    } finally {
      setBusy(false)
    }
    const yes = await confirm({
      title: t('settings.approvals.revokeQuestion', { app: a.app_name }),
      body: [
        t(row.live ? 'settings.approvals.revokeLive' : 'settings.approvals.revokeBody'),
        madeGrantOf(row)
          ? t(keepable === false ? 'settings.approvals.revokeSpaceTemporary' : 'settings.approvals.revokeSpace', {
              space,
            })
          : '',
      ]
        .filter(Boolean)
        .join(' '),
      confirmLabel: t('settings.approvals.revoke'),
      tone: 'danger',
    })
    if (yes) await decide('revoke', t('settings.approvals.revoked', { app: a.app_name }))
  }

  return (
    <>
      <Dialog
        open
        onClose={onClose}
        dismissible={!busy}
        size="lg"
        title={t('settings.approvals.dialogTitle', { app: a.app_name })}
        description={t('settings.approvals.dialogDetail', {
          slot: a.slot,
          space,
          who: a.requested_by?.name ?? t('settings.approvals.unknownPerson'),
          when: f.dateTime(a.requested_at),
        })}
        actions={
          <>
            <Button disabled={busy} onClick={onClose}>
              {t('common.close')}
            </Button>
            {waiting && (
              <Button
                variant="danger"
                disabled={busy}
                onClick={() => decide('reject', t('settings.approvals.rejected', { app: a.app_name }))}
              >
                {t('settings.approvals.reject')}
              </Button>
            )}
            {waiting && (
              <Button
                variant="primary"
                loading={busy}
                disabled={!picked}
                onClick={() => decide('approve', t('settings.approvals.approved', { app: a.app_name }))}
              >
                {t('settings.approvals.approve')}
              </Button>
            )}
            {a.status === 'approved' && (
              <Button variant="danger" disabled={busy} onClick={revoke}>
                {t('settings.approvals.revoke')}
              </Button>
            )}
          </>
        }
      >
        <div className="approvals-review">
          <div className="approvals-status">
            <ApprovalBadge status={a.status} usable={a.integration?.usable ?? true} />
            {a.live && <span className="approvals-note">{t('settings.approvals.isLive')}</span>}
          </div>
          <ContractSummary contract={a.contract} />
          {a.integration && a.status === 'approved' && (
            <p className="approvals-note">
              {t(a.integration.usable ? 'apps.integrations.connectedTo' : 'apps.integrations.connectedUnusable', {
                name: a.integration.display_name,
              })}
            </p>
          )}
          {a.decided_by && a.decided_at && (
            <p className="approvals-note">
              {t('settings.approvals.decidedBy', { who: a.decided_by.name, when: f.dateTime(a.decided_at) })}
            </p>
          )}
          {waiting && (
            <>
              <Field label={t('apps.integrations.approve.connect')} required>
                <Select
                  value={picked ? String(picked.id) : ''}
                  disabled={busy || options.length === 0}
                  onChange={(e) => setChosen(e.target.value)}
                >
                  <option value="">{t('apps.integrations.approve.choose')}</option>
                  {options.map((i) => (
                    <option key={i.id} value={i.id}>
                      {t('apps.integrations.approve.option', {
                        name: i.display_name,
                        mode: t(`settings.integrations.mode.${i.mode}` as Key),
                      })}
                    </option>
                  ))}
                </Select>
              </Field>
              {integrations.error && <Notice tone="danger">{integrations.error}</Notice>}
              {integrations.data && options.length === 0 && (
                <Notice
                  tone="warning"
                  action={
                    <Link href={routePath({ workspace, section: 'settings', item: 'integrations' })}>
                      {t('apps.integrations.approve.openSettings')}
                    </Link>
                  }
                >
                  {t('apps.integrations.approve.none', { kind: t(`settings.integrations.kind.${a.kind}` as Key) })}
                </Notice>
              )}
              {picked && keepable === true && allowing && (
                <p className="approvals-note">
                  {t('settings.approvals.allowSpace', { space })}{' '}
                  <Link href={routePath({ workspace, section: 'settings', item: 'integrations' })}>
                    {t('settings.approvals.keepLink')}
                  </Link>
                </p>
              )}
              {/* A space the admin doesn't manage (another member's private space) has no setting to keep
                  its grant in, and the integrations' list doesn't say whether it has one: the grant lasts as
                  long as the approval, whatever it was before. */}
              {picked && keepable === false && (
                <p className="approvals-note">{t('settings.approvals.allowSpaceTemporary')}</p>
              )}
            </>
          )}
          {spaces.error && <Notice tone="danger">{spaces.error}</Notice>}
          {error && <Notice tone="danger">{error}</Notice>}
        </div>
      </Dialog>
      {confirmDialog}
    </>
  )
}
