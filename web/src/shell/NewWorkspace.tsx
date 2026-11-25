// "New workspace" in the top bar, beside the workspace switcher: a small form in the same kind
// of popover as the account menu. The server decides who may (`can_create_workspace` in
// /api/me, the operator's -workspace-creation); the shell only offers it to those it says yes to.

import { type FormEvent, useEffect, useId, useRef, useState } from 'react'
import { api, errorText, forAccount, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { navigate, routePath } from '../router'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { toast } from '../ui/Toast'
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

// "New workspace" as a small form in a dialog: the one the shell has, here for the screen of
// someone with no workspace yet, where a popover hanging from a button doesn't fit a phone. Its
// name, props and behaviour are the shell's, so the two are one component.
//
// Made, it gives the account's new state to `onMe` (first, so the shell knows the workspace when the
// address names it) and opens the workspace. A save the person cancelled that still succeeds made a
// workspace all the same: the account is updated and a toast says so, with a way to open it, but
// the page stays where the person is. One that fails after Cancel made nothing, and isn't shown.
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
  // Whether the dialog is still open when a save ends: a save outlives a Cancel.
  const shown = useRef(open)
  shown.current = open

  // Each opening starts clean: no name, and no error left from a save before.
  useEffect(() => {
    if (!open) return
    setName('')
    setError(null)
  }, [open])

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
      onMe(me)
      if (shown.current) {
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
      if (shown.current) setError(errorText(err))
    } finally {
      setBusy(false)
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
      <form id={form} onSubmit={submit} className="ui-new-workspace-form">
        <Field label={t('shell.newWorkspaceName')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={MAX_NAME} data-autofocus />
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}
