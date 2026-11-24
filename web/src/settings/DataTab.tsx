// Your data: a zip of what you own in this workspace, to keep (docs/api.md#export-my-data). It is a
// plain download link, so the browser saves the zip as it arrives, however large; a server that
// is busy, or an hour's exports used up, answers an error the browser shows as a failed download,
// which the text under the link says.

import { exportPath } from '../api'
import { useT } from '../i18n'

export function DataTab({ workspace }: { workspace: number }) {
  const t = useT()
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
        <p className="tt-muted tt-small">{t('settings.data.never')}</p>
        <p>
          <a className="tt-button tt-primary" href={exportPath(workspace)} download>
            {t('settings.data.download')}
          </a>
        </p>
        <p className="tt-hint">{t('settings.data.limit')}</p>
      </section>
    </div>
  )
}
