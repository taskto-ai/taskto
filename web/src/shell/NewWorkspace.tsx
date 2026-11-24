// "New workspace" in the top bar, beside the workspace switcher: a small form in the same kind
// of popover as the account menu. The server decides who may (`can_create_workspace` in
// /api/me, the operator's -workspace-creation); the shell only offers it to those it says yes to.

import { type FormEvent, useRef, useState } from 'react'
import { api, errorText, type Me, type Workspace } from '../api'
import { useT } from '../i18n'
import { Icon } from './Icon'

// The answer of POST /api/workspaces: what /api/me says now, and the new workspace.
type Created = Me & { workspace: Workspace }

const MAX_NAME = 100

export function NewWorkspace({ onCreated }: { onCreated: (me: Me, workspace: Workspace) => void }) {
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
