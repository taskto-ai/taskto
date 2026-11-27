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
