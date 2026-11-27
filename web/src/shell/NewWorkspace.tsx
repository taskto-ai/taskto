import { type FormEvent, useEffect, useId, useRef, useState } from 'react'
import { api, errorText, forAccount, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { useMounted } from '../mounted'
import { navigateWholeApp, routePath } from '../router'
import { sameSession } from '../session'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { toast } from '../ui/Toast'

type Created = Me & { workspace: Workspace }

const MAX_NAME = 100

export interface NewWorkspaceDialogProps {
  open: boolean
  onClose: () => void
  user: number
  onMe: (me: Me) => void
}

const openWorkspace = (id: number) => navigateWholeApp(routePath({ workspace: id, section: 'conversations' }))

export function NewWorkspaceDialog({ open, onClose, user, onMe }: NewWorkspaceDialogProps) {
  const t = useT()
  const form = useId()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const opening = useRef(0)
  const mounted = useMounted()

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
