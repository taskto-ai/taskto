import { errorText, type Me } from '../api'
import { type Key, type T, t as translate } from '../i18n'
import { useInstall } from '../pwa/install'
import { navigate, routePath } from '../router'
import { type Theme, useTheme } from '../theme'
import { Avatar } from '../ui/Avatar'
import type { IconName } from '../ui/Icon'
import { Menu, MenuButton, MenuGroup, MenuItem, MenuList, MenuSeparator } from '../ui/Menu'
import { toast } from '../ui/Toast'
import { Tooltip } from '../ui/Tooltip'
import { LanguageItems, useLanguagePick } from './LanguagePicker'

const THEMES = [
  { theme: 'system', icon: 'contrast', label: 'shell.theme.system' },
  { theme: 'light', icon: 'sun', label: 'shell.theme.light' },
  { theme: 'dark', icon: 'moon', label: 'shell.theme.dark' },
] as const satisfies readonly { theme: Theme; icon: IconName; label: Key }[]

export function AccountMenu({ me, workspace, onLogout, t }: { me: Me; workspace: number; onLogout: () => void; t: T }) {
  const { name, email } = me.user
  const { installed } = useInstall()
  const [theme, setTheme] = useTheme()
  const language = useLanguagePick(me.user.locale ?? '', (e) =>
    toast({ message: translate('language.notSaved', { error: errorText(e) }), tone: 'danger' }),
  )

  return (
    <div className="ui-shell-account">
      <Menu align="start">
        <Tooltip content={name} describes={false} side="inline-end">
          <MenuButton label={t('shell.accountLabel')} indicator={null} className="ui-shell-account-button">
            <Avatar name={name} decorative />
            <span className="ui-shell-account-text">
              <span className="ui-shell-account-name" dir="auto">
                {name}
              </span>{' '}
              <span className="ui-shell-account-email" dir="auto">
                {email}
              </span>
            </span>
          </MenuButton>
        </Tooltip>
        <MenuList>
          <MenuGroup label={t('shell.theme.label')}>
            {THEMES.map((o) => (
              <MenuItem key={o.theme} icon={o.icon} checked={theme === o.theme} onSelect={() => setTheme(o.theme)}>
                {t(o.label)}
              </MenuItem>
            ))}
          </MenuGroup>
          <MenuSeparator />
          <LanguageItems language={language} t={t} />
          <MenuSeparator />
          {!installed && (
            <MenuItem onSelect={() => navigate(routePath({ workspace, section: 'settings', item: 'about' }))}>
              {t('shell.installApp')}
            </MenuItem>
          )}
          <MenuItem onSelect={onLogout}>{t('shell.logOut')}</MenuItem>
        </MenuList>
      </Menu>
    </div>
  )
}
