// The records of one launched app: the data explorer (AppExplorer) behind a button. Shown to
// whoever may read the app's records; changing and restoring them take the right to change them.
// The server decides what each person reads and changes: this only asks.

import { useState } from 'react'
import type { AppAccess, AppDefinition } from '../api'
import { useT } from '../i18n'
import { AppExplorer } from './AppExplorer'

export function AppRecords({
  base,
  appId,
  access,
  writable,
  definition,
  release,
  onRestored,
}: {
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
  const [open, setOpen] = useState(false)
  const t = useT()
  if (access.read_records === 'none') return null
  const canChange = writable && access.modify_records !== 'none'
  return (
    <div className="tt-records">
      <button
        type="button"
        className="tt-button"
        aria-expanded={open}
        aria-controls="tt-records-panel"
        onClick={() => setOpen((o) => !o)}
      >
        {open ? t('apps.records.close') : t('apps.records.open')}
      </button>
      {open && (
        <div id="tt-records-panel" className="tt-records-panel">
          <AppExplorer
            base={base}
            appId={appId}
            definition={definition}
            release={release}
            canChange={canChange}
            canRestore={canChange}
            onChanged={onRestored}
          />
        </div>
      )}
    </div>
  )
}
