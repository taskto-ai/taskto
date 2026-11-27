import type { CSSProperties } from 'react'
import './Skeleton.css'

export interface SkeletonProps {
  shape?: 'text' | 'block'
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
