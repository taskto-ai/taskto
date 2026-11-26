// The component catalog personal apps may use: A2UI's basic components and functions, without
// the ones that load or open outside addresses (an app is shared, and a picture from elsewhere
// would tell that server who opens it), plus TaskTo.AI's own (Stat, Show, PhotoInput, Photos,
// LocationInput, SignatureInput, BarcodeInput). A surface naming anything outside
// it is rejected by the renderer, so an app can only ever be built from these. The Go server
// checks every saved app against the same catalog (internal/apps/catalog.go); its
// testdata/catalog.json is the contract the tests hold both to.

import { createComponentImplementation, useSignalValue } from '@a2ui/react/v0_9'
import { basicCatalog, Catalog, CommonSchemas } from '@a2ui/web_core/v0_9'
import { z } from 'zod'
import { BarcodeField } from './BarcodeInput'
import { type CaptureInfo, isLocation } from './capture'
import { iconPath } from './iconPath'
import { LocationField } from './LocationInput'
import { PhotoField, PhotoStrip, useAppFiles } from './PhotoInput'
import { type PhotoLimits, photoIds, signatureId } from './photos'
import { holds, type Rule, type Values } from './rules'
import { SignatureField } from './SignaturePad'

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

// A labelled headline number, such as a total: a term and its value, drawn as a tile (AppView.css);
// tiles in one of the app's rows wrap onto the next line when there isn't room.
const Stat = createComponentImplementation(StatApi, ({ props }) => (
  <dl className="ui-stat">
    <dt className="ui-stat-label">{props.label}</dt>
    <dd className="ui-stat-value">{props.value}</dd>
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

// An input for a location field (internal/apps/capture.go): the person's position, read once on a
// button or as the form is submitted, or typed when the device can't give it. value holds the
// location object; readOnly only shows what is stored.
const LocationInputApi = {
  name: 'LocationInput',
  schema: z
    .object({
      field: z.string(),
      label: CommonSchemas.DynamicString.optional(),
      value: CommonSchemas.DynamicValue,
      readOnly: z.boolean().optional(),
    })
    .strict(),
}

const LocationInput = createComponentImplementation(LocationInputApi, ({ props, context }) => {
  const info = useSignalValue(context.dataContext.dataModel.getSignal<CaptureInfo>(`/_fields/${props.field}`))
  const setValue = (props as { setValue?: (value: unknown) => void }).setValue
  return (
    <LocationField
      label={props.label}
      field={props.field}
      value={isLocation(props.value) ? props.value : undefined}
      onChange={(loc) => setValue?.(loc)}
      info={info ?? undefined}
      readOnly={props.readOnly}
    />
  )
})

// An input for a signature field: a pad to draw on, or a typed name, saved as a PNG attachment whose
// id is held in value.
const SignatureInputApi = {
  name: 'SignatureInput',
  schema: z
    .object({
      field: z.string(),
      label: CommonSchemas.DynamicString.optional(),
      value: CommonSchemas.DynamicValue,
      readOnly: z.boolean().optional(),
    })
    .strict(),
}

const SignatureInput = createComponentImplementation(SignatureInputApi, ({ props }) => {
  const files = useAppFiles()
  const setValue = (props as { setValue?: (value: unknown) => void }).setValue
  return (
    <SignatureField
      label={props.label}
      files={files}
      field={props.field}
      value={signatureId(props.value)}
      onChange={(id) => setValue?.(id)}
      readOnly={props.readOnly}
    />
  )
})

// An input for a barcode field: the code typed, or scanned with the camera.
const BarcodeInputApi = {
  name: 'BarcodeInput',
  schema: z
    .object({
      field: z.string(),
      label: CommonSchemas.DynamicString.optional(),
      value: CommonSchemas.DynamicString,
    })
    .strict(),
}

const BarcodeInput = createComponentImplementation(BarcodeInputApi, ({ props, context }) => {
  const info = useSignalValue(context.dataContext.dataModel.getSignal<CaptureInfo>(`/_fields/${props.field}`))
  const setValue = (props as { setValue?: (value: string) => void }).setValue
  return (
    <BarcodeField
      label={props.label}
      value={typeof props.value === 'string' ? props.value : ''}
      onChange={(code) => setValue?.(code)}
      info={info ?? undefined}
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
  return files ? (
    <PhotoStrip base={files.base} ids={photoIds(props.value)} label={props.label} local={files.local} />
  ) : null
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
  return <div className="ui-app-screen">{shown ? buildChild(shown.child) : null}</div>
})

// Left out: what loads from elsewhere, and regular expressions. An app's pattern would run in
// every viewer's browser against text other members can store, and a pattern such as (a+)+
// or .*.*.*.* can freeze it; the server refuses both too (internal/apps/catalog.go).
const LEFT_OUT = new Set(['Image', 'Video', 'AudioPlayer', 'openUrl', 'regex'])

// A2UI's Icon, drawn by us: its name is SVG path data ({"svgPath": "…"}), which the server checked
// on save and iconPath checks again here. The <svg> and <path> are made here and the string goes
// only into the path's d attribute, through React: never markup, a URL, a style or a class. The
// view box is fixed and the fill is the text's colour. A name draws nothing: this client loads no
// icon font, and the word would read as text. An icon with a label is an image named so (the
// server requires one for an icon that is all of a button); without one it is decoration, hidden
// from assistive technology, as beside a label.
const basicIcon = basicCatalog.components.get('Icon')
const IconApi = { name: 'Icon', schema: basicIcon?.schema ?? z.object({}).passthrough() }

const Icon = createComponentImplementation(IconApi, ({ props }) => {
  const { name, accessibility } = props as { name?: unknown; accessibility?: { label?: unknown } }
  const d = iconPath(name)
  if (!d) return null
  const label = typeof accessibility?.label === 'string' ? accessibility.label.trim() : ''
  return (
    // biome-ignore lint/a11y/noSvgWithoutTitle: an icon without a label is aria-hidden; one with a label is named by aria-label.
    <svg
      className="ui-app-icon"
      viewBox="0 0 24 24"
      fill="currentColor"
      focusable="false"
      {...(label ? { role: 'img', 'aria-label': label } : { 'aria-hidden': true })}
    >
      <path d={d} />
    </svg>
  )
})

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
    ...[...basicCatalog.components.values()].filter(
      (c) => !LEFT_OUT.has(c.name) && c.name !== 'TextField' && c.name !== 'Icon',
    ),
    ...(TextField ? [TextField] : []),
    Icon,
    Stat,
    Show,
    PhotoInput,
    Photos,
    LocationInput,
    SignatureInput,
    BarcodeInput,
    Screens,
  ],
  [...basicCatalog.functions.values()].filter((f) => !LEFT_OUT.has(f.name)),
)

// The names of what the catalog holds, and what a release may need, live in capabilities.ts, which
// doesn't load the renderer; they are said again here for the modules that have the catalog anyway.
export { catalogCapabilities, DEFINITION_VERSION, EFFECT_KINDS, unsupportedBy } from './capabilities'
