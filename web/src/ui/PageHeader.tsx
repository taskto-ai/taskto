import type { ReactNode, Ref } from 'react'
import './PageHeader.css'

export interface PageHeaderProps {
  title: ReactNode
  help?: ReactNode
  actions?: ReactNode
  path?: ReactNode
  level?: 1 | 2
  titleRef?: Ref<HTMLHeadingElement>
  titleAction?: ReactNode
}

export function PageHeader({ title, help, actions, path, level = 1, titleRef, titleAction }: PageHeaderProps) {
  const Heading = level === 1 ? 'h1' : 'h2'
  return (
    <header className="ui-page-header">
      <div className="ui-page-header-titles">
        {path && <div className="ui-page-header-path">{path}</div>}
        {titleAction ? (
          <div className="ui-page-header-title-row">
            <Heading ref={titleRef} tabIndex={titleRef ? -1 : undefined} className="ui-page-header-title">
              {title}
            </Heading>
            {titleAction}
          </div>
        ) : (
          <Heading ref={titleRef} tabIndex={titleRef ? -1 : undefined} className="ui-page-header-title">
            {title}
          </Heading>
        )}
        {help && <p className="ui-page-header-help">{help}</p>}
      </div>
      {actions && <div className="ui-page-header-actions">{actions}</div>}
    </header>
  )
}
