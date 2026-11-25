// The top of a page: an optional path above (where this page is), the title in the display face,
// an optional line of help under it, and the page's actions at the end. On a narrow screen the
// actions wrap under the title.

import type { ReactNode } from 'react'
import './PageHeader.css'

export interface PageHeaderProps {
  title: ReactNode
  // A line that says what the page is for.
  help?: ReactNode
  // Buttons, a menu: the page's actions. At most one primary button on a page.
  actions?: ReactNode
  // Where the page is, above the title: a space and a board, a folder.
  path?: ReactNode
  // The title's heading level: 1 for a page, 2 where the page is inside another.
  level?: 1 | 2
}

export function PageHeader({ title, help, actions, path, level = 1 }: PageHeaderProps) {
  const Heading = level === 1 ? 'h1' : 'h2'
  return (
    <header className="ui-page-header">
      <div className="ui-page-header-titles">
        {path && <div className="ui-page-header-path">{path}</div>}
        <Heading className="ui-page-header-title">{title}</Heading>
        {help && <p className="ui-page-header-help">{help}</p>}
      </div>
      {actions && <div className="ui-page-header-actions">{actions}</div>}
    </header>
  )
}
