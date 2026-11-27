import { type FormEvent, useId, useState } from 'react'
import { type App, type AppRole, type AppTemplate, api, type Board, errorText, type Space, ws } from '../api'
import { has, type Key, useT } from '../i18n'
import { useLoad } from '../load'
import { useMounted } from '../mounted'
import { navigate, routePath } from '../router'
import { steps } from '../session'
import { spaceLabel } from '../spaces'
import { Avatar } from '../ui/Avatar'
import { Button, ButtonLink } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { appError, PublishPanel } from './AppPublish'
import { AppStage } from './AppStage'
import { LoadingRows } from './AppsLoading'
import { appPath, installedPath } from './route'
import { type Where, WhereChoice, writableSpaces } from './WhereChoice'
import './Apps.css'

function templateText(tpl: AppTemplate, part: 'name' | 'description', t: (k: Key) => string): string {
  const key = `apps.templates.items.${tpl.id.replaceAll('-', '_')}.${part}`
  return has(key) ? t(key as Key) : tpl[part]
}

export type BoardChoice = { space: Space; board: Board }

export function useWritableBoards(workspace: number, spaces: Space[] | undefined) {
  const writable = (spaces ?? []).filter((s) => s.access === 'write' && !s.archived_at)
  const key = writable.map((s) => s.id).join(',')
  return useLoad<BoardChoice[]>(async () => {
    const all = await Promise.all(
      writable.map(async (space) => {
        const r = await api<{ boards: Board[] }>(`${ws(workspace)}/spaces/${space.id}/boards`).catch(() => ({
          boards: [] as Board[],
        }))
        return r.boards.filter((b) => !b.archived_at).map((board) => ({ space, board }))
      }),
    )
    return all.flat()
  }, [workspace, key])
}

function BoardField({
  choices,
  value,
  onChange,
  hint,
}: {
  choices: BoardChoice[] | undefined
  value: string
  onChange: (v: string) => void
  hint?: string
}) {
  const t = useT()
  const none = choices !== undefined && choices.length === 0
  return (
    <Field
      label={t('apps.templates.board')}
      hint={[hint, none ? t('apps.templates.noBoards') : ''].filter(Boolean).join(' ') || undefined}
    >
      <Select value={value} onChange={(e) => onChange(e.target.value)} required>
        <option value="">{t('apps.templates.chooseBoard')}</option>
        {(choices ?? []).map((c) => (
          <option key={`${c.space.id}:${c.board.id}`} value={`${c.space.id}:${c.board.id}`}>
            {t('apps.templates.boardIn', { board: c.board.name, space: spaceLabel(c.space) })}
          </option>
        ))}
      </Select>
    </Field>
  )
}

function parseChoice(v: string): { space_id: number; board_id: number } | null {
  const [s, b] = v.split(':').map(Number)
  return s && b ? { space_id: s, board_id: b } : null
}

function TemplateFacts({ template }: { template: AppTemplate }) {
  const t = useT()
  return (
    <ul className="apps-facts">
      <li>{t(template.recommended_space === 'private' ? 'apps.templates.forPrivate' : 'apps.templates.forShared')}</li>
      {template.captures.length > 0 && (
        <li>
          {t('apps.templates.captures', {
            kinds: template.captures.map((c) => t(`apps.templates.capture.${c}`)).join(', '),
          })}
        </li>
      )}
      {template.needs_board && <li>{t('apps.templates.needsBoard')}</li>}
      {template.widgets.length > 0 && <li>{t('apps.templates.widgets')}</li>}
    </ul>
  )
}

export function TemplatesPage({ workspace }: { workspace: number }) {
  const t = useT()
  const templates = useLoad(
    () => api<{ templates: AppTemplate[] }>(`${ws(workspace)}/app-templates`).then((r) => r.templates),
    [workspace],
  )
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const [chosen, setChosen] = useState<AppTemplate | null>(null)

  return (
    <Page width="list">
      <PageHeader
        path={<BackToApps workspace={workspace} />}
        title={t('apps.templates.title')}
        help={t('apps.templates.intro')}
      />
      {(templates.error || spaces.error) && <Notice tone="danger">{templates.error ?? spaces.error}</Notice>}
      {!templates.data && !templates.error && <LoadingRows count={3} />}
      {templates.data && templates.data.length > 0 && (
        <Panel>
          <Rows label={t('apps.templates.title')}>
            {templates.data.map((tpl) => {
              const name = templateText(tpl, 'name', t)
              return (
                <Row
                  key={tpl.id}
                  leading={<Avatar name={name} shape="square" size="lg" decorative />}
                  title={name}
                  detail={
                    <>
                      <span className="apps-detail-line">{templateText(tpl, 'description', t)}</span>
                      <TemplateFacts template={tpl} />
                    </>
                  }
                  actions={
                    <Button
                      size="sm"
                      aria-label={t('apps.templates.useNamed', { name })}
                      onClick={() => setChosen(tpl)}
                    >
                      {t('apps.templates.use')}
                    </Button>
                  }
                />
              )
            })}
          </Rows>
        </Panel>
      )}
      {chosen && (
        <InstallDialog
          key={chosen.id}
          workspace={workspace}
          template={chosen}
          spaces={spaces.data}
          onClose={() => setChosen(null)}
        />
      )}
    </Page>
  )
}

export function BackToApps({ workspace }: { workspace: number }) {
  const t = useT()
  return (
    <ButtonLink variant="ghost" size="sm" icon="back" href={routePath({ workspace, section: 'apps' })}>
      {t('apps.all')}
    </ButtonLink>
  )
}

function InstallDialog({
  workspace,
  template,
  spaces,
  onClose,
}: {
  workspace: number
  template: AppTemplate
  spaces: Space[] | undefined
  onClose: () => void
}) {
  const t = useT()
  const formId = useId()
  const { mine, shared: sharedSpaces } = writableSpaces(spaces)
  const [name, setName] = useState(templateText(template, 'name', t))
  const [where, setWhere] = useState<Where>('private')
  const [shared, setShared] = useState('')
  const [board, setBoard] = useState('')
  const [busy, setBusy] = useState(false)
  const mounted = useMounted()
  const [error, setError] = useState<string | null>(null)
  const boards = useWritableBoards(workspace, template.needs_board ? spaces : undefined)
  const space = where === 'private' ? mine : sharedSpaces.find((s) => String(s.id) === shared)
  const target = parseChoice(board)
  const ready = !!space && !!name.trim() && (!template.needs_board || !!target)

  const install = async (e: FormEvent) => {
    e.preventDefault()
    if (!space || busy || !ready) return
    setBusy(true)
    setError(null)
    const s = steps(mounted)
    try {
      const made = await api<App>(`${ws(workspace)}/spaces/${space.id}/app-templates/${template.id}/install`, {
        method: 'POST',
        body: { name: name.trim(), ...(template.needs_board && target ? { board: target } : {}) },
      })
      if (s.ok()) return navigate(installedPath(workspace, space.id, made.id))
    } catch (err) {
      if (s.ok()) setError(appError(err))
    }
    if (mounted.current) setBusy(false)
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t('apps.templates.useNamed', { name: templateText(template, 'name', t) })}
      description={t('apps.templates.draftNote')}
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={busy} disabled={!ready}>
            {t('apps.templates.install')}
          </Button>
        </>
      }
    >
      <form id={formId} className="apps-form" onSubmit={install}>
        <Field label={t('apps.name')}>
          <Input value={name} maxLength={100} required onChange={(e) => setName(e.target.value)} data-autofocus />
        </Field>
        <WhereChoice spaces={spaces} where={where} onWhere={setWhere} shared={shared} onShared={setShared}>
          {where === 'shared' && template.roles && <RolesNote roles={template.roles} />}
          {where !== template.recommended_space && (
            <p className="apps-note">
              {t(
                template.recommended_space === 'shared'
                  ? 'apps.templates.betterShared'
                  : 'apps.templates.betterPrivate',
              )}
            </p>
          )}
        </WhereChoice>
        {template.needs_board && (
          <BoardField choices={boards.data} value={board} onChange={setBoard} hint={t('apps.templates.boardNote')} />
        )}
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}

function RolesNote({ roles }: { roles: NonNullable<AppTemplate['roles']> }) {
  const t = useT()
  const names = (list: AppRole[]) => list.map((r) => t(`apps.roles.${r}`)).join(', ')
  return (
    <p className="apps-note">
      {roles.members && roles.members.length > 0
        ? t('apps.templates.rolesShared', { installer: names(roles.installer), members: names(roles.members) })
        : t('apps.templates.rolesInstaller', { installer: names(roles.installer) })}
    </p>
  )
}

export function BoardChooser({
  workspace,
  base,
  app,
  spaces,
  onChanged,
}: {
  workspace: number
  base: string
  app: App
  spaces: Space[] | undefined
  onChanged: () => void
}) {
  const t = useT()
  const boards = useWritableBoards(workspace, spaces)
  const [board, setBoard] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const target = parseChoice(board)
  const choose = async (e: FormEvent) => {
    e.preventDefault()
    if (!target) return
    setBusy(true)
    setError(null)
    try {
      await api<App>(`${base}/effect-board`, { method: 'POST', body: { version: app.version, ...target } })
      onChanged()
    } catch (err) {
      setError(appError(err, app.name))
      onChanged()
    } finally {
      setBusy(false)
    }
  }
  return (
    <Panel padded>
      <form className="apps-form" onSubmit={choose} aria-label={t('apps.templates.board')}>
        <p>{t('apps.templates.boardNeeded')}</p>
        <BoardField choices={boards.data} value={board} onChange={setBoard} />
        {error && <Notice tone="danger">{error}</Notice>}
        <div className="apps-actions">
          <Button variant="primary" type="submit" loading={busy} disabled={!target}>
            {t('apps.templates.useBoard')}
          </Button>
        </div>
      </form>
    </Panel>
  )
}

export function InstalledApp({ workspace, space, app }: { workspace: number; space: number; app: number }) {
  const t = useT()
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const info = useLoad(() => api<App>(base), [base])
  const where = spaces.data?.find((s) => s.id === space)
  const mayReview = !!info.data && (info.data.access.design || info.data.access.publish)

  return (
    <Page width="list">
      <PageHeader
        path={<BackToApps workspace={workspace} />}
        title={info.data?.name ?? t('apps.app')}
        help={info.data && t(info.data.live ? 'apps.templates.liveNote' : 'apps.templates.installedNote')}
        actions={
          info.data?.live && (
            <ButtonLink href={appPath(workspace, space, app)}>
              {t('apps.openNamedInApps', { name: info.data.name })}
            </ButtonLink>
          )
        }
      />
      {(info.error || spaces.error) && <Notice tone="danger">{info.error ?? spaces.error}</Notice>}
      {!info.data && !info.error && <LoadingRows count={1} />}
      {info.data && (
        <>
          {mayReview ? (
            <AppStage key={base} base={base} app={info.data} builder />
          ) : (
            <p className="apps-note">{t('apps.templates.noReview')}</p>
          )}
          {info.data.needs_board && info.data.access.design && (
            <BoardChooser
              workspace={workspace}
              base={base}
              app={info.data}
              spaces={spaces.data}
              onChanged={info.reload}
            />
          )}
          {where && mayReview && (
            <PublishPanel
              base={base}
              spaceBase={`${ws(workspace)}/spaces/${space}`}
              app={info.data}
              space={where}
              onChanged={info.reload}
              headingLevel={2}
            />
          )}
        </>
      )}
    </Page>
  )
}

export function DuplicateDialog({
  workspace,
  space,
  app,
  spaces,
  onClose,
}: {
  workspace: number
  space: Space
  app: App
  spaces: Space[]
  onClose: () => void
}) {
  const t = useT()
  const formId = useId()
  const mine = spaces.find((s) => s.kind === 'private')
  const [name, setName] = useState(t('apps.duplicate.defaultName', { name: app.name }))
  const [to, setTo] = useState(String((mine ?? spaces[0])?.id ?? space.id))
  const [busy, setBusy] = useState(false)
  const mounted = useMounted()
  const [error, setError] = useState<string | null>(null)

  const duplicate = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !name.trim()) return
    setBusy(true)
    setError(null)
    const s = steps(mounted)
    try {
      const made = await api<App>(`${ws(workspace)}/spaces/${space.id}/apps/${app.id}/duplicate`, {
        method: 'POST',
        body: { name: name.trim(), space_id: Number(to) },
      })
      if (s.ok()) return navigate(installedPath(workspace, made.space_id, made.id))
    } catch (err) {
      if (s.ok()) setError(errorText(err))
    }
    if (mounted.current) setBusy(false)
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t('apps.duplicate.title', { name: app.name })}
      description={t('apps.duplicate.note')}
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={busy} disabled={!name.trim()}>
            {t('apps.duplicate.make')}
          </Button>
        </>
      }
    >
      <form id={formId} className="apps-form" onSubmit={duplicate}>
        <Field label={t('apps.name')}>
          <Input value={name} maxLength={100} required onChange={(e) => setName(e.target.value)} data-autofocus />
        </Field>
        <Field label={t('apps.duplicate.space')}>
          <Select value={to} onChange={(e) => setTo(e.target.value)} required>
            {spaces.map((s) => (
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
