import { errorText } from '../api'
import { useT } from '../i18n'
import { isConflict, isNamedConflict } from './docs'

// A change refused because the document changed meanwhile: say so, and offer the reload.
export function Problem({ error, onReload }: { error: unknown; onReload: () => void }) {
  const t = useT()
  if (!isConflict(error)) {
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
