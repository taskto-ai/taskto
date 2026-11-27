import { useId } from 'react'
import { api, type HomeView, ws } from '../api'
import { appPath } from '../apps/route'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { NavLink } from '../shell/NavLink'
import { Avatar, initialsAmong } from '../ui/Avatar'
import { useHomeVersion } from './store'

export function PinnedNav({ workspace, path }: { workspace: number; path: string }) {
  const version = useHomeVersion()
  const home = useLoad(() => api<HomeView>(`${ws(workspace)}/home`), [workspace, version])
  const t = useT()
  const title = useId()
  const pins = (home.data?.pins ?? []).filter((p) => p.state === 'ready')
  if (pins.length === 0) return null
  const labels = pins.map((p) => p.label || p.app_name)
  const letters = initialsAmong(labels)
  return (
    <section className="ui-shell-pins" aria-labelledby={title}>
      <h2 id={title} className="ui-shell-group-title">
        {t('shell.pinned')}
      </h2>
      <ul className="ui-shell-pin-list">
        {pins.map((p, i) => {
          const href = appPath(workspace, p.space_id, p.app_id, p.screen)
          return (
            <li key={`${p.source}:${p.id ?? p.default_id}`}>
              <NavLink
                href={href}
                current={href === path}
                leading={<Avatar name={labels[i]} letters={letters[i]} size="sm" shape="square" decorative />}
                label={labels[i]}
                userText
              />
            </li>
          )
        })}
      </ul>
    </section>
  )
}
