// Photos in an app: an input that takes them from the camera or the library and uploads each to
// the server, and the thumbnails that show them. A photo is never an outside address: it is an
// attachment the server keeps, fetched with the viewer's own session (docs/api.md).
//
// The camera is one way in and never the only one. A file input with `capture` opens the camera on
// a phone, and if the person has blocked it (or the device has none) the library's input, which
// has no `capture`, still works. A photo taken this way is sent to the app: the browser may not
// keep a copy in the device's camera roll, so nothing here promises one.

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

// Where the app's attachments are (…/spaces/{space}/apps/{app}), given by the screen that draws
// them; uploads is false where the screen is a preview, whose buttons change nothing.
export type AppFiles = {
  base: string
  uploads: boolean
  // Offline (docs/offline.md): there is no server to upload to. A photo is staged on the device and
  // given a local id, which the record holds until the outbox has uploaded the photo and the
  // server's id has replaced it.
  local?: LocalPhotos
}

export type LocalPhotos = {
  // Keeps the photo in memory under a new id; it is written to the device with the record that holds it.
  stage(file: Blob): string
  // Something an <img> can show for the id, if the photo is here.
  url(id: string): string | undefined
  forget(id: string): void
}

export const AppFilesContext = createContext<AppFiles | null>(null)

// Whether the person has blocked the camera for this site, as far as the browser says: false if
// it doesn't say, since the camera input is harmless then.
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
    } catch {
      // No such permission here (some browsers refuse the name): not knowing is not blocking.
    }
    return () => {
      live = false
      if (status) status.onchange = null
    }
  }, [])
  return blocked
}

// One upload that hasn't finished: on its way, or failed and waiting to be resumed.
type Pending = {
  key: number
  name: string
  file: File
  // Once the server has made the upload: what a retry resumes.
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
  // Called with the whole new list of attachment ids.
  onChange: (ids: string[]) => void
}) {
  const t = useT()
  const f = useFormat()
  const [pending, setPending] = useState<Pending[]>([])
  const [problem, setProblem] = useState<string | null>(null)
  const cameraBlocked = useCameraBlocked()
  const hintId = useId()
  // Uploads finish at different times: they add to the list as it is, not as it was drawn.
  const current = useRef(value)
  current.current = value
  // The uploads of this screen that the person then removed, or cancelled: they stop.
  const stopped = useRef(new Set<number>())
  // The photos uploaded here and not yet in a record: removing one discards it on the server too.
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
        // Nothing to send now: the photo goes with the record, when the outbox sends it.
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
        {/* The camera: where the browser has one, this opens it; where it hasn't, or the person has
            blocked it, the library's input below does the same job. A file input has no component of
            its own: it is kept, unseen over its label, for the keyboard and screen readers, and the
            label is what is seen, drawn as a button. */}
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

// The thumbnails of attachments, each opening its picture in a tab of its own.
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
  // Offline, only the photos this device holds can be shown: the others are on the server.
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
