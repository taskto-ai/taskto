import type { ReactNode } from 'react'
import './VisuallyHidden.css'

export function VisuallyHidden({ children }: { children: ReactNode }) {
  return <span className="ui-visually-hidden">{children}</span>
}
