import { hueClass, labelHue } from './Label'
import './Avatar.css'
import './Label.css'

export interface AvatarProps {
  name: string
  size?: 'sm' | 'md' | 'lg'
  shape?: 'round' | 'square'
  decorative?: boolean
  className?: string
  letters?: string
}

const SIZE_CLASS = { sm: 'ui-avatar-sm', md: '', lg: 'ui-avatar-lg' } as const

const segmenter =
  typeof Intl !== 'undefined' && 'Segmenter' in Intl ? new Intl.Segmenter(undefined, { granularity: 'grapheme' }) : null
function characters(text: string): string[] {
  return segmenter ? Array.from(segmenter.segment(text), (s) => s.segment) : Array.from(text)
}

export function initials(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return ''
  const first = (w: string) => characters(w)[0] ?? ''
  const letters = words.length === 1 ? first(words[0]) : first(words[0]) + first(words[words.length - 1])
  return letters.toUpperCase()
}

export function initialsAmong(names: readonly string[]): string[] {
  const chars = (name: string) => characters(name.replace(/\s+/g, ''))
  const base = names.map((name) => {
    const words = name.trim().split(/\s+/).filter(Boolean)
    if (words.length > 1) return initials(name)
    return chars(name).slice(0, 2).join('').toUpperCase()
  })
  const unique = (letters: string) => base.indexOf(letters) === base.lastIndexOf(letters)
  const choices = names.map((name, i) => {
    const own = chars(name)
    return [...new Set([base[i], ...own.slice(2).map((c) => (own[0] + c).toUpperCase())])]
  })
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
