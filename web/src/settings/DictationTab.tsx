// Dictation: how the chat box's microphone turns speech into text (docs/dictation.md).
// Admins choose the engine: the operator's local model (the default: local first), a hosted
// API with the workspace's key (write-only: the server only says whether one is set; a key
// alone never switches to it), or an add-on the operator installed, whose notice they must
// confirm first. Members see what is in use.

import { type FormEvent, useState } from 'react'
import { api, codeText, type DictationAddon, type DictationStatus, dictationPath } from '../api'
import { type Key, t as translate, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, useAction } from './parts'

const LOCAL_MODELS: Record<string, Key> = {
  'parakeet-v3': 'settings.dictation.models.parakeet',
  whistle: 'settings.dictation.models.whistle',
}

// A local model's name and languages; one the app doesn't know by its ID.
const localModel = (model: string) => (LOCAL_MODELS[model] ? translate(LOCAL_MODELS[model]) : model)

// The engine, as "Dictation uses {engine}." names it.
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
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-dictation">
        <h2 id="tt-dictation">{t('settings.tabs.dictation')}</h2>
        <p className="tt-muted tt-small">
          {t.rich('settings.dictation.intro', { file: <code>{'glossary.md'}</code> })}
        </p>
        <Alert error={status.error} />
        {!status.data && !status.error && <p className="tt-muted">{t('app.loading')}</p>}
        {status.data && <Current status={status.data} workspace={workspace} admin={admin} onChange={status.reload} />}
        {admin && status.data && (
          <DictationForm
            key={status.data.settings.updated_at ?? 'defaults'}
            workspace={workspace}
            status={status.data}
            onSaved={status.reload}
          />
        )}
      </section>
    </div>
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
  return (
    <>
      <p>
        {status.engine
          ? t('settings.dictation.uses', { engine: engineName(status, status.engine) })
          : t('settings.dictation.off')}
      </p>
      {status.reason && (
        <p className="tt-muted tt-small">{codeText(status.reason_code, status.reason_params, status.reason)}</p>
      )}
      {local && (
        <p className="tt-small">
          {t('settings.dictation.localLine', { model: localModel(local.model), state })}
          {local.error ? ` ${codeText(local.error_code, local.error_params, local.error)}` : ''}{' '}
          {admin && status.engine === 'local' && (local.state === 'not_downloaded' || local.state === 'failed') && (
            <button
              type="button"
              className="tt-button"
              disabled={action.busy}
              onClick={() =>
                action.run(() => api(`${dictationPath(workspace)}/warm`, { method: 'POST', body: {} })).then(onChange)
              }
            >
              {t('settings.dictation.download')}
            </button>
          )}
          {local.state === 'downloading' && (
            <button type="button" className="tt-button" onClick={onChange}>
              {t('settings.dictation.refresh')}
            </button>
          )}
        </p>
      )}
      <Alert error={action.error} />
    </>
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
    <form className="tt-form" onSubmit={submit} aria-label={t('settings.dictation.settings')}>
      <label>
        {t('settings.dictation.engine')}
        <select
          value={engine}
          onChange={(e) => {
            setEngine(e.target.value)
            setAccepted(false) // a confirmation is for one add-on's notice
          }}
        >
          {/* Local is the default, so it is listed even where the operator turned it off. */}
          <option value="local" disabled={!status.local && s.engine !== 'local'}>
            {status.local ? t('settings.dictation.localOption') : t('settings.dictation.localOffOption')}
          </option>
          <option value="api">{t('settings.dictation.apiOption')}</option>
          {installed.map((a) => (
            <option key={a.id} value={a.id}>
              {t('settings.dictation.engines.addon', { name: a.name, vendor: a.vendor })}
            </option>
          ))}
        </select>
      </label>
      {addon && (
        <div className="tt-panel" role="note" aria-label={t('settings.dictation.notice', { name: addon.name })}>
          {/* The add-on's own notice, as its vendor wrote it. */}
          <p>{addon.notice}</p>
          <p className="tt-small">
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
            <label className="tt-check">
              <input type="checkbox" checked={accepted} onChange={(e) => setAccepted(e.target.checked)} />{' '}
              {t('settings.dictation.accept', { name: addon.name })}
            </label>
          )}
        </div>
      )}
      <label>
        {t('settings.dictation.language')}
        <input
          value={language}
          onChange={(e) => setLanguage(e.target.value)}
          placeholder={t('settings.dictation.languagePlaceholder')}
          maxLength={3}
          aria-describedby="tt-dictation-language"
        />
      </label>
      <span id="tt-dictation-language" className="tt-muted tt-small">
        {t('settings.dictation.languageHint')}
      </span>
      <fieldset className="tt-fieldset">
        <legend>{t('settings.dictation.hosted')}</legend>
        <label>
          {t('settings.actions.baseUrl')}
          <input value={baseURL} onChange={(e) => setBaseURL(e.target.value)} />
        </label>
        <label>
          {t('settings.actions.model')}
          <input value={model} onChange={(e) => setModel(e.target.value)} />
        </label>
        <label>
          {t('settings.actions.apiKey')}
          <input
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
        </label>
        <p className="tt-muted tt-small">{t('settings.dictation.hostedNote')}</p>
        {s.api.api_key === 'set' && (
          <button
            type="button"
            className="tt-button"
            disabled={action.busy}
            onClick={() => save({ api: { api_key: null } })}
          >
            {t('settings.dictation.removeKey')}
          </button>
        )}
      </fieldset>
      <Alert error={action.error} />
      <button type="submit" className="tt-button tt-primary" disabled={action.busy || (needsNotice && !accepted)}>
        {t('settings.actions.save')}
      </button>
    </form>
  )
}
