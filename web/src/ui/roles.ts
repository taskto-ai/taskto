// The colour roles: every one a brand theme (themes/*.css) sets, as `--color-<role>`, and nothing
// else. A test holds each theme to this list (a role missing or one not on it fails), and the
// gallery of components shows them. Add a role here and to every theme, with its pairs in
// contrast.ts when it is text, an edge or a mark.

export const COLOR_ROLES = [
  // Grounds
  'bg',
  'bg-sunken',
  'surface',
  'surface-own',
  'paper',
  'media',
  // Tints laid over a ground
  'hover',
  'selected',
  'scrim',
  'shadow',
  // Text
  'text',
  'text-soft',
  'text-muted',
  // Lines
  'border',
  'border-strong',
  'rule',
  // The primary action
  'accent',
  'accent-hover',
  'on-accent',
  'accent-edge',
  'accent-soft',
  // Focus, links, the agent, the logo
  'focus',
  'link',
  'node',
  'node-edge',
  'logo',
  // Status, each with a soft ground
  'success',
  'success-soft',
  'warning',
  'warning-soft',
  'danger',
  'danger-soft',
  'on-danger',
  'info',
  'info-soft',
  'neutral',
  'neutral-soft',
  // A board label's six hues
  'label-green',
  'label-green-soft',
  'label-blue',
  'label-blue-soft',
  'label-violet',
  'label-violet-soft',
  'label-pink',
  'label-pink-soft',
  'label-amber',
  'label-amber-soft',
  'label-teal',
  'label-teal-soft',
  // A board column's status dot
  'column-todo',
  'column-doing',
  'column-review',
  'column-done',
] as const

// The hues a label's name picks from, in a fixed order.
export const LABEL_HUES = ['green', 'blue', 'violet', 'pink', 'amber', 'teal'] as const

export type ColorRole = (typeof COLOR_ROLES)[number]

// The brands: the default, which is on :root, and the sample second brand, which only the tests
// and the gallery load (data-brand="harbor" on <html>, with themes/harbor.css imported).
export const DEFAULT_BRAND = 'mint'
export const BRANDS = ['mint', 'harbor'] as const
