import { createComponentImplementation, useSignalValue } from '@a2ui/react/v0_9'
import { basicCatalog, Catalog, CommonSchemas } from '@a2ui/web_core/v0_9'
import { z } from 'zod'
import { Markdown as MarkdownText } from '../markdown'
import { BarcodeField } from './BarcodeInput'
import { ChartComponent } from './Chart'
import { type CaptureInfo, isLocation } from './capture'
import { iconPath, iconsShowingTheirName, type SurfaceComponent } from './iconPath'
import { LocationField } from './LocationInput'
import { PhotoField, PhotoStrip, useAppFiles } from './PhotoInput'
import { type PhotoLimits, photoIds, signatureId } from './photos'
import { holds, type Rule, type Values } from './rules'
import { SignatureField } from './SignaturePad'
import { TableComponent } from './TableView'

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

const Stat = createComponentImplementation(StatApi, ({ props }) => (
  <dl className="ui-stat">
    <dt className="ui-stat-label">{props.label}</dt>
    <dd className="ui-stat-value">{props.value}</dd>
  </dl>
))

const MarkdownApi = {
  name: 'Markdown',
  schema: z
    .object({
      text: CommonSchemas.DynamicString,
      accessibility: CommonSchemas.AccessibilityAttributes.optional(),
      weight: z.number().optional(),
    })
    .strict(),
}

const Markdown = createComponentImplementation(MarkdownApi, ({ props }) => (
  <div className="ui-app-markdown">
    <MarkdownText text={props.text ?? ''} headingShift={2} links="text" />
  </div>
))

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

const LEFT_OUT = new Set(['Image', 'Video', 'AudioPlayer', 'openUrl', 'regex'])

const basicIcon = basicCatalog.components.get('Icon')
const IconApi = { name: 'Icon', schema: basicIcon?.schema ?? z.object({}).passthrough() }

function surfaceComponents(context: {
  surfaceComponents: { getAll(): Map<string, { type: string; properties: Record<string, unknown> }> }
}) {
  const out = new Map<string, SurfaceComponent>()
  for (const [id, model] of context.surfaceComponents.getAll())
    out.set(id, { component: model.type, props: model.properties })
  return out
}

const Icon = createComponentImplementation(IconApi, ({ props, context }) => {
  const { name, accessibility } = props as { name?: unknown; accessibility?: { label?: unknown } }
  if (typeof name === 'string') {
    const shown = iconsShowingTheirName(surfaceComponents(context)).has(context.componentModel.id)
    return shown ? <span className="ui-app-icon-name">{name}</span> : null
  }
  const d = iconPath(name)
  if (!d) return null
  const label = typeof accessibility?.label === 'string' ? accessibility.label.trim() : ''
  return (
    <svg
      className="ui-app-icon"
      viewBox="0 0 24 24"
      fill="currentColor"
      focusable="false"
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
    >
      <path d={d} />
    </svg>
  )
})

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
    ChartComponent,
    TableComponent,
    Markdown,
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

export { catalogCapabilities, DEFINITION_VERSION, EFFECT_KINDS, unsupportedBy } from './capabilities'
