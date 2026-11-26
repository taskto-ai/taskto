// What this client can draw, as names: the definition format it reads and the capabilities a
// release may need (apps.Capabilities on the server). Kept apart from the catalog (catalog.tsx),
// which holds the renderer itself: the offline download and its panel only ask whether a release
// can be drawn, and importing the catalog for that would load the renderer (A2UI and its schema
// library, most of a megabyte of code) on every page instead of only when an app is opened.
// capabilities.test.ts keeps these names equal to the catalog's, and apps.test.tsx to the server's.

// The version of the definition format this client reads (internal/apps DefinitionVersion), which
// is TaskTo.AI's and has nothing to do with the A2UI protocol's.
export const DEFINITION_VERSION = 3

// The kinds of effect an action may set going (internal/apps EffectKinds), which this client shows
// the progress of (AppEffects). A payment link is asked of an integration, and its progress is the
// integrations' to show.
export const EFFECT_KINDS = [
  'create_card',
  'update_card',
  'create_document',
  'open_route',
  'create_payment_link',
] as const

// The catalog's components and functions (appsCatalog's, by name).
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

// What this client can draw, named as the server names the capabilities a release needs
// (apps.Capabilities): component.<Name>, function.<name> and effect.<kind>.
export const catalogCapabilities: ReadonlySet<string> = new Set([
  ...COMPONENTS.map((name) => `component.${name}`),
  ...FUNCTIONS.map((name) => `function.${name}`),
  ...EFFECT_KINDS.map((kind) => `effect.${kind}`),
  // Holding an attachment field: reading its value as a list of attachment ids and sending them.
  'field.attachment',
  // Holding a location, a signature and a barcode field: their values as the server stores them.
  'field.location',
  'field.signature',
  'field.barcode',
  // Totalling one calendar month: a summary's month_of (offline/local.ts).
  'summary.month_of',
  // Drawing a choice by its label: /_labels/<field> of an item or a record (offline/local.ts).
  'data.labels',
  // The extension slots this client draws (internal/apps/contributions.go): Home's widgets and the
  // navigation's pin.
  'slot.home_widgets.v1',
  'slot.nav_pin.v1',
])

// What a release needs that this client doesn't have: a definition format newer than it reads, or
// capabilities its catalog lacks. Empty when the release can be drawn. A release that needs
// more is not drawn: the app says the client must be updated, instead of showing a broken screen.
export function unsupportedBy(needs: { definition_version: number; requires: readonly string[] }): string[] {
  const missing = needs.requires.filter((c) => !catalogCapabilities.has(c))
  return needs.definition_version > DEFINITION_VERSION
    ? [`definition_version.${needs.definition_version}`, ...missing]
    : missing
}
