export const DEFINITION_VERSION = 3

export const EFFECT_KINDS = [
  'create_card',
  'update_card',
  'create_document',
  'open_route',
  'create_payment_link',
] as const

export const COMPONENTS = [
  'BarcodeInput',
  'Button',
  'Card',
  'CheckBox',
  'ChoicePicker',
  'Column',
  'DateTimeInput',
  'Divider',
  'Icon',
  'List',
  'LocationInput',
  'Markdown',
  'Modal',
  'PhotoInput',
  'Photos',
  'Row',
  'Screens',
  'Show',
  'SignatureInput',
  'Slider',
  'Stat',
  'Tabs',
  'Text',
  'TextField',
] as const

export const FUNCTIONS = [
  'and',
  'email',
  'formatCurrency',
  'formatDate',
  'formatNumber',
  'formatString',
  'length',
  'not',
  'numeric',
  'or',
  'pluralize',
  'required',
] as const

export const catalogCapabilities: ReadonlySet<string> = new Set([
  ...COMPONENTS.map((name) => `component.${name}`),
  ...FUNCTIONS.map((name) => `function.${name}`),
  ...EFFECT_KINDS.map((kind) => `effect.${kind}`),
  'field.attachment',
  'field.location',
  'field.signature',
  'field.barcode',
  'summary.month_of',
  'data.labels',
  'icon.svg_path',
  'slot.home_widgets.v1',
  'slot.nav_pin.v1',
])

export function unsupportedBy(needs: { definition_version: number; requires: readonly string[] }): string[] {
  const missing = needs.requires.filter((c) => !catalogCapabilities.has(c))
  return needs.definition_version > DEFINITION_VERSION
    ? [`definition_version.${needs.definition_version}`, ...missing]
    : missing
}
