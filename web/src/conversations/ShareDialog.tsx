import { type FormEvent, useState } from 'react'
import { api, type ConversationMembers, errorText, type Member, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Avatar } from '../ui/Avatar'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Notice } from '../ui/Notice'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import './Conversations.css'

export function ShareDialog({
  workspace,
  url,
  owner,
  open,
  onClose,
}: {
  workspace: number
  url: string
  owner: number
  open: boolean
  onClose: () => void
}) {
  const t = useT()
  const shared = useLoad(() => api<ConversationMembers>(`${url}/members`).then((r) => r.members ?? []), [url])
  const everyone = useLoad(
    () => api<{ members: Member[] }>(`${ws(workspace)}/members`).then((r) => r.members),
    [workspace],
  )
  const [chosen, setChosen] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const change = async (run: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await run()
      shared.reload()
      return true
    } catch (e) {
      setError(errorText(e))
      return false
    } finally {
      setBusy(false)
    }
  }
  const add = async (e: FormEvent) => {
    e.preventDefault()
    if (!chosen) return
    if (await change(() => api(`${url}/members/${chosen}`, { method: 'PUT', body: { role: 'viewer' } }))) setChosen('')
  }
  const remove = (user: number) => change(() => api(`${url}/members/${user}`, { method: 'DELETE' }))

  const people = shared.data ?? []
  const candidates = (everyone.data ?? []).filter(
    (m) => m.id !== owner && !m.disabled && !people.some((p) => p.user.id === m.id),
  )
  const loadError = shared.error ?? everyone.error

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t('conversations.share.title')}
      description={t('conversations.share.help')}
      actions={
        <Button variant="primary" onClick={onClose}>
          {t('conversations.share.done')}
        </Button>
      }
    >
      <div className="ui-chat-share">
        {(error ?? loadError) && <Notice tone="danger">{error ?? loadError}</Notice>}
        <form className="ui-chat-share-add" onSubmit={add}>
          <Field label={t('conversations.share.person')}>
            <Select
              value={chosen}
              onChange={(e) => setChosen(e.target.value)}
              disabled={busy || candidates.length === 0}
            >
              <option value="">{t('conversations.share.choose')}</option>
              {candidates.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </Select>
          </Field>
          <Button type="submit" variant="primary" loading={busy} disabled={!chosen}>
            {t('conversations.share.add')}
          </Button>
        </form>
        {everyone.data && candidates.length === 0 && (
          <p className="ui-chat-share-note">{t('conversations.share.noOthers')}</p>
        )}
        <h3 className="ui-chat-share-heading">{t('conversations.share.people')}</h3>
        {shared.data && people.length === 0 && <p className="ui-chat-share-note">{t('conversations.share.none')}</p>}
        {people.length > 0 && (
          <Rows label={t('conversations.share.people')}>
            {people.map((p) => (
              <Row
                key={p.user.id}
                leading={<Avatar name={p.user.name} size="sm" decorative />}
                title={p.user.name}
                actions={
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={busy}
                    aria-label={t('conversations.share.remove', { name: p.user.name })}
                    onClick={() => remove(p.user.id)}
                  >
                    {t('conversations.share.removeLabel')}
                  </Button>
                }
              />
            ))}
          </Rows>
        )}
      </div>
    </Dialog>
  )
}
