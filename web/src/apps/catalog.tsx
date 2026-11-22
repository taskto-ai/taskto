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
