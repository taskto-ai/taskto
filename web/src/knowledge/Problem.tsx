import { errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { isConflict, isNamedConflict } from './docs'

// A change refused: say why. One refused because the document changed meanwhile says so, and
// offers the reload.
// reloadable: this error is one for which the reload is offered too (what was asked for is gone).
export function Problem({
  error,
  onReload,
  reloadable = false,
}: {
  error: unknown
  onReload: () => void
  reloadable?: boolean
}) {
  const t = useT()
  if (!isConflict(error) && !reloadable) {
    return <Notice tone="danger">{errorText(error)}</Notice>
  }
  return (
    <Notice
      tone="danger"
      action={
        <Button size="sm" onClick={onReload}>
          {t('knowledge.reload')}
        </Button>
      }
    >
      {isNamedConflict(error) ? errorText(error) : t('knowledge.conflict')}
    </Notice>
  )
}
