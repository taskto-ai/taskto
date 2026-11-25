import { useT } from '../i18n'

// The board's filters: a label among the board's, and only the overdue cards.
export function BoardFilters({
  labels,
  label,
  onLabel,
  overdueOnly,
  onOverdueOnly,
}: {
  labels: string[]
  label: string
  onLabel: (label: string) => void
  overdueOnly: boolean
  onOverdueOnly: (overdueOnly: boolean) => void
}) {
  const t = useT()
  return (
    <fieldset className="tt-board-filters" aria-label={t('boards.filters')}>
      {labels.length > 0 && (
        <label className="tt-inline-field">
          {t('boards.filterLabel')}
          <select value={label} onChange={(e) => onLabel(e.target.value)}>
            <option value="">{t('boards.allLabels')}</option>
            {labels.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
        </label>
      )}
      <label className="tt-inline-field">
        <input type="checkbox" checked={overdueOnly} onChange={(e) => onOverdueOnly(e.target.checked)} />
        {t('boards.overdueOnly')}
      </label>
    </fieldset>
  )
}
