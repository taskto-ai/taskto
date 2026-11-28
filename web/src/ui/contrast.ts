import type { ColorRole } from './roles'

export type Ground = ColorRole | readonly ColorRole[]

export interface PairSet {
  fg: readonly ColorRole[]
  on: readonly Ground[]
  min: number
}

export const PAGE_GROUNDS: readonly Ground[] = [
  'bg',
  'bg-sunken',
  'surface',
  'surface-own',
  ['hover', 'bg'],
  ['hover', 'surface'],
  ['hover', 'bg-sunken'],
  ['selected', 'bg'],
  ['selected', 'bg-sunken'],
  ['accent-soft', 'bg'],
  ['accent-soft', 'bg-sunken'],
]

const CARD_GROUNDS: readonly ColorRole[] = ['surface', 'bg-sunken', 'bg']

const onSoft = (soft: ColorRole): Ground[] => CARD_GROUNDS.map((g) => [soft, g])

export const TEXT_PAIRS: readonly PairSet[] = [
  {
    fg: ['text', 'text-soft', 'text-muted', 'link', 'success', 'warning', 'danger', 'info', 'neutral'],
    on: PAGE_GROUNDS,
    min: 4.5,
  },
  { fg: ['success'], on: onSoft('success-soft'), min: 4.5 },
  { fg: ['warning'], on: onSoft('warning-soft'), min: 4.5 },
  { fg: ['danger'], on: onSoft('danger-soft'), min: 4.5 },
  { fg: ['info'], on: onSoft('info-soft'), min: 4.5 },
  { fg: ['neutral', 'text'], on: onSoft('neutral-soft'), min: 4.5 },
  { fg: ['label-green'], on: onSoft('label-green-soft'), min: 4.5 },
  { fg: ['label-blue'], on: onSoft('label-blue-soft'), min: 4.5 },
  { fg: ['label-violet'], on: onSoft('label-violet-soft'), min: 4.5 },
  { fg: ['label-pink'], on: onSoft('label-pink-soft'), min: 4.5 },
  { fg: ['label-amber'], on: onSoft('label-amber-soft'), min: 4.5 },
  { fg: ['label-teal'], on: onSoft('label-teal-soft'), min: 4.5 },
  {
    fg: ['text', 'text-soft', 'link'],
    on: (['info-soft', 'success-soft', 'warning-soft', 'danger-soft', 'neutral-soft'] as const).flatMap((soft) =>
      (['bg', 'surface'] as const).map((g): Ground => [soft, g]),
    ),
    min: 4.5,
  },
  { fg: ['text', 'text-muted', 'danger'], on: [['selected', 'surface']], min: 4.5 },
  {
    fg: ['diff-add-text'],
    on: CARD_GROUNDS.flatMap((g): Ground[] => [
      ['diff-add-soft', g],
      ['diff-add', 'diff-add-soft', g],
    ]),
    min: 4.5,
  },
  {
    fg: ['diff-del-text'],
    on: CARD_GROUNDS.flatMap((g): Ground[] => [
      ['diff-del-soft', g],
      ['diff-del', 'diff-del-soft', g],
    ]),
    min: 4.5,
  },
  { fg: ['on-accent'], on: ['accent', 'accent-hover'], min: 4.5 },
  { fg: ['on-danger'], on: ['danger'], min: 4.5 },
  { fg: ['on-accent'], on: ['paper'], min: 4.5 },
  { fg: ['text-muted'], on: onSoft('neutral-soft'), min: 4.5 },
  { fg: ['bg'], on: ['text'], min: 4.5 },
  {
    fg: ['text'],
    on: [
      ['selected', 'bg-sunken'],
      ['selected', 'hover', 'bg'],
    ],
    min: 4.5,
  },
]

export const NON_TEXT_PAIRS: readonly PairSet[] = [
  { fg: ['border-strong', 'focus'], on: PAGE_GROUNDS, min: 3 },
  {
    fg: ['info', 'focus'],
    on: [
      ['info-soft', 'bg'],
      ['info-soft', 'surface'],
    ],
    min: 3,
  },
  {
    fg: ['success', 'focus'],
    on: [
      ['success-soft', 'bg'],
      ['success-soft', 'surface'],
    ],
    min: 3,
  },
  {
    fg: ['warning', 'focus'],
    on: [
      ['warning-soft', 'bg'],
      ['warning-soft', 'surface'],
    ],
    min: 3,
  },
  {
    fg: ['danger', 'focus'],
    on: [
      ['danger-soft', 'bg'],
      ['danger-soft', 'surface'],
    ],
    min: 3,
  },
  {
    fg: ['neutral', 'focus'],
    on: [
      ['neutral-soft', 'bg'],
      ['neutral-soft', 'surface'],
    ],
    min: 3,
  },
  { fg: ['accent-edge'], on: ['bg', 'bg-sunken', 'surface'], min: 3 },
  { fg: ['column-todo', 'column-doing', 'column-review', 'column-done'], on: ['bg-sunken', 'bg'], min: 3 },
]
