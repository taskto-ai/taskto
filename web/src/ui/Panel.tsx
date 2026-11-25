// A bordered surface that holds a group of things: a list of rows, a table, a form. A panel is
// never inside another: one inside a panel draws no edge of its own (and says so in development).

import { createContext, type HTMLAttributes, type ReactNode, useContext } from 'react'
import './Panel.css'

const Nested = createContext(false)

export interface PanelProps extends HTMLAttributes<HTMLElement> {
  // `section` with a heading inside and a label, `div` otherwise.
  as?: 'div' | 'section'
  // Space inside, for a form or text; a panel of rows or a table has none.
  padded?: boolean
  children: ReactNode
}

export function Panel({ as: Tag = 'div', padded = false, className, children, ...rest }: PanelProps) {
  const nested = useContext(Nested)
  if (nested && import.meta.env.DEV) console.error('A Panel is inside another Panel: it draws no edge of its own.')
  return (
    <Tag
      {...rest}
      className={['ui-panel', className].filter(Boolean).join(' ')}
      data-padded={padded || undefined}
      data-nested={nested || undefined}
    >
      <Nested.Provider value={true}>{children}</Nested.Provider>
    </Tag>
  )
}
