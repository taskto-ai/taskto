// The app's line icons, drawn inline: one path each on a 24×24 grid, stroked with the text colour,
// round caps and joins. Every size draws the same line on screen (1.5px): the stroke is set in the
// grid's units from the size, so a 14px icon is not thinner than a 20px one.
//
// An icon is decoration beside words that say the same, so it is hidden from screen readers; one
// that stands alone and means something takes a `label`. Icons that point a direction (back,
// forward, a chevron to the side, send, move) are mirrored when the page is right to left.

import './Icon.css'

const PATHS = {
  // Navigation and the shell
  menu: 'M4 6h16M4 12h16M4 18h16',
  home: 'M4 11l8-7 8 7M6 10v10h12V10M10 20v-6h4v6',
  pin: 'M9 4h6l-1 6 3 3H7l3-3-1-6zM12 13v7',
  conversations: 'M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z',
  knowledge: 'M4 5a2 2 0 0 1 2-2h13v16H6a2 2 0 0 0-2 2V5zM4 21a2 2 0 0 1 2-2h13v2',
  boards: 'M4 4h4v16H4zM10 4h4v10h-4zM16 4h4v13h-4z',
  apps: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  settings: 'M4 6h9M17 6h3M15 4v4M4 12h3M11 12h9M9 10v4M4 18h11M19 18h1M17 16v4',
  bell: 'M6 17h12l-1.5-2v-5a4.5 4.5 0 0 0-9 0v5L6 17zM10 20h4',
  browser: 'M3 5h18v14H3zM3 9h18M6 7h.01M9 7h.01',
  // The theme
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4',
  moon: 'M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z',
  contrast: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 3v18',
  // Chevrons and arrows
  up: 'M6 15l6-6 6 6',
  down: 'M6 9l6 6 6-6',
  'chevron-up': 'M6 15l6-6 6 6',
  'chevron-down': 'M6 9l6 6 6-6',
  'chevron-start': 'M15 18l-6-6 6-6',
  'chevron-end': 'M9 6l6 6-6 6',
  'chevron-updown': 'M8 9l4-4 4 4M8 15l4 4 4-4',
  back: 'M15 18l-6-6 6-6',
  send: 'M5 12h14M13 6l6 6-6 6',
  move: 'M4 12h14M13 6l6 6-6 6',
  upload: 'M12 16V4M7 9l5-5 5 5M4 20h16',
  // Actions
  plus: 'M12 5v14M5 12h14',
  close: 'M6 6l12 12M18 6L6 18',
  check: 'M5 12.5l4.5 4.5L19 7.5',
  edit: 'M4 20h4L19 9l-4-4L4 16v4zM13.5 6.5l4 4',
  archive: 'M3 4h18v4H3zM5 8v12h14V8M10 12h4',
  copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
  search: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20 20l-4-4',
  filter: 'M4 6h16M7 12h10M10 18h4',
  // Three dots, drawn as small rings that the stroke fills.
  more: 'M6 12a1 1 0 1 1-2 0 1 1 0 0 1 2 0zM13 12a1 1 0 1 1-2 0 1 1 0 0 1 2 0zM20 12a1 1 0 1 1-2 0 1 1 0 0 1 2 0z',
  stop: 'M7 7h10v10H7z',
  mic: 'M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3zM5 11a7 7 0 0 0 14 0M12 18v3',
  // Things
  folder: 'M3 6h6l2 2h10v11H3z',
  file: 'M6 3h8l4 4v14H6zM14 3v4h4',
  clock: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 7.5V12l3 2',
  // Status
  info: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 11v6M12 7.5h.01',
  alert: 'M12 4l9 16H3zM12 10v4M12 17h.01',
  block: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM5.7 5.7l12.6 12.6',
  offline: 'M4 4l16 16M8.5 8.6A6 6 0 0 0 6 18h10M10.7 6.2A6 6 0 0 1 18 12a4 4 0 0 1 2 5.5',
} as const

export type IconName = keyof typeof PATHS

export const ICON_NAMES = Object.keys(PATHS) as IconName[]

// The icons that point along the line of text, mirrored right to left.
const DIRECTIONAL: ReadonlySet<IconName> = new Set(['chevron-start', 'chevron-end', 'back', 'send', 'move'])

export const isDirectional = (name: IconName) => DIRECTIONAL.has(name)

// The sizes, in px: a badge's (12), a menu's or a breadcrumb's (14, 16), a button's (18), the
// shell's (20) and a large one (24).
export type IconSize = 12 | 14 | 16 | 18 | 20 | 24

// The line every icon draws, in px on screen.
const STROKE_PX = 1.5

export interface IconProps {
  name: IconName
  size?: IconSize
  // What the icon says, when no words beside it do: it is then an image with this name.
  label?: string
  className?: string
}

export function Icon({ name, size = 20, label, className }: IconProps) {
  const classes = ['ui-icon', isDirectional(name) ? 'ui-icon-directional' : '', className ?? '']
    .filter(Boolean)
    .join(' ')
  const named = label ? { role: 'img', 'aria-label': label } : { 'aria-hidden': true as const }
  return (
    // biome-ignore lint/a11y/noSvgWithoutTitle: decorative icons are aria-hidden; one with a label is named by aria-label.
    <svg
      className={classes}
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={(STROKE_PX * 24) / size}
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
      {...named}
    >
      <path d={PATHS[name]} />
    </svg>
  )
}
