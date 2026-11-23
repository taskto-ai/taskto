// The component catalog personal apps may use: A2UI's basic components and functions, without
// the ones that load or open outside addresses (an app is shared, and a picture from elsewhere
// would tell that server who opens it), plus TaskTo.AI's own (Stat, Show, PhotoInput, Photos). A surface naming anything outside
// it is rejected by the renderer, so an app can only ever be built from these. The Go server
// checks every saved app against the same catalog (internal/apps/catalog.go); its
// testdata/catalog.json is the contract the tests hold both to.

import { createComponentImplementation, useSignalValue } from '@a2ui/react/v0_9'
import { basicCatalog, Catalog, CommonSchemas } from '@a2ui/web_core/v0_9'
import { z } from 'zod'
import { PhotoField, PhotoStrip, useAppFiles } from './PhotoInput'
import { type PhotoLimits, photoIds } from './photos'
import { holds, type Rule, type Values } from './rules'

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

// Draws its child only while its rule holds of the form (/draft, the default) or of the record a
// screen shows (/record). It hides an input for people and decides nothing the server accepts.
// Reading the whole object is what keeps it up to date: a change to any of its fields notifies it.
const ShowApi = {
  name: 'Show',
  schema: z
    .object({
      when: z.any(),
      source: z.enum(['draft', 'record']).optional(),
      child: CommonSchemas.ComponentId,
    })
    .strict(),
}

const Show = createComponentImplementation(ShowApi, ({ props, buildChild, context }) => {
  const source = props.source ?? 'draft'
  const values = useSignalValue(context.dataContext.dataModel.getSignal<Values>(`/${source}`))
  return holds(props.when as Rule, values ?? {}) ? buildChild(props.child) : null
})

// An input for an attachment field: photos from the camera or the library, each uploaded to the
// server, their ids held in value (a list at a path such as /draft/photos). field names the
// collection's attachment field, whose limits the server puts at /_files/<field>.
const PhotoInputApi = {
  name: 'PhotoInput',
  schema: z
    .object({
      field: z.string(),
      label: CommonSchemas.DynamicString.optional(),
      value: CommonSchemas.DynamicStringList,
    })
    .strict(),
}

const PhotoInput = createComponentImplementation(PhotoInputApi, ({ props, context }) => {
  const files = useAppFiles()
  const limits = useSignalValue(context.dataContext.dataModel.getSignal<PhotoLimits>(`/_files/${props.field}`))
  const setValue = (props as { setValue?: (ids: string[]) => void }).setValue
  return (
    <PhotoField
      label={props.label}
      files={files}
      field={props.field}
      value={photoIds(props.value)}
      limits={limits ?? undefined}
      onChange={(ids) => setValue?.(ids)}
    />
  )
})

// The thumbnails of the attachments whose ids are in value, drawn from the server with the viewer's
// own session: never from an outside address.
const PhotosApi = {
  name: 'Photos',
  schema: z
    .object({
      label: CommonSchemas.DynamicString.optional(),
      value: CommonSchemas.DynamicStringList,
    })
    .strict(),
}

const Photos = createComponentImplementation(PhotosApi, ({ props }) => {
  const files = useAppFiles()
  return files ? <PhotoStrip base={files.base} ids={photoIds(props.value)} label={props.label} /> : null
})

// The host of an app's screens, which the server adds as the root: it draws the screen the data
// model names (/screen). Moving between screens only changes that, so what is typed into the form
// (/draft) is where it was left.
const ScreensApi = {
  name: 'Screens',
  schema: z
    .object({
      current: CommonSchemas.DynamicString,
      screens: z.array(z.object({ name: z.string(), child: CommonSchemas.ComponentId }).strict()),
    })
    .strict(),
}

const Screens = createComponentImplementation(ScreensApi, ({ props, buildChild }) => {
  const shown = props.screens.find((s) => s.name === props.current)
  return <div className="tt-screen">{shown ? buildChild(shown.child) : null}</div>
})

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
    Show,
    PhotoInput,
    Photos,
    Screens,
  ],
  [...basicCatalog.functions.values()].filter((f) => !LEFT_OUT.has(f.name)),
)

// The version of the definition format this client reads (internal/apps DefinitionVersion), which
// is TaskTo.AI's and has nothing to do with the A2UI protocol's.
export const DEFINITION_VERSION = 2

// The kinds of effect an action may set going (internal/apps EffectKinds), which this client shows
// the progress of (AppEffects).
export const EFFECT_KINDS = ['create_card', 'update_card', 'create_document', 'open_route'] as const

// What this client can draw, named as the server names the capabilities a release needs
// (apps.Capabilities): component.<Name>, function.<name> and effect.<kind>.
export const catalogCapabilities: ReadonlySet<string> = new Set([
  ...[...appsCatalog.components.keys()].map((name) => `component.${name}`),
  ...[...appsCatalog.functions.keys()].map((name) => `function.${name}`),
  ...EFFECT_KINDS.map((kind) => `effect.${kind}`),
  // Holding an attachment field: reading its value as a list of attachment ids and sending them.
  'field.attachment',
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
