import { createContext, type HTMLAttributes, type ReactNode, useContext } from 'react'
import './Panel.css'

const Nested = createContext(false)

export interface PanelProps extends HTMLAttributes<HTMLElement> {
  as?: 'div' | 'section'
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
