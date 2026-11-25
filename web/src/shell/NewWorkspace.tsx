// "New workspace": a small form in a dialog, the one the shell has, opened from the workspace
// switcher's menu and from the screen for someone with no workspace yet. The server decides who may
// (`can_create_workspace` in /api/me, the operator's -workspace-creation); the shell only offers it
// to those it says yes to.
//
// Made, it gives the account's new state to `onMe` (first, so the shell knows the workspace when the
// address names it) and opens the workspace. A save the person cancelled that still succeeds made a
// workspace all the same: the account is updated and a toast says so, with a way to open it, but
// the page stays where the person is. One that fails after Cancel made nothing, and isn't shown.
// One that ends after the session did (logged out, or the session lost) does nothing at all: no
// toast and no change of address on the login screen, or in the next person's session. One that
// ends while the dialog is gone but the session isn't (the shell is showing a push's notification)
// still updates the account and says so, and opens nothing.

import { type FormEvent, useEffect, useId, useRef, useState } from 'react'
import { api, errorText, forAccount, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { useMounted } from '../mounted'
import { navigate, routePath } from '../router'
import { sameSession } from '../session'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { toast } from '../ui/Toast'

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
  // The account's new state, with the workspace made.
  onMe: (me: Me) => void
}

const openWorkspace = (id: number) => navigate(routePath({ workspace: id, section: 'conversations' }))

export function NewWorkspaceDialog({ open, onClose, user, onMe }: NewWorkspaceDialogProps) {
  const t = useT()
  const form = useId()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Which opening of the dialog this is: each opening and each closing counts one more. A save
  // belongs to the opening it was started in, and outlives a Cancel: when it ends, it acts on the
  // dialog only if that same opening is still there (not one opened again since).
  const opening = useRef(0)
  const mounted = useMounted()

  // Each opening starts clean: no name, no error and no save left from one before.
  useEffect(() => {
    opening.current += 1
    if (!open) return
    setName('')
    setError(null)
    setBusy(false)
  }, [open])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || name.trim() === '') return
    const mine = opening.current
    const still = () => opening.current === mine
    const session = sameSession()
    setBusy(true)
    setError(null)
    try {
      const { workspace, ...me } = await api<Created>('/api/workspaces', {
        method: 'POST',
        headers: forAccount(user),
        body: { name: name.trim() },
      })
      if (!session()) return
      onMe(me)
      if (mounted.current && still()) {
        onClose()
        openWorkspace(workspace.id)
      } else {
        toast({
          message: t('shell.workspaceCreated', { name: workspace.name }),
          tone: 'success',
          action: { label: t('shell.openWorkspace'), onClick: () => openWorkspace(workspace.id) },
        })
      }
    } catch (err) {
      if (mounted.current && still()) setError(errorText(err))
    } finally {
      if (mounted.current && still()) setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t('shell.newWorkspace')}
      size="sm"
      actions={
        <>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
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
