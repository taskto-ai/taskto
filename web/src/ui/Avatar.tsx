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

export function Avatar({ name, size = 'md', shape = 'round', decorative = false, className }: AvatarProps) {
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
      {initials(name)}
    </span>
  )
}
