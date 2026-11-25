// Your data: a zip of what you own in this workspace, to keep (docs/api.md#export-my-data). The
// button asks the server to start an export (a POST, which uses one of your few exports of the
// hour), and the server answers with an address that works once, for a minute, for this login; the
// page then goes to it, and the browser saves the zip as it arrives, however large. A server that
// is busy, or an hour's exports used up, answers the POST, so the reason is shown here. An admin has
// a second one, for the workspace's shared spaces they are a member of (docs/api.md#export-a-workspace).

import { api, exportPath, workspaceExportPath } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { DeleteAccount } from './DeleteAccount'
import { Alert, FormActions, Section, SettingsTab, useAction } from './parts'

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
    <SettingsTab title={t('settings.tabs.data')} help={t('settings.help.data')}>
      <Section title={t('settings.data.title')} help={t('settings.data.intro')}>
        <Panel padded className="settings-panel-body">
          <ul className="settings-list">
            <li>{t('settings.data.documents')}</li>
            <li>{t('settings.data.boards')}</li>
            <li>{t('settings.data.conversations')}</li>
            <li>{t('settings.data.apps')}</li>
          </ul>
          <p className="settings-muted">{t('settings.data.scope')}</p>
          <p className="settings-muted">{t('settings.data.never')}</p>
          <Alert error={action.error} />
          <FormActions>
            <Button variant="primary" icon="down" onClick={start} loading={action.busy}>
              {t('settings.data.download')}
            </Button>
          </FormActions>
          <p className="settings-muted">{t('settings.data.limit')}</p>
        </Panel>
      </Section>
      <DeleteAccount workspace={workspace} />
      {admin && (
        <Section title={t('settings.data.workspace.title')} help={t('settings.data.workspace.intro')}>
          <Panel padded className="settings-panel-body">
            <Notice tone="info">{t('settings.data.workspace.scope')}</Notice>
            <Alert error={wsAction.error} />
            <FormActions>
              <Button icon="down" onClick={startWorkspace} loading={wsAction.busy}>
                {t('settings.data.workspace.download')}
              </Button>
            </FormActions>
            <p className="settings-muted">{t('settings.data.workspace.limit')}</p>
          </Panel>
        </Section>
      )}
    </SettingsTab>
  )
}
