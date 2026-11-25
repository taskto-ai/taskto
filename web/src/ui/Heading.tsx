// A heading a screen writes itself: a section of a page, a group in a panel, the title of a card.
// Its look comes from the type roles, not from the element: the display face (Bricolage
// Grotesque) only from 20px up, `display` (24px, a page's title; PageHeader draws that one) and
// `title` (20px, a dialog's, a drawer's or a large card's), and Inter below that, `section` (16px,
// the default) and `subsection` (14px, strong). The level is the document's outline, the size is
// how it looks: pick them apart.

import type { HTMLAttributes, ReactNode } from 'react'
import './Heading.css'

export type HeadingSize = 'display' | 'title' | 'section' | 'subsection'

export interface HeadingProps extends HTMLAttributes<HTMLHeadingElement> {
  level: 1 | 2 | 3 | 4 | 5 | 6
  size?: HeadingSize
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
