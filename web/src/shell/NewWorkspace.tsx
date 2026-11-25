// "New workspace" in the top bar, beside the workspace switcher: a small form in the same kind
// of popover as the account menu. The server decides who may (`can_create_workspace` in
// /api/me, the operator's -workspace-creation); the shell only offers it to those it says yes to.

import { type FormEvent, useId, useRef, useState } from 'react'
import { api, errorText, forAccount, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Icon } from './Icon'

// The answer of POST /api/workspaces: what /api/me says now, and the new workspace.
type Created = Me & { workspace: Workspace }

const MAX_NAME = 100

// `user`: the account the form was opened for; the server refuses the request (409) if the browser's session
// became another account's since, so a workspace is never made for someone the person didn't mean.
export function NewWorkspace({ user, onCreated }: { user: number; onCreated: (me: Me, workspace: Workspace) => void }) {
  const t = useT()
  const details = useRef<HTMLDetailsElement>(null)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      const { workspace, ...me } = await api<Created>('/api/workspaces', {
        method: 'POST',
        headers: forAccount(user),
        body: { name: name.trim() },
      })
      setName('')
      details.current?.removeAttribute('open')
      onCreated(me, workspace)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <details ref={details} className="tt-account tt-new-workspace">
      <summary className="tt-icon-button" aria-label={t('shell.newWorkspace')} title={t('shell.newWorkspace')}>
        <Icon name="plus" />
      </summary>
      <form className="tt-menu" onSubmit={submit}>
        <label>
          {t('shell.newWorkspaceName')}
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={MAX_NAME} />
        </label>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        <button type="submit" className="tt-button tt-primary" disabled={busy || name.trim() === ''}>
          {t('shell.createWorkspace')}
        </button>
      </form>
    </details>
  )
}

// The same form in a dialog, for the screen of someone with no workspace yet, where a popover
// hanging from a button doesn't fit a phone. Its props are the shell's: `user` as above.
export interface NewWorkspaceDialogProps {
  open: boolean
  onClose: () => void
  user: number
  onCreated: (me: Me, workspace: Workspace) => void
}

export function NewWorkspaceDialog({ open, onClose, user, onCreated }: NewWorkspaceDialogProps) {
  const t = useT()
  const form = useId()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const close = () => {
    setError(null)
    onClose()
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || name.trim() === '') return
    setBusy(true)
    setError(null)
    try {
      const { workspace, ...me } = await api<Created>('/api/workspaces', {
        method: 'POST',
        headers: forAccount(user),
        body: { name: name.trim() },
      })
      setName('')
      onClose()
      onCreated(me, workspace)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title={t('shell.newWorkspace')}
      size="sm"
      actions={
        <>
          <Button onClick={close}>{t('common.cancel')}</Button>
          <Button type="submit" form={form} variant="primary" loading={busy} disabled={!busy && name.trim() === ''}>
            {t('shell.createWorkspace')}
          </Button>
        </>
      }
    >
      <form id={form} onSubmit={submit} className="ui-new-workspace-form">
        <Field label={t('shell.newWorkspaceName')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={MAX_NAME} data-autofocus />
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}
