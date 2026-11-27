import type { HTMLAttributes, ReactNode, Ref } from 'react'
import './Heading.css'

export type HeadingSize = 'display' | 'title' | 'section' | 'subsection'

export interface HeadingProps extends HTMLAttributes<HTMLHeadingElement> {
  level: 1 | 2 | 3 | 4 | 5 | 6
  size?: HeadingSize
  ref?: Ref<HTMLHeadingElement>
  children: ReactNode
}

export function Heading({ level, size = 'section', className, children, ...rest }: HeadingProps) {
  const Tag = `h${level}` as const
  return (
    <Tag {...rest} className={['ui-heading', className].filter(Boolean).join(' ')} data-size={size}>
      {children}
    </Tag>
  )
}
