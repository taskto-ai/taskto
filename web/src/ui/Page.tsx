// A page's frame: the scroll container, the padding every page has, and the width rule. A page
// of reading or a form is `reading` (760px), a page of lists `list` (1,040px), a board `full`
// (the window's width). The content is centred in what the shell gives it.

import type { HTMLAttributes, ReactNode } from 'react'
import './Page.css'

export type PageWidth = 'reading' | 'list' | 'full'

export interface PageProps extends HTMLAttributes<HTMLDivElement> {
  width?: PageWidth
  children: ReactNode
}

export function Page({ width = 'list', className, children, ...rest }: PageProps) {
  return (
    <div {...rest} className={['ui-page', className].filter(Boolean).join(' ')} data-width={width}>
      <div className="ui-page-inner">{children}</div>
    </div>
  )
}
