// What a page shows in its place when it threw while drawing (the shell's error boundary): what
// happened, that the rest still works, and Reload.

import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import './PageFailed.css'

export function PageFailed() {
  const t = useT()
  return (
    <div className="ui-page-failed" role="alert">
      <EmptyState
        icon={<Icon name="alert" size={24} />}
        title={t('shell.pageFailedTitle')}
        actions={
          <Button variant="primary" onClick={() => window.location.reload()}>
            {t('shell.reload')}
          </Button>
        }
      >
        <p>{t('shell.pageFailedHelp')}</p>
      </EmptyState>
    </div>
  )
}
