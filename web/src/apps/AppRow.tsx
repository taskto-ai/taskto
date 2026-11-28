import { type FormEvent, useEffect, useEffectEvent, useId, useState } from 'react'
import { type App, api, type Space } from '../api'
import { useFormat, useT } from '../i18n'
import { spaceLabel } from '../spaces'
import { Badge } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { MenuItem, MenuSeparator, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Row } from '../ui/Rows'
import { Select } from '../ui/Select'
import { AppAvatar } from './AppAvatar'
import { appError, PublishPanel } from './AppPublish'
import { AppReleases } from './AppReleases'
import { AppRoles } from './AppRoles'
import { LoadingRows } from './AppsLoading'
import { DuplicateDialog } from './AppTemplates'
import { IconDialog } from './IconDialog'
import { appPath, builderPath } from './route'
import './Apps.css'

type Open = 'rename' | 'icon' | 'move' | 'duplicate' | 'releases' | 'roles' | 'publish' | null

export function AppRow({
  workspace,
  space,
  app,
  writable,
  targets,
  copyTo,
  spaceBase,
  change,
  move,
  report,
  reload,
}: {
  workspace: number
  space: Space
  app: App
  writable: boolean
  targets: Space[]
  copyTo: Space[]
  spaceBase: string
  change: (app: App, body: { name?: string; icon?: string; archived?: boolean }) => Promise<string | null>
  move: (app: App, to: number) => Promise<string | null>
  report: (error: string | null) => void
  reload: () => void
}) {
  const t = useT()
  const f = useFormat()
  const [busy, setBusy] = useState(false)
  const [open, setOpen] = useState<Open>(null)
  const [confirmDialog, confirm] = useConfirm()
  const offline = app.archived_at !== null
  const reviewer = writable && (app.access.design || app.access.publish)
  const appBase = `${spaceBase}/apps/${app.id}`
  const close = () => setOpen(null)

  const may = {
    publish: writable && app.access.publish && app.unpublished_changes,
    releases: reviewer && !!app.live,
    roles: writable && app.access.manage_roles && space.kind === 'shared',
    rename: writable && app.access.design,
    icon: writable && app.access.design,
    duplicate: copyTo.length > 0,
    move: writable && app.access.design && targets.length > 0,
    offline: writable && app.access.publish,
  }

  const run = async (body: { name?: string; icon?: string; archived?: boolean }) => {
    setBusy(true)
    const failed = await change(app, body)
    setBusy(false)
    return failed
  }
  const archive = async (archived: boolean) => report(await run({ archived }))
  const takeOffline = async () => {
    const yes = await confirm({
      title: t('apps.takeOfflineConfirm.title', { name: app.name }),
      body: t('apps.takeOfflineConfirm.body'),
      confirmLabel: t('apps.takeOffline'),
      tone: 'danger',
    })
    if (yes) await archive(true)
  }

  const detail = [
    t('apps.rows', { count: app.rows }),
    ...(app.live && !Number.isNaN(Date.parse(app.live.published_at))
      ? [t('apps.publishedOn', { date: f.date(app.live.published_at) })]
      : []),
  ].join(' · ')

  const status = (
    <>
      {offline && <Badge tone="neutral">{t('apps.offlineBadge')}</Badge>}
      {!offline && app.live && (
        <Badge tone="success" dot>
          {t('apps.stage.live', { number: app.live.number })}
        </Badge>
      )}
      {!offline && !app.live && (
        <Badge tone="warning" dot>
          {t('apps.notPublished')}
        </Badge>
      )}
      {!offline && app.live && app.unpublished_changes && (
        <Badge tone="warning" dot>
          {t('apps.unpublishedChanges')}
        </Badge>
      )}
    </>
  )

  const menu =
    may.publish || may.releases || may.roles || may.rename || may.icon || may.duplicate || may.move || may.offline
  const actions = (
    <>
      {!offline && writable && app.access.design && (
        <ButtonLink
          variant="ghost"
          size="sm"
          href={builderPath(workspace, { kind: 'edit', space: space.id, app: app.id })}
          aria-label={t('apps.builder.editNamed', { name: app.name })}
        >
          {t('apps.builder.edit')}
        </ButtonLink>
      )}
      {!offline && app.live && (
        <ButtonLink
          size="sm"
          href={appPath(workspace, space.id, app.id)}
          aria-label={t('apps.openNamed', { name: app.name })}
        >
          {t('apps.open')}
        </ButtonLink>
      )}
      {!offline && !app.live && reviewer && (
        <ButtonLink
          size="sm"
          href={appPath(workspace, space.id, app.id)}
          aria-label={t('apps.previewDraftNamed', { name: app.name })}
        >
          {t('apps.previewDraft')}
        </ButtonLink>
      )}
      {offline && may.offline && (
        <Button
          size="sm"
          loading={busy}
          onClick={() => archive(false)}
          aria-label={t('apps.restoreNamed', { name: app.name })}
        >
          {t('apps.restore')}
        </Button>
      )}
      {menu && (
        <MoreMenu label={t('apps.moreNamed', { name: app.name })}>
          {may.publish && (
            <MenuItem icon="upload" onSelect={() => setOpen('publish')}>
              {t('apps.publishItem')}
            </MenuItem>
          )}
          {may.releases && (
            <MenuItem icon="clock" onSelect={() => setOpen('releases')}>
              {t('apps.releases.open')}
            </MenuItem>
          )}
          {may.roles && (
            <MenuItem icon="settings" onSelect={() => setOpen('roles')}>
              {t('apps.roles.open')}
            </MenuItem>
          )}
          {may.rename && (
            <MenuItem icon="edit" disabled={busy} onSelect={() => setOpen('rename')}>
              {t('apps.rename')}
            </MenuItem>
          )}
          {may.icon && (
            <MenuItem icon="edit" disabled={busy} onSelect={() => setOpen('icon')}>
              {t('apps.changeIcon')}
            </MenuItem>
          )}
          {may.duplicate && (
            <MenuItem icon="copy" onSelect={() => setOpen('duplicate')}>
              {t('apps.duplicate.open')}
            </MenuItem>
          )}
          {may.move && (
            <MenuItem icon="move" disabled={busy} onSelect={() => setOpen('move')}>
              {t('apps.moveItem')}
            </MenuItem>
          )}
          {may.offline && !offline && (
            <>
              <MenuSeparator />
              <MenuItem icon="offline" tone="danger" disabled={busy} onSelect={takeOffline}>
                {t('apps.takeOffline')}
              </MenuItem>
            </>
          )}
        </MoreMenu>
      )}
    </>
  )

  const dialogs = (
    <>
      {confirmDialog}
      {open === 'rename' && (
        <RenameDialog
          app={app}
          onClose={close}
          onRename={async (name) => {
            const failed = await run({ name })
            if (!failed) close()
            return failed
          }}
        />
      )}
      {open === 'icon' && (
        <IconDialog
          app={app}
          onClose={close}
          onSave={async (icon) => {
            const failed = await run({ icon })
            if (!failed) close()
            return failed
          }}
        />
      )}
      {open === 'move' && (
        <MoveDialog
          app={app}
          targets={targets}
          onClose={close}
          onMove={async (to) => {
            setBusy(true)
            const failed = await move(app, to)
            setBusy(false)
            if (!failed) close()
            return failed
          }}
        />
      )}
      {open === 'duplicate' && (
        <DuplicateDialog workspace={workspace} space={space} app={app} spaces={copyTo} onClose={close} />
      )}
      {open === 'releases' && (
        <Dialog open onClose={close} title={t('apps.releases.openNamed', { name: app.name })} size="md">
          <AppReleases base={appBase} app={app} canRollBack={writable && app.access.publish} changed={reload} />
        </Dialog>
      )}
      {open === 'roles' && (
        <Dialog open onClose={close} title={t('apps.roles.openNamed', { name: app.name })} size="lg">
          <AppRoles base={appBase} spaceBase={spaceBase} />
        </Dialog>
      )}
      {open === 'publish' && (
        <PublishDialog
          appBase={appBase}
          spaceBase={spaceBase}
          app={app}
          space={space}
          reload={reload}
          onClose={close}
        />
      )}
    </>
  )

  return (
    <Row
      leading={<AppAvatar name={app.name} icon={app.icon} size="lg" decorative />}
      title={app.name}
      detail={detail}
      status={status}
      actions={
        <>
          {actions}
          {dialogs}
        </>
      }
    />
  )
}

function RenameDialog({
  app,
  onClose,
  onRename,
}: {
  app: App
  onClose: () => void
  onRename: (name: string) => Promise<string | null>
}) {
  const t = useT()
  const formId = useId()
  const [name, setName] = useState(app.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !name.trim()) return
    setBusy(true)
    setError(null)
    const failed = await onRename(name)
    setBusy(false)
    setError(failed)
  }
  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t('apps.renameNamed', { name: app.name })}
      size="sm"
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={busy} disabled={!name.trim()}>
            {t('apps.save')}
          </Button>
        </>
      }
    >
      <form id={formId} className="apps-form" onSubmit={submit}>
        <Field label={t('apps.name')}>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required data-autofocus />
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}

function MoveDialog({
  app,
  targets,
  onClose,
  onMove,
}: {
  app: App
  targets: Space[]
  onClose: () => void
  onMove: (to: number) => Promise<string | null>
}) {
  const t = useT()
  const formId = useId()
  const [to, setTo] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !to) return
    setBusy(true)
    setError(null)
    const failed = await onMove(Number(to))
    setBusy(false)
    setError(failed)
  }
  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t('apps.moveTitle', { name: app.name })}
      description={t('apps.moveNote')}
      size="sm"
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={busy} disabled={!to}>
            {t('apps.move')}
          </Button>
        </>
      }
    >
      <form id={formId} className="apps-form" onSubmit={submit}>
        <Field label={t('apps.moveNamed', { name: app.name })}>
          <Select value={to} onChange={(e) => setTo(e.target.value)} required data-autofocus>
            <option value="">{t('apps.moveTo')}</option>
            {targets.map((s) => (
              <option key={s.id} value={s.id}>
                {spaceLabel(s)}
              </option>
            ))}
          </Select>
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}

function PublishDialog({
  appBase,
  spaceBase,
  app,
  space,
  reload,
  onClose,
}: {
  appBase: string
  spaceBase: string
  app: App
  space: Space
  reload: () => void
  onClose: () => void
}) {
  const t = useT()
  const [reviewing, setReviewing] = useState<App | null>(null)
  const [error, setError] = useState<string | null>(null)
  const read = async () => {
    try {
      setReviewing(await api<App>(appBase))
      setError(null)
    } catch (e) {
      setReviewing(null)
      setError(appError(e, app.name))
    }
  }
  const readOnOpen = useEffectEvent(() => void read())
  useEffect(() => readOnOpen(), [])
  return (
    <Dialog open onClose={onClose} title={t('apps.publishNamed', { name: app.name })} size="md">
      {error && <Notice tone="danger">{error}</Notice>}
      {!reviewing && !error && <LoadingRows count={1} />}
      {reviewing && (
        <PublishPanel
          base={appBase}
          spaceBase={spaceBase}
          app={reviewing}
          space={space}
          inDialog
          onChanged={() => {
            reload()
            void read()
          }}
        />
      )}
    </Dialog>
  )
}
