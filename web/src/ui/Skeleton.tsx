// The shape of what is still loading: a line of text or a block, shimmering until it comes. It is
// hidden from screen readers; the region that waits says so (aria-busy, or a "Loading" of its own).

import type { CSSProperties } from 'react'
import './Skeleton.css'

export interface SkeletonProps {
  // A line of text (the default) or a block (a card, an image).
  shape?: 'text' | 'block'
  // Its width, as a CSS length or a percentage ("70%"); a block's height too.
  width?: string
  height?: string
  className?: string
}

export function Skeleton({ shape = 'text', width, height, className }: SkeletonProps) {
  const style: CSSProperties = {}
  if (width) style.inlineSize = width
  if (height) style.blockSize = height
  const cls = ['ui-skeleton', shape === 'block' ? 'ui-skeleton-block' : '', className ?? ''].filter(Boolean).join(' ')
  return <span className={cls} style={style} aria-hidden="true" />
}
