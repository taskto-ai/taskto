// The colour pairs the roles allow, as data: what may be drawn on what, and the contrast it must
// reach (WCAG 2.x). A test computes each from every brand theme's values, in light and in dark.
// A screen that puts a role on a ground not listed here adds the pair here first.
//
// A ground is a role, or roles laid one over another, top first: ['selected', 'bg-sunken'] is the
// selected tint on a column. The last of a stack must be opaque.

import type { ColorRole } from './roles'

export type Ground = ColorRole | readonly ColorRole[]

export interface PairSet {
  // What is drawn: text, an edge or a mark.
  fg: readonly ColorRole[]
  // What it is drawn on.
  on: readonly Ground[]
  // At least this: 4.5 for text, 3 for a control's edge, the focus ring and a mark that must be
  // seen on its own.
  min: number
}

// The grounds of the page and what sits on it: everything a line of text can be on.
export const PAGE_GROUNDS: readonly Ground[] = [
  'bg',
  'bg-sunken',
  'surface',
  'surface-own',
  ['hover', 'bg'],
  ['hover', 'surface'],
  ['selected', 'bg'],
  ['selected', 'bg-sunken'],
  ['accent-soft', 'bg'],
  ['accent-soft', 'bg-sunken'],
]

// The grounds a label or a status badge sits on: a card, a column, the page.
const CARD_GROUNDS: readonly ColorRole[] = ['surface', 'bg-sunken', 'bg']

const onSoft = (soft: ColorRole): Ground[] => CARD_GROUNDS.map((g) => [soft, g])

export const TEXT_PAIRS: readonly PairSet[] = [
  // Text of every level, links and status text on every ground of the page.
  {
    fg: ['text', 'text-soft', 'text-muted', 'link', 'success', 'warning', 'danger', 'info', 'neutral'],
    on: PAGE_GROUNDS,
    min: 4.5,
  },
  // A status badge: its colour on its own soft ground.
  { fg: ['success'], on: onSoft('success-soft'), min: 4.5 },
  { fg: ['warning'], on: onSoft('warning-soft'), min: 4.5 },
  { fg: ['danger'], on: onSoft('danger-soft'), min: 4.5 },
  { fg: ['info'], on: onSoft('info-soft'), min: 4.5 },
  { fg: ['neutral', 'text'], on: onSoft('neutral-soft'), min: 4.5 },
  // A label: its hue on its soft ground.
  { fg: ['label-green'], on: onSoft('label-green-soft'), min: 4.5 },
  { fg: ['label-blue'], on: onSoft('label-blue-soft'), min: 4.5 },
  { fg: ['label-violet'], on: onSoft('label-violet-soft'), min: 4.5 },
  { fg: ['label-pink'], on: onSoft('label-pink-soft'), min: 4.5 },
  { fg: ['label-amber'], on: onSoft('label-amber-soft'), min: 4.5 },
  { fg: ['label-teal'], on: onSoft('label-teal-soft'), min: 4.5 },
  // A notice: its words (and a link in them) on its tone's full tint, on the page and in a panel.
  {
    fg: ['text', 'text-soft', 'link'],
    on: (['info-soft', 'success-soft', 'warning-soft', 'danger-soft', 'neutral-soft'] as const).flatMap((soft) =>
      (['bg', 'surface'] as const).map((g): Ground => [soft, g]),
    ),
    min: 4.5,
  },
  // A row in a panel: the current one, and one under the pointer.
  { fg: ['text', 'text-muted', 'danger'], on: [['selected', 'surface']], min: 4.5 },
  // What sits on a fill: the primary button, a danger button.
  { fg: ['on-accent'], on: ['accent', 'accent-hover'], min: 4.5 },
  { fg: ['on-danger'], on: ['danger'], min: 4.5 },
  // A QR code or a signature: ink on paper whatever the theme.
  { fg: ['on-accent'], on: ['paper'], min: 4.5 },
  // A disabled control: muted text on the neutral ground (Button, the fields).
  { fg: ['text-muted'], on: onSoft('neutral-soft'), min: 4.5 },
  // A tooltip: the page's colour on the text colour (Tooltip).
  { fg: ['bg'], on: ['text'], min: 4.5 },
  // A secondary or ghost button pressed: text on the pressed tint over the hover ground.
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
  // The edge of a control, and the focus ring, on every ground.
  { fg: ['border-strong', 'focus'], on: PAGE_GROUNDS, min: 3 },
  // A notice's icon on its tint, and the focus ring of a link or button in it.
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
  // The primary button's outline against the page.
  { fg: ['accent-edge'], on: ['bg', 'bg-sunken', 'surface'], min: 3 },
  // A column's status dot, on the column and on the page.
  { fg: ['column-todo', 'column-doing', 'column-review', 'column-done'], on: ['bg-sunken', 'bg'], min: 3 },
]
