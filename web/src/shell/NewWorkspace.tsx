// "New workspace": a small form in a dialog, opened from the workspace switcher's menu, or from the
// screen for someone with no workspace yet. The server decides who may (`can_create_workspace` in
// /api/me, the operator's -workspace-creation); the shell only offers it to those it says yes to.

import { type FormEvent, useId, useState } from 'react'
import { api, errorText, forAccount, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'

// The answer of POST /api/workspaces: what /api/me says now, and the new workspace.
type Created = Me & { workspace: Workspace }

const MAX_NAME = 100

export interface NewWorkspaceDialogProps {
  open: boolean
  onClose: () => void
  // The account the form was opened for; the server refuses the request (409) if the browser's
  // session became another account's since, so a workspace is never made for someone the person
  // didn't mean.
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
      <form id={form} onSubmit={submit} className="ui-shell-new-workspace">
        <Field label={t('shell.newWorkspaceName')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={MAX_NAME} data-autofocus />
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}
