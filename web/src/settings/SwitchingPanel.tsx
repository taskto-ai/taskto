// Automatic switching (docs/account-switching.md): the user's ordered queue of accounts, their
// own and their workspaces' shared ones. While it is on, a turn whose account stops (a usage
// limit, an expired login) goes on with the next one in the queue that can run. It is off until
// the user turns it on, next to a warning and the providers' terms: plans assume individual use.

import { api, CLI_PRODUCTS, CLI_SWITCHING, type CliName, type CliSwitching, type QueueAccount } from '../api'
import { type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Switch } from '../ui/Switch'
import { CLI_TERMS } from './CliAccountsTab'
import { Alert, Loading, Section, useAction } from './parts'
import { SHARED_TERMS } from './SharedCliAccounts'

const entry = (a: QueueAccount) => (a.kind === 'own' ? { account: a.id } : { shared: Number(a.id) })
const key = (a: QueueAccount) => `${a.kind}:${a.id}`

// An account's name in the user's language: the CLI, with its label, its number or its
// workspace. The server's English title is the fallback.
function queueTitle(t: T, a: QueueAccount): string {
  const name = CLI_PRODUCTS[a.cli] ?? a.cli
  if (a.kind === 'shared') {
    return a.label && a.workspace_name
      ? t('settings.switching.shared', { name, label: a.label, workspace: a.workspace_name })
      : a.title
  }
  if (a.label) return t('settings.switching.ownLabelled', { name, label: a.label })
  if (a.number === undefined) return a.title
  return a.number > 1 ? t('settings.switching.ownNumbered', { name, number: a.number }) : name
}

export function SwitchingPanel() {
  const state = useLoad(() => api<CliSwitching>(CLI_SWITCHING), [])
  const action = useAction()
  const t = useT()
  const f = useFormat()
  const save = (body: { enabled?: boolean; queue?: QueueAccount[] }) =>
    action
      .run(() =>
        api(CLI_SWITCHING, {
          method: 'PUT',
          body: { ...body, ...(body.queue ? { queue: body.queue.map(entry) } : {}) },
        }),
      )
      // Read again whatever happened: a refused change (an account gone since) shows the
      // queue the server has.
      .then(() => state.reload())
  if (!state.data) {
    return (
      <Section title={t('settings.switching.title')}>
        <Alert error={state.error} />
        {!state.error && <Loading lines={2} />}
      </Section>
    )
  }
  const { enabled, queue, available } = state.data
  const queued = new Set(queue.map(key))
  const rest = available.filter((a) => !queued.has(key(a)))
  const move = (i: number, by: number) => {
    const next = [...queue]
    const [a] = next.splice(i, 1)
    next.splice(i + by, 0, a)
    save({ queue: next })
  }
  const clis = [...new Set(available.map((a) => a.cli))] as CliName[]
  return (
    <Section title={t('settings.switching.title')} help={t('settings.switching.intro')}>
      <Panel padded className="settings-panel-body">
        <Switch
          label={t('settings.switching.toggle')}
          checked={enabled}
          disabled={action.busy}
          onChange={(e) => save({ enabled: e.target.checked })}
        />
        <Notice tone="warning">
          {t('settings.switching.warning')}
          {clis.length > 0 && (
            <ul className="settings-terms" aria-label={t('settings.switching.termsLabel')}>
              {clis.map((cli) => (
                <li key={cli}>
                  <a href={CLI_TERMS[cli]} target="_blank" rel="noopener noreferrer">
                    {t('settings.actions.termsOf', { name: CLI_PRODUCTS[cli] })}
                  </a>
                  {available.some((a) => a.cli === cli && a.kind === 'shared') &&
                    SHARED_TERMS[cli].map((term) => (
                      <span key={term.href}>
                        {' · '}
                        <a href={term.href} target="_blank" rel="noopener noreferrer">
                          {t(term.label)}
                        </a>
                      </span>
                    ))}
                </li>
              ))}
            </ul>
          )}
        </Notice>
      </Panel>
      <Alert error={action.error} />
      {queue.length === 0 ? (
        <p className="settings-muted">{t('settings.switching.empty')}</p>
      ) : (
        <Panel>
          <ol className="settings-queue" aria-label={t('settings.switching.queue')}>
            {queue.map((a, i) => {
              const title = queueTitle(t, a)
              return (
                <li key={key(a)} className="settings-queue-item" aria-label={title}>
                  <span className="settings-person-text">
                    <span className="settings-person-name">
                      {t('settings.switching.position', { position: i + 1, name: title })}
                      {a.limited && <Badge tone="warning">{t('settings.switching.limitedBadge')}</Badge>}
                    </span>
                    {a.limited && (
                      <span className="settings-muted">
                        {a.limited_until
                          ? t('settings.switching.atLimitUntil', { time: f.dateTime(a.limited_until) })
                          : t('settings.switching.atLimit')}
                      </span>
                    )}
                  </span>
                  <span className="settings-actions">
                    <IconButton
                      size="sm"
                      icon="up"
                      label={t('settings.switching.moveUp', { name: title })}
                      disabled={action.busy || i === 0}
                      onClick={() => move(i, -1)}
                    />
                    <IconButton
                      size="sm"
                      icon="down"
                      label={t('settings.switching.moveDown', { name: title })}
                      disabled={action.busy || i === queue.length - 1}
                      onClick={() => move(i, 1)}
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t('settings.switching.removeNamed', { name: title })}
                      disabled={action.busy}
                      onClick={() => save({ queue: queue.filter((_, j) => j !== i) })}
                    >
                      {t('settings.actions.remove')}
                    </Button>
                  </span>
                </li>
              )
            })}
          </ol>
        </Panel>
      )}
      {rest.length > 0 && (
        <Field label={t('settings.switching.addTo')}>
          <Select
            value=""
            disabled={action.busy}
            onChange={(e) => {
              const a = rest.find((r) => key(r) === e.target.value)
              if (a) save({ queue: [...queue, a] })
            }}
          >
            <option value="">{t('settings.switching.choose')}</option>
            {rest.map((a) => (
              <option key={key(a)} value={key(a)}>
                {queueTitle(t, a)}
              </option>
            ))}
          </Select>
        </Field>
      )}
    </Section>
  )
}
