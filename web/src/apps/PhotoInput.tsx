import { createContext, useContext, useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Button, buttonClass } from '../ui/Button'
import { Icon } from '../ui/Icon'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import {
  DEFAULT_LIMITS,
  discardPhoto,
  PHOTO_TYPES,
  type PhotoLimits,
  photoUrl,
  thumbnailUrl,
  UploadCancelled,
  uploadPhoto,
} from './photos'
import './capture.css'

export type AppFiles = {
  base: string
  uploads: boolean
  local?: LocalPhotos
}

export type LocalPhotos = {
  stage(file: Blob): string
  url(id: string): string | undefined
  forget(id: string): void
}

export const AppFilesContext = createContext<AppFiles | null>(null)

export function useCameraBlocked(): boolean {
  const [blocked, setBlocked] = useState(false)
  useEffect(() => {
    let live = true
    let status: PermissionStatus | undefined
    try {
      navigator.permissions
        ?.query({ name: 'camera' as PermissionName })
        .then((s) => {
          if (!live) return
          status = s
          setBlocked(s.state === 'denied')
          s.onchange = () => setBlocked(s.state === 'denied')
        })
        .catch(() => {})
    } catch {}
    return () => {
      live = false
      if (status) status.onchange = null
    }
  }, [])
  return blocked
}

type Pending = {
  key: number
  name: string
  file: File
  id?: string
  sent: number
  failed?: string
}

let keys = 0

export function PhotoField({
  label,
  files,
  field,
  value,
  limits = DEFAULT_LIMITS,
  onChange,
}: {
  label?: string
  files: AppFiles | null
  field: string
  value: string[]
  limits?: PhotoLimits
  onChange: (ids: string[]) => void
}) {
  const t = useT()
  const f = useFormat()
  const [pending, setPending] = useState<Pending[]>([])
  const [problem, setProblem] = useState<string | null>(null)
  const cameraBlocked = useCameraBlocked()
  const hintId = useId()
  const current = useRef(value)
  current.current = value
  const stopped = useRef(new Set<number>())
  const mine = useRef(new Set<string>())

  const uploads = !!files?.uploads
  const full = value.length + pending.filter((p) => !p.failed).length >= limits.max_files
  const canAdd = uploads && !full

  const patch = (key: number, change: Partial<Pending>) =>
    setPending((all) => all.map((p) => (p.key === key ? { ...p, ...change } : p)))

  const run = async (item: Pending) => {
    if (!files) return
    patch(item.key, { failed: undefined })
    try {
      const done = await uploadPhoto(files.base, field, item.file, {
        resume: item.id,
        onStarted: (id) => patch(item.key, { id }),
        onProgress: (sent) => patch(item.key, { sent }),
        cancelled: () => stopped.current.has(item.key),
      })
      mine.current.add(done.id)
      current.current = [...current.current, done.id]
      onChange(current.current)
      setPending((all) => all.filter((p) => p.key !== item.key))
    } catch (e) {
      if (e instanceof UploadCancelled) return
      patch(item.key, { failed: errorText(e) })
    }
  }

  const add = (picked: FileList | null) => {
    setProblem(null)
    const room = limits.max_files - value.length - pending.filter((p) => !p.failed).length
    const chosen = [...(picked ?? [])]
    if (chosen.length > room) setProblem(t('apps.photos.limit', { count: limits.max_files }))
    for (const file of chosen.slice(0, Math.max(0, room))) {
      if (file.type && !PHOTO_TYPES.includes(file.type)) {
        setProblem(t('apps.photos.notPicture', { name: file.name }))
        continue
      }
      if (file.size > limits.max_bytes) {
        setProblem(t('apps.photos.tooLarge', { name: file.name, max: f.bytes(limits.max_bytes) }))
        continue
      }
      if (file.size === 0) {
        setProblem(t('apps.photos.notPicture', { name: file.name }))
        continue
      }
      if (files?.local) {
        current.current = [...current.current, files.local.stage(file)]
        onChange(current.current)
        continue
      }
      const item: Pending = { key: ++keys, name: file.name || t('apps.photos.unnamed'), file, sent: 0 }
      setPending((all) => [...all, item])
      void run(item)
    }
  }

  const remove = (id: string) => {
    current.current = current.current.filter((x) => x !== id)
    onChange(current.current)
    if (files?.local) files.local.forget(id)
    else if (files && mine.current.delete(id)) void discardPhoto(files.base, id)
  }

  const cancel = (item: Pending) => {
    stopped.current.add(item.key)
    setPending((all) => all.filter((p) => p.key !== item.key))
    if (files && item.id) void discardPhoto(files.base, item.id)
  }

  const heading = label ?? t('apps.photos.label')
  return (
    <fieldset className="ui-capture" aria-describedby={hintId}>
      <legend className="ui-capture-label">{heading}</legend>
      {files && value.length > 0 && (
        <ul className="ui-photo-grid">
          {value.map((id, i) => (
            <li key={id} className="ui-photo">
              <img
                src={files.local ? files.local.url(id) : thumbnailUrl(files.base, id)}
                alt={t('apps.photos.photoN', { n: i + 1, total: value.length })}
                loading="lazy"
              />
              <span className="ui-photo-remove">
                <IconButton
                  size="sm"
                  icon="close"
                  label={t('apps.photos.removeNamed', { n: i + 1 })}
                  onClick={() => remove(id)}
                />
              </span>
            </li>
          ))}
        </ul>
      )}
      {pending.length > 0 && (
        <ul className="ui-photo-uploads">
          {pending.map((p) => (
            <li key={p.key} className="ui-photo-upload" data-failed={p.failed ? true : undefined}>
              <span className="ui-photo-name">{p.name}</span>
              {p.failed ? (
                <>
                  <span role="alert" className="ui-photo-failed">
                    <Icon name="alert" size={14} />
                    {t('apps.photos.failed', { name: p.name, error: p.failed })}
                  </span>
                  <Button
                    size="sm"
                    aria-label={t('apps.photos.retryNamed', { name: p.name })}
                    onClick={() => void run(p)}
                  >
                    {t('apps.photos.retry')}
                  </Button>
                </>
              ) : (
                <progress
                  className="ui-photo-progress"
                  max={p.file.size}
                  value={p.sent}
                  aria-label={t('apps.photos.progressNamed', { name: p.name })}
                />
              )}
              <IconButton
                size="sm"
                icon="close"
                label={t('apps.photos.cancelNamed', { name: p.name })}
                onClick={() => cancel(p)}
              />
            </li>
          ))}
        </ul>
      )}
      <div className="ui-photo-add">
        <label className={buttonClass()} data-disabled={!canAdd || cameraBlocked || undefined}>
          <Icon name="upload" size={16} />
          {t('apps.photos.take')}
          <input
            type="file"
            className="ui-photo-file"
            accept={PHOTO_TYPES.join(',')}
            capture="environment"
            disabled={!canAdd || cameraBlocked}
            onChange={(e) => {
              add(e.target.files)
              e.target.value = ''
            }}
          />
        </label>
        <label className={buttonClass()} data-disabled={!canAdd || undefined}>
          <Icon name="folder" size={16} />
          {t('apps.photos.choose')}
          <input
            type="file"
            className="ui-photo-file"
            accept={PHOTO_TYPES.join(',')}
            multiple={limits.max_files > 1}
            disabled={!canAdd}
            onChange={(e) => {
              add(e.target.files)
              e.target.value = ''
            }}
          />
        </label>
      </div>
      <p id={hintId} className="ui-capture-note">
        {!uploads
          ? t(files ? 'apps.photos.previewOnly' : 'apps.photos.notHere')
          : cameraBlocked
            ? t('apps.photos.cameraBlocked')
            : t('apps.photos.cameraNote')}{' '}
        {t('apps.photos.limit', { count: limits.max_files })}{' '}
        {t('apps.photos.maxSize', { max: f.bytes(limits.max_bytes) })}
      </p>
      {problem && <Notice tone="danger">{problem}</Notice>}
    </fieldset>
  )
}

export function PhotoStrip({
  base,
  ids,
  label,
  local,
}: {
  base: string
  ids: string[]
  label?: string
  local?: LocalPhotos
}) {
  const t = useT()
  if (local) ids = ids.filter((id) => local.url(id) !== undefined)
  if (ids.length === 0) return null
  return (
    <figure className="ui-photos" aria-label={label ?? t('apps.photos.label')}>
      <ul className="ui-photo-grid">
        {ids.map((id, i) => (
          <li key={id} className="ui-photo">
            <a
              className="ui-capture-link"
              href={local ? local.url(id) : photoUrl(base, id)}
              target="_blank"
              rel="noopener noreferrer"
              aria-label={t('apps.photos.openN', { n: i + 1, total: ids.length })}
            >
              <img
                src={local ? local.url(id) : thumbnailUrl(base, id)}
                alt={t('apps.photos.photoN', { n: i + 1, total: ids.length })}
                loading="lazy"
              />
            </a>
          </li>
        ))}
      </ul>
    </figure>
  )
}

export function useAppFiles(): AppFiles | null {
  return useContext(AppFilesContext)
}
