import { type FormEvent, useState } from 'react'
import { api, codeText, type DictationAddon, type DictationStatus, dictationPath } from '../api'
import { type Key, t as translate, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Alert, FormActions, Loading, Section, SettingsTab, useAction } from './parts'

const LOCAL_MODELS: Record<string, Key> = {
  'parakeet-v3': 'settings.dictation.models.parakeet',
  whistle: 'settings.dictation.models.whistle',
}

const localModel = (model: string) => (LOCAL_MODELS[model] ? translate(LOCAL_MODELS[model]) : model)

export function engineName(status: DictationStatus, engine: string): string {
  if (engine === 'api') return translate('settings.dictation.engines.api')
  if (engine === 'local') {
    const model = status.local?.model
    return model
      ? translate('settings.dictation.engines.localModel', { model: localModel(model) })
      : translate('settings.dictation.engines.local')
  }
  const addon = status.addons.find((a) => a.id === engine)
  return addon ? translate('settings.dictation.engines.addon', { name: addon.name, vendor: addon.vendor }) : engine
}

export function DictationTab({ workspace, admin }: { workspace: number; admin: boolean }) {
  const status = useLoad(() => api<DictationStatus>(dictationPath(workspace)), [workspace])
  const t = useT()
  return (
    <SettingsTab
      title={t('settings.tabs.dictation')}
      help={t.rich('settings.dictation.intro', { file: <code>{'glossary.md'}</code> })}
    >
      <Alert error={status.error} />
      {!status.data && !status.error && <Loading />}
      {status.data && <Current status={status.data} workspace={workspace} admin={admin} onChange={status.reload} />}
      {admin && status.data && (
        <DictationForm
          key={status.data.settings.updated_at ?? 'defaults'}
          workspace={workspace}
          status={status.data}
          onSaved={status.reload}
        />
      )}
    </SettingsTab>
  )
}

const LOCAL_STATES: Record<string, Key> = {
  not_downloaded: 'settings.dictation.states.not_downloaded',
  ready: 'settings.dictation.states.ready',
  failed: 'settings.dictation.states.failed',
  unavailable: 'settings.dictation.states.unavailable',
}

function Current({
  status,
  workspace,
  admin,
  onChange,
}: {
  status: DictationStatus
  workspace: number
  admin: boolean
  onChange: () => void
}) {
  const action = useAction()
  const t = useT()
  const f = useFormat()
  const local = status.local
  const state = !local
    ? ''
    : local.state === 'downloading'
      ? t('settings.dictation.states.downloading', {
          percent: f.number(Math.round((local.progress ?? 0) * 100) / 100, { style: 'percent' }),
        })
      : LOCAL_STATES[local.state]
        ? t(LOCAL_STATES[local.state])
        : local.state
  const showDownload =
    admin && status.engine === 'local' && local && (local.state === 'not_downloaded' || local.state === 'failed')
  return (
    <Panel padded className="settings-panel-body">
      <header className="settings-section-head">
        <div className="settings-section-titles">
          <p className="settings-text">
            {status.engine
              ? t('settings.dictation.uses', { engine: engineName(status, status.engine) })
              : t('settings.dictation.off')}
          </p>
          {status.reason && (
            <p className="settings-muted">{codeText(status.reason_code, status.reason_params, status.reason)}</p>
          )}
        </div>
        {status.engine === 'local' && local?.state === 'unavailable' ? (
          <Badge tone="neutral">{t('settings.dictation.unavailableBadge')}</Badge>
        ) : status.engine === 'local' && local && local.state !== 'ready' ? (
          <Badge tone="warning">{t('settings.dictation.notReadyBadge')}</Badge>
        ) : status.engine ? (
          <Badge tone="success" dot>
            {t('settings.dictation.onBadge')}
          </Badge>
        ) : (
          <Badge tone="neutral">{t('settings.dictation.offBadge')}</Badge>
        )}
      </header>
      {local && (
        <div className="settings-actions">
          <span className="settings-muted">
            {t('settings.dictation.localLine', { model: localModel(local.model), state })}
            {local.error ? ` ${codeText(local.error_code, local.error_params, local.error)}` : ''}
          </span>
          {showDownload && (
            <Button
              size="sm"
              icon="down"
              loading={action.busy}
              onClick={() =>
                action.run(() => api(`${dictationPath(workspace)}/warm`, { method: 'POST', body: {} })).then(onChange)
              }
            >
              {t('settings.dictation.download')}
            </Button>
          )}
          {local.state === 'downloading' && (
            <Button size="sm" variant="ghost" onClick={onChange}>
              {t('settings.dictation.refresh')}
            </Button>
          )}
        </div>
      )}
      <Alert error={action.error} />
    </Panel>
  )
}

function DictationForm({
  workspace,
  status,
  onSaved,
}: {
  workspace: number
  status: DictationStatus
  onSaved: () => void
}) {
  const s = status.settings
  const [engine, setEngine] = useState(s.engine)
  const [language, setLanguage] = useState(s.language)
  const [baseURL, setBaseURL] = useState(s.api.base_url)
  const [model, setModel] = useState(s.api.model)
  const [key, setKey] = useState('')
  const [accepted, setAccepted] = useState(false)
  const action = useAction()
  const t = useT()
  const installed = status.addons.filter((a) => a.installed)
  const addon: DictationAddon | undefined = status.addons.find((a) => a.id === engine)
  const needsNotice = addon !== undefined && s.accepted_notice !== addon.id

  const save = (body: object) =>
    action
      .run(() => api(dictationPath(workspace), { method: 'PATCH', body }))
      .then((ok) => {
        if (ok) {
          setKey('')
          onSaved()
        }
      })
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const apiPart: Record<string, string> = {}
    if (baseURL.trim() !== s.api.base_url) apiPart.base_url = baseURL.trim()
    if (model.trim() !== s.api.model) apiPart.model = model.trim()
    if (key.trim()) apiPart.api_key = key.trim()
    save({
      engine,
      language: language.trim(),
      ...(Object.keys(apiPart).length ? { api: apiPart } : {}),
      ...(needsNotice ? { accept_notice: addon.id } : {}),
    })
  }

  return (
    <Section title={t('settings.dictation.settings')}>
      <Panel padded>
        <form className="settings-form" onSubmit={submit} aria-label={t('settings.dictation.settings')}>
          <Field label={t('settings.dictation.engine')}>
            <Select
              value={engine}
              onChange={(e) => {
                setEngine(e.target.value)
                setAccepted(false)
              }}
            >
              <option value="local" disabled={!status.local && s.engine !== 'local'}>
                {status.local ? t('settings.dictation.localOption') : t('settings.dictation.localOffOption')}
              </option>
              <option value="api">{t('settings.dictation.apiOption')}</option>
              {installed.map((a) => (
                <option key={a.id} value={a.id}>
                  {t('settings.dictation.engines.addon', { name: a.name, vendor: a.vendor })}
                </option>
              ))}
            </Select>
          </Field>
          {addon && (
            <div role="note" aria-label={t('settings.dictation.notice', { name: addon.name })}>
              <Notice tone="warning" title={t('settings.dictation.notice', { name: addon.name })}>
                <p className="settings-text">{addon.notice}</p>
                <p className="settings-muted">
                  {t.rich('settings.dictation.licence', {
                    name: addon.licence_name,
                    link: (text) => (
                      <a href={addon.licence_url} target="_blank" rel="noreferrer noopener">
                        {text}
                      </a>
                    ),
                  })}
                </p>
                {needsNotice && (
                  <Checkbox
                    label={t('settings.dictation.accept', { name: addon.name })}
                    checked={accepted}
                    onChange={(e) => setAccepted(e.target.checked)}
                  />
                )}
              </Notice>
            </div>
          )}
          <Field label={t('settings.dictation.language')} hint={t('settings.dictation.languageHint')}>
            <Input
              value={language}
              onChange={(e) => setLanguage(e.target.value)}
              placeholder={t('settings.dictation.languagePlaceholder')}
              maxLength={3}
            />
          </Field>
          <fieldset className="settings-fieldset">
            <legend>{t('settings.dictation.hosted')}</legend>
            <p className="settings-muted">{t('settings.dictation.hostedNote')}</p>
            <Field label={t('settings.actions.baseUrl')}>
              <Input value={baseURL} onChange={(e) => setBaseURL(e.target.value)} />
            </Field>
            <Field label={t('settings.actions.model')}>
              <Input value={model} onChange={(e) => setModel(e.target.value)} />
            </Field>
            <Field label={t('settings.actions.apiKey')}>
              <Input
                type="password"
                autoComplete="off"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                placeholder={
                  s.api.api_key === 'set'
                    ? t('settings.dictation.keySetPlaceholder')
                    : t('settings.dictation.keyNotSetPlaceholder')
                }
              />
            </Field>
            {s.api.api_key === 'set' && (
              <FormActions>
                <Button variant="ghost" disabled={action.busy} onClick={() => save({ api: { api_key: null } })}>
                  {t('settings.dictation.removeKey')}
                </Button>
              </FormActions>
            )}
          </fieldset>
          <Alert error={action.error} />
          <FormActions>
            <Button type="submit" variant="primary" loading={action.busy} disabled={needsNotice && !accepted}>
              {t('settings.actions.save')}
            </Button>
          </FormActions>
        </form>
      </Panel>
    </Section>
  )
}
