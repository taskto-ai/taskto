// The records of one launched app: the data explorer (AppExplorer), opened from the app's header
// (RecordsButton) and shown under the app. Shown to whoever may read the app's records; changing and
// restoring them take the right to change them. The server decides what each person reads and
// changes: this only asks.

import { type Ref, useEffect } from 'react'
import type { AppAccess, AppDefinition } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Panel } from '../ui/Panel'
import { AppExplorer } from './AppExplorer'
import './AppRecords.css'

// Whether the caller may see the app's records at all.
export const readsRecords = (access: AppAccess) => access.read_records !== 'none'

// The nearest box around an element that scrolls up and down.
function scrollBox(el: HTMLElement): HTMLElement | null {
  for (let at = el.parentElement; at; at = at.parentElement) {
    const overflow = getComputedStyle(at).overflowY
    if ((overflow === 'auto' || overflow === 'scroll') && at.scrollHeight > at.clientHeight) return at
  }
  return null
}

// The way to the records, in the app's header: opens the panel, and closes it again.
export function RecordsButton({
  access,
  open,
  controls,
  onToggle,
  ref,
}: {
  access: AppAccess
  open: boolean
  // The id of the panel it opens.
  controls: string
  onToggle: () => void
  // The button, for the panel to give the keyboard back to when it is closed from inside.
  ref?: Ref<HTMLButtonElement>
}) {
  const t = useT()
  if (!readsRecords(access)) return null
  return (
    <Button ref={ref} icon="file" aria-expanded={open} aria-controls={controls} onClick={onToggle}>
      {open ? t('apps.records.close') : t('apps.records.open')}
    </Button>
  )
}

export function AppRecords({
  id,
  open,
  onClose,
  base,
  appId,
  access,
  writable,
  definition,
  release,
  onRestored,
}: {
  // The panel's id, which RecordsButton names.
  id: string
  open: boolean
  onClose: () => void
  // …/spaces/{space}/apps/{app}
  base: string
  appId: number
  access: AppAccess
  // Whether the space lets the caller write; an edit, a delete and a restore are writes.
  writable: boolean
  // The live release's definition, and its number: the fields to show and the actions to edit with.
  definition: AppDefinition | undefined
  release?: number
  // Called after a record was changed, deleted or restored, so the screen shows it again.
  onRestored: () => void
}) {
  const t = useT()
  // Opened from the header, which may be a screen away: the panel is brought into view and the
  // keyboard taken to it. Only the page's own scroll box moves (scrollIntoView would scroll every
  // box around it, the shell's too).
  useEffect(() => {
    const el = open ? document.getElementById(`${id}-title`) : null
    if (!el) return
    const box = scrollBox(el)
    // The panel's top, not the heading's: its padding leaves the heading's focus ring room above it.
    const panel = document.getElementById(id) ?? el
    if (box) box.scrollTop += panel.getBoundingClientRect().top - box.getBoundingClientRect().top
    el.focus({ preventScroll: true })
  }, [open, id])
  if (!open || !readsRecords(access)) return null
  const canChange = writable && access.modify_records !== 'none'
  return (
    <Panel as="section" padded id={id} aria-labelledby={`${id}-title`} className="ui-records">
      <header className="ui-records-head">
        <Heading level={2} id={`${id}-title`} tabIndex={-1} className="ui-records-title">
          {t('apps.records.open')}
        </Heading>
        <IconButton icon="close" label={t('apps.records.close')} onClick={onClose} />
      </header>
      <AppExplorer
        base={base}
        appId={appId}
        definition={definition}
        release={release}
        canChange={canChange}
        canRestore={canChange}
        onChanged={onRestored}
      />
    </Panel>
  )
}
