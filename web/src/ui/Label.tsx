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

// A label's name as the server keeps it (normalizeLabel in internal/boards/validate.go): runs of
// white space (Unicode's White_Space, Go's unicode.IsSpace) made one space and trimmed, then full
// case folding between two NFC normalisations. JavaScript has no case folding: each character is
// taken to upper case and back, twice ("ẞ" → "ß" → "ss"), one at a time so the final sigma stays
// "σ" as folding has it, except the dotless "ı", which folding leaves alone and the round trip
// would make "i". Neither step depends on the locale.
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

// The hue of a name: FNV-1a (32 bits) over the UTF-8 bytes of the name as the server keeps it, so
// "Launch", "launch ", "tech  debt" and a name typed with a combining accent each get the hue of
// the label they are. Pure: the same in every browser, session and language.
export function labelHue(name: string): LabelHue {
  const bytes = new TextEncoder().encode(normalizeLabelName(name))
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
