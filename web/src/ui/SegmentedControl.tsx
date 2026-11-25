// A choice of one among a few, shown side by side (Write or Preview; List or Board). Underneath it
// is a group of the browser's own radio buttons, so the arrow keys move the choice, Tab enters
// and leaves the group as one stop, and a screen reader says "1 of 2, selected". It is for a
// choice of view or mode; a form's choice among many is a Select.

import { type ReactNode, useId } from 'react'
import { Icon, type IconName } from './Icon'
import './SegmentedControl.css'

export interface Segment<V extends string> {
  value: V
  label: ReactNode
  icon?: IconName
  disabled?: boolean
}

export interface SegmentedControlProps<V extends string> {
  // The group's name for a screen reader: what is being chosen.
  label: string
  options: readonly Segment<V>[]
  value: V
  onChange: (value: V) => void
  size?: 'md' | 'sm'
  // The radios' name, for a form; one is made otherwise.
  name?: string
  disabled?: boolean
  className?: string
}

export function SegmentedControl<V extends string>({
  label,
  options,
  value,
  onChange,
  size = 'md',
  name,
  disabled = false,
  className,
}: SegmentedControlProps<V>) {
  const made = useId()
  const group = name ?? made
  const cls = ['ui-segmented', size === 'sm' ? 'ui-segmented-sm' : '', className ?? ''].filter(Boolean).join(' ')
  return (
    <div role="radiogroup" aria-label={label} aria-disabled={disabled || undefined} className={cls}>
      {options.map((o) => (
        <label key={o.value} className="ui-segment">
          <input
            type="radio"
            className="ui-control ui-segment-input"
            name={group}
            value={o.value}
            checked={o.value === value}
            disabled={disabled || o.disabled}
            onChange={() => onChange(o.value)}
          />
          <span className="ui-segment-face">
            {o.icon && <Icon name={o.icon} size={16} />}
            {o.label}
          </span>
        </label>
      ))}
    </div>
  )
}
