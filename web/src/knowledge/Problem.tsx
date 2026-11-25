import { errorText } from '../api'
import { useT } from '../i18n'
import { isConflict, isNamedConflict } from './docs'

// A change refused because the document changed meanwhile: say so, and offer the reload.
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
    return (
      <p role="alert" className="tt-error">
        {errorText(error)}
      </p>
    )
  }
  return (
    <div role="alert" className="tt-conflict">
      <span>{isNamedConflict(error) ? errorText(error) : t('knowledge.conflict')}</span>
      <button type="button" className="tt-button" onClick={onReload}>
        {t('knowledge.reload')}
      </button>
    </div>
  )
}
