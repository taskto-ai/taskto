import type { ReactNode } from 'react'
import { LABEL_HUES } from './roles'
import './Label.css'

export type LabelHue = (typeof LABEL_HUES)[number]

const HUE_CLASS = {
  green: 'ui-hue-green',
  blue: 'ui-hue-blue',
  violet: 'ui-hue-violet',
  pink: 'ui-hue-pink',
  amber: 'ui-hue-amber',
  teal: 'ui-hue-teal',
} as const

export function normalizeLabelName(name: string): string {
  const spaced = name
    .normalize('NFC')
    .split(/\p{White_Space}+/u)
    .filter(Boolean)
    .join(' ')
  const folded = Array.from(spaced, (c) =>
    c === 'ı' ? c : c.toUpperCase().toLowerCase().toUpperCase().toLowerCase(),
  ).join('')
  return folded.normalize('NFC')
}

export function labelHue(name: string): LabelHue {
  const bytes = new TextEncoder().encode(normalizeLabelName(name))
  let h = 0x811c9dc5
  for (const b of bytes) {
    h ^= b
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return LABEL_HUES[h % LABEL_HUES.length]
}

export const hueClass = (hue: LabelHue) => HUE_CLASS[hue]

export interface LabelProps {
  name: string
  children?: ReactNode
  className?: string
}

export function Label({ name, children, className }: LabelProps) {
  return (
    <span className={['ui-label', hueClass(labelHue(name)), className ?? ''].filter(Boolean).join(' ')}>
      {children ?? name}
    </span>
  )
}
