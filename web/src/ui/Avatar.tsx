// A person's (or a workspace's) initials in a circle, or a square for a workspace, in the hue their
// name picks (the labels' hues, labelHue). Its name is the person's, for a screen reader; where the
// name is written beside it, it is `decorative` and says nothing.

import { hueClass, labelHue } from './Label'
import './Avatar.css'
import './Label.css'

export interface AvatarProps {
  name: string
  size?: 'sm' | 'md' | 'lg'
  shape?: 'round' | 'square'
  decorative?: boolean
  className?: string
  // The letters to show in place of the name's initials: from initialsAmong, where several
  // avatars side by side must not look alike.
  letters?: string
}

const SIZE_CLASS = { sm: 'ui-avatar-sm', md: '', lg: 'ui-avatar-lg' } as const

// Up to two letters: the first of the first word and of the last, as written (a whole character,
// so an accented letter or an emoji is not cut in half).
export function initials(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return ''
  const first = (w: string) => Array.from(w)[0] ?? ''
  const letters = words.length === 1 ? first(words[0]) : first(words[0]) + first(words[words.length - 1])
  return letters.toUpperCase()
}

// Two letters for each of several names shown together (a sidebar's pinned apps), told apart by
// their letters and not by hue alone: a name of several words, the first letters of its first and
// last; a name of one word, its first two. Names that would still look alike take, after their
// first letter, the first later one that sets each apart. Whole characters, as initials().
export function initialsAmong(names: readonly string[]): string[] {
  const chars = (name: string) => Array.from(name.replace(/\s+/g, ''))
  const base = names.map((name) => {
    const words = name.trim().split(/\s+/).filter(Boolean)
    if (words.length > 1) return initials(name)
    return chars(name).slice(0, 2).join('').toUpperCase()
  })
  const unique = (letters: string) => base.indexOf(letters) === base.lastIndexOf(letters)
  // What each name may show: its usual letters, then its first with each later one.
  const choices = names.map((name, i) => {
    const own = chars(name)
    return [...new Set([base[i], ...own.slice(2).map((c) => (own[0] + c).toUpperCase())])]
  })
  // The letters taken so far: every name's own that is unique, then each one given out. The names
  // with the fewest choices choose first (in their order when equal), so a short name's only
  // letters stay its own; the answer keeps the names' order.
  const used = new Set(base.filter(unique))
  const out = base.slice()
  const rest = names.map((_, i) => i).filter((i) => !unique(base[i]))
  rest.sort((a, b) => choices[a].length - choices[b].length || a - b)
  for (const i of rest) {
    const pick = choices[i].find((tried) => !used.has(tried)) ?? base[i]
    used.add(pick)
    out[i] = pick
  }
  return out
}

export function Avatar({ name, size = 'md', shape = 'round', decorative = false, className, letters }: AvatarProps) {
  const cls = [
    'ui-avatar',
    hueClass(labelHue(name)),
    SIZE_CLASS[size],
    shape === 'square' ? 'ui-avatar-square' : '',
    className ?? '',
  ]
    .filter(Boolean)
    .join(' ')
  const named = decorative ? { 'aria-hidden': true as const } : { role: 'img', 'aria-label': name }
  return (
    <span className={cls} {...named}>
      {letters ?? initials(name)}
    </span>
  )
}
