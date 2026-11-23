// The component catalog personal apps may use: A2UI's basic components and functions, without
// the ones that load or open outside addresses (an app is shared, and a picture from elsewhere
// would tell that server who opens it), plus TaskTo.AI's own. A surface naming anything outside
// it is rejected by the renderer, so an app can only ever be built from these. The Go server
// checks every saved app against the same catalog (internal/apps/catalog.go); its
// testdata/catalog.json is the contract the tests hold both to.

import { createComponentImplementation } from '@a2ui/react/v0_9'
import { basicCatalog, Catalog, CommonSchemas } from '@a2ui/web_core/v0_9'
import { z } from 'zod'

export const APPS_CATALOG_ID = 'https://taskto.ai/catalogs/apps/v1.json'

const StatApi = {
  name: 'Stat',
  schema: z
    .object({
      label: CommonSchemas.DynamicString,
      value: CommonSchemas.DynamicString,
    })
    .strict(),
}

// A labelled headline number, such as a total: a term and its value.
const Stat = createComponentImplementation(StatApi, ({ props }) => (
  <dl className="tt-stat">
    <dt className="tt-stat-label">{props.label}</dt>
    <dd className="tt-stat-value">{props.value}</dd>
  </dl>
))

// Left out: what loads from elsewhere, and regular expressions. An app's pattern would run in
// every viewer's browser against text other members can store, and a pattern such as (a+)+
// or .*.*.*.* can freeze it; the server refuses both too (internal/apps/catalog.go).
const LEFT_OUT = new Set(['Image', 'Video', 'AudioPlayer', 'openUrl', 'regex'])

// A2UI's TextField without its validationRegexp prop, for the same reason.
const basicTextField = basicCatalog.components.get('TextField')
const TextField = basicTextField && {
  ...basicTextField,
  schema: (basicTextField.schema as z.AnyZodObject).omit({ validationRegexp: true }),
}

export const appsCatalog = new Catalog(
  APPS_CATALOG_ID,
  '0.9',
  [
    ...[...basicCatalog.components.values()].filter((c) => !LEFT_OUT.has(c.name) && c.name !== 'TextField'),
    ...(TextField ? [TextField] : []),
    Stat,
  ],
  [...basicCatalog.functions.values()].filter((f) => !LEFT_OUT.has(f.name)),
)

// The version of the definition format this client reads (internal/apps DefinitionVersion), which
// is TaskTo.AI's and has nothing to do with the A2UI protocol's.
export const DEFINITION_VERSION = 1

// What this client can draw, named as the server names the capabilities a release needs
// (apps.Capabilities): component.<Name> and function.<name>.
export const catalogCapabilities: ReadonlySet<string> = new Set([
  ...[...appsCatalog.components.keys()].map((name) => `component.${name}`),
  ...[...appsCatalog.functions.keys()].map((name) => `function.${name}`),
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
