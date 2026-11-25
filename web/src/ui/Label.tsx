// A board label: its name in one of six hues. The hue is worked out from the name, so a label has
// the same colour on every card, for everyone, on every device and in every language, and nothing
// about it is stored. The hue only helps the eye find the same label again: the name says it.

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

// The hue of a name: FNV-1a (32 bits) over the name's UTF-8 bytes, after Unicode normalisation
// (NFC), trimming and lower-casing (toLowerCase, which doesn't depend on the locale), so "Launch",
// "launch " and a name typed with a combining accent all get one hue. Pure: the same in every
// browser, session and language.
export function labelHue(name: string): LabelHue {
  const bytes = new TextEncoder().encode(name.normalize('NFC').trim().toLowerCase())
  let h = 0x811c9dc5
  for (const b of bytes) {
    h ^= b
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return LABEL_HUES[h % LABEL_HUES.length]
}

// The class that colours something in a hue (a label, an avatar).
export const hueClass = (hue: LabelHue) => HUE_CLASS[hue]

export interface LabelProps {
  name: string
  // The words shown, when they are not the name itself (a count after it, a remove button).
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
