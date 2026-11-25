// Words for a screen reader that the page doesn't show: a busy button's "Loading", a status said
// by a dot. They stay in the accessibility tree, unlike `hidden` or `display: none`.

import type { ReactNode } from 'react'
import './VisuallyHidden.css'

export function VisuallyHidden({ children }: { children: ReactNode }) {
  return <span className="ui-visually-hidden">{children}</span>
}
