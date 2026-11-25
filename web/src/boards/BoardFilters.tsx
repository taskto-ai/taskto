import { useT } from '../i18n'
import { Checkbox } from '../ui/Checkbox'
import { Label } from '../ui/Label'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator } from '../ui/Menu'
import { Toolbar } from '../ui/Toolbar'

// The board's filters: a label among the board's (a menu), and only the overdue cards.
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
  const name = t('boards.filterLabel')
  return (
    <Toolbar label={t('boards.filters')} className="ui-board-filters">
      {labels.length > 0 && (
        <Menu align="start">
          <MenuButton label={name}>{label || t('boards.allLabels')}</MenuButton>
          <MenuList>
            <MenuItem icon={label === '' ? 'check' : undefined} onSelect={() => onLabel('')}>
              {t('boards.allLabels')}
            </MenuItem>
            <MenuSeparator />
            {labels.map((l) => (
              <MenuItem key={l} icon={l === label ? 'check' : undefined} onSelect={() => onLabel(l)}>
                <Label name={l} />
              </MenuItem>
            ))}
          </MenuList>
        </Menu>
      )}
      <Checkbox
        label={t('boards.overdueOnly')}
        checked={overdueOnly}
        onChange={(e) => onOverdueOnly(e.target.checked)}
      />
    </Toolbar>
  )
}
