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
  indeterminate?: boolean
}

export function Checkbox({ label, indeterminate = false, className, ref, onChange, ...rest }: CheckboxProps) {
  const own = useRef<HTMLInputElement>(null)
  useLayoutEffect(() => {
    if (own.current) own.current.indeterminate = indeterminate
  })
  const change = (e: ChangeEvent<HTMLInputElement>) => {
    onChange?.(e)
    e.currentTarget.indeterminate = indeterminate
  }
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
