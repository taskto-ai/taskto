// Your data: a zip of what you own in this workspace, to keep (docs/api.md#export-my-data). The
// button asks the server to start an export (a POST, which uses one of your few exports of the
// hour), and the server answers with an address that works once, for a minute, for this login; the
// page then goes to it, and the browser saves the zip as it arrives, however large. A server that
// is busy, or an hour's exports used up, answers the POST, so the reason is shown here. An admin has
// a second one, for the workspace's shared spaces they are a member of (docs/api.md#export-a-workspace).

import { api, exportPath, workspaceExportPath } from '../api'
import { useT } from '../i18n'
import { DeleteAccount } from './DeleteAccount'
import { Alert, useAction } from './parts'

export function DataTab({ workspace, admin = false }: { workspace: number; admin?: boolean }) {
  const t = useT()
  const action = useAction()
  const wsAction = useAction()
  const startAt = (path: string) => async () => {
    const { download } = await api<{ download: string }>(path, { method: 'POST', body: {} })
    window.location.assign(download)
  }
  const start = () => action.run(startAt(exportPath(workspace)))
  const startWorkspace = () => wsAction.run(startAt(workspaceExportPath(workspace)))
  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-data">
        <h2 id="tt-data">{t('settings.data.title')}</h2>
        <p className="tt-muted tt-small">{t('settings.data.intro')}</p>
        <ul>
          <li>{t('settings.data.documents')}</li>
          <li>{t('settings.data.boards')}</li>
          <li>{t('settings.data.conversations')}</li>
          <li>{t('settings.data.apps')}</li>
        </ul>
        <p className="tt-muted tt-small">{t('settings.data.scope')}</p>
        <p className="tt-muted tt-small">{t('settings.data.never')}</p>
        <Alert error={action.error} />
        <p>
          <button type="button" className="tt-button tt-primary" onClick={start} disabled={action.busy}>
            {t('settings.data.download')}
          </button>
        </p>
        <p className="tt-hint">{t('settings.data.limit')}</p>
      </section>
      <DeleteAccount workspace={workspace} />
      {admin && (
        <section className="tt-panel" aria-labelledby="tt-data-workspace">
          <h2 id="tt-data-workspace">{t('settings.data.workspace.title')}</h2>
          <p className="tt-muted tt-small">{t('settings.data.workspace.intro')}</p>
          <p className="tt-muted tt-small">{t('settings.data.workspace.scope')}</p>
          <Alert error={wsAction.error} />
          <p>
            <button type="button" className="tt-button tt-primary" onClick={startWorkspace} disabled={wsAction.busy}>
              {t('settings.data.workspace.download')}
            </button>
          </p>
          <p className="tt-hint">{t('settings.data.workspace.limit')}</p>
        </section>
      )}
    </div>
  )
}
