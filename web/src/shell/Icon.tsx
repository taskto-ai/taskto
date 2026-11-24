// The few line icons the shell uses, drawn inline: 24×24, stroked with the text colour.

const PATHS = {
  menu: 'M4 6h16M4 12h16M4 18h16',
  home: 'M4 11l8-7 8 7M6 10v10h12V10M10 20v-6h4v6',
  pin: 'M9 4h6l-1 6 3 3H7l3-3-1-6zM12 13v7',
  up: 'M6 15l6-6 6 6',
  down: 'M6 9l6 6 6-6',
  conversations: 'M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z',
  knowledge: 'M4 5a2 2 0 0 1 2-2h13v16H6a2 2 0 0 0-2 2V5zM4 21a2 2 0 0 1 2-2h13v2',
  boards: 'M4 4h4v16H4zM10 4h4v10h-4zM16 4h4v13h-4z',
  apps: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  settings: 'M4 6h9M17 6h3M15 4v4M4 12h3M11 12h9M9 10v4M4 18h11M19 18h1M17 16v4',
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4',
  moon: 'M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z',
  contrast: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 3v18',
  plus: 'M12 5v14M5 12h14',
  close: 'M6 6l12 12M18 6L6 18',
  back: 'M15 18l-6-6 6-6',
  send: 'M5 12h14M13 6l6 6-6 6',
  stop: 'M7 7h10v10H7z',
  edit: 'M4 20h4L19 9l-4-4L4 16v4zM13.5 6.5l4 4',
  archive: 'M3 4h18v4H3zM5 8v12h14V8M10 12h4',
  mic: 'M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3zM5 11a7 7 0 0 0 14 0M12 18v3',
  bell: 'M6 17h12l-1.5-2v-5a4.5 4.5 0 0 0-9 0v5L6 17zM10 20h4',
  browser: 'M3 5h18v14H3zM3 9h18M6 7h.01M9 7h.01',
} as const

export function Icon({ name }: { name: keyof typeof PATHS }) {
  return (
    <svg
      className="tt-icon"
      viewBox="0 0 24 24"
      width="20"
      height="20"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={PATHS[name]} />
    </svg>
  )
}
