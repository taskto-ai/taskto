// A checkbox with its label beside it: the browser's own <input type="checkbox">, drawn as an ink
// box with a check (or a dash, when it is mixed), so the keyboard (Space), a screen reader and a
// form all work as they always do. The whole row is the target, at least 44px tall on touch.

import { type ComponentPropsWithRef, type ReactNode, useEffect, useRef } from 'react'
import './Checkbox.css'

export type CheckboxProps = Omit<ComponentPropsWithRef<'input'>, 'type' | 'children'> & {
  label: ReactNode
  // Neither checked nor not: some of what it stands for is (a list's "select all").
  indeterminate?: boolean
}

export function Checkbox({ label, indeterminate = false, className, ref, ...rest }: CheckboxProps) {
  const own = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (own.current) own.current.indeterminate = indeterminate
  }, [indeterminate])
  const setRef = (el: HTMLInputElement | null) => {
    own.current = el
    if (typeof ref === 'function') ref(el)
    else if (ref) ref.current = el
  }
  return (
    <label className={['ui-check', className ?? ''].filter(Boolean).join(' ')}>
      <input ref={setRef} type="checkbox" className="ui-control ui-check-box" {...rest} />
      <span>{label}</span>
    </label>
  )
}
