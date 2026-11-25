// The apps a person pinned (and those a shared space pins for its members), listed in the
// navigation in a group of their own, after the platform's pages: a pin is only ever one more link
// to launch an app, whatever it says, so it can't stand in for or hide a platform page. Its label is
// drawn as plain text, beside a small avatar of it. Unavailable pins (the app is offline or was
// never published) aren't listed.
//
// Loading and drawing them can't take the navigation down: a failed load shows nothing, and the
// shell wraps this in an error boundary for an answer that is malformed.

import { useId } from 'react'
import { api, type HomeView, ws } from '../api'
import { appPath } from '../apps/route'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { NavLink } from '../shell/NavLink'
import { Avatar } from '../ui/Avatar'
import { useHomeVersion } from './store'

export function PinnedNav({ workspace, path }: { workspace: number; path: string }) {
  const version = useHomeVersion()
  const home = useLoad(() => api<HomeView>(`${ws(workspace)}/home`), [workspace, version])
  const t = useT()
  const title = useId()
  const pins = (home.data?.pins ?? []).filter((p) => p.state === 'ready')
  if (pins.length === 0) return null
  return (
    <section className="ui-shell-pins" aria-labelledby={title}>
      <h2 id={title} className="ui-shell-group-title">
        {t('shell.pinned')}
      </h2>
      <ul className="ui-shell-pin-list">
        {pins.map((p) => {
          const href = appPath(workspace, p.space_id, p.app_id, p.screen)
          const label = p.label || p.app_name
          return (
            <li key={`${p.source}:${p.id ?? p.default_id}`}>
              <NavLink
                href={href}
                current={href === path}
                leading={<Avatar name={label} size="sm" shape="square" decorative />}
                label={label}
              />
            </li>
          )
        })}
      </ul>
    </section>
  )
}
