// A checkbox with its label beside it: the browser's own <input type="checkbox">, drawn as an ink
// box with a check (or a dash, when it is mixed), so the keyboard (Space), a screen reader and a
// form all work as they always do. The whole row is the target, at least 44px tall on touch.

import {
  type ChangeEvent,
  type ComponentPropsWithRef,
  type ReactNode,
  useCallback,
  useLayoutEffect,
  useRef,
} from 'react'
import './Checkbox.css'

export type CheckboxProps = Omit<ComponentPropsWithRef<'input'>, 'type' | 'children'> & {
  label: ReactNode
  // Neither checked nor not: some of what it stands for is (a list's "select all").
  indeterminate?: boolean
}

export function Checkbox({ label, indeterminate = false, className, ref, onChange, ...rest }: CheckboxProps) {
  const own = useRef<HTMLInputElement>(null)
  // Pressing a checkbox clears the browser's mixed state. It is put back after every render, and
  // right after the change too, since a controlled checkbox whose state doesn't change is not
  // drawn again.
  useLayoutEffect(() => {
    if (own.current) own.current.indeterminate = indeterminate
  })
  const change = (e: ChangeEvent<HTMLInputElement>) => {
    onChange?.(e)
    e.currentTarget.indeterminate = indeterminate
  }
  // The caller's ref too, with what React 19 lets a callback ref return: its own cleanup.
  const setRef = useCallback(
    (el: HTMLInputElement | null) => {
      own.current = el
      if (typeof ref === 'function') {
        const cleanup = ref(el)
        return () => {
          own.current = null
          if (typeof cleanup === 'function') cleanup()
          else ref(null)
        }
      }
      if (ref) ref.current = el
      return () => {
        own.current = null
        if (ref) ref.current = null
      }
    },
    [ref],
  )
  return (
    <label className={['ui-check', className ?? ''].filter(Boolean).join(' ')}>
      <input ref={setRef} type="checkbox" className="ui-control ui-check-box" onChange={change} {...rest} />
      <span>{label}</span>
    </label>
  )
}
