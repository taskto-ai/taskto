import { type Ref, useEffect } from 'react'
import type { AppAccess, AppDefinition } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Panel } from '../ui/Panel'
import { AppExplorer } from './AppExplorer'
import './AppRecords.css'

export const readsRecords = (access: AppAccess) => access.read_records !== 'none'

function scrollBox(el: HTMLElement): HTMLElement | null {
  for (let at = el.parentElement; at; at = at.parentElement) {
    const overflow = getComputedStyle(at).overflowY
    if ((overflow === 'auto' || overflow === 'scroll') && at.scrollHeight > at.clientHeight) return at
  }
  return null
}

export function RecordsButton({
  access,
  open,
  controls,
  onToggle,
  ref,
}: {
  access: AppAccess
  open: boolean
  controls: string
  onToggle: () => void
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
  leaveRef,
}: {
  id: string
  open: boolean
  onClose: () => void
  base: string
  appId: number
  access: AppAccess
  writable: boolean
  definition: AppDefinition | undefined
  release?: number
  onRestored: () => void
  leaveRef?: { current: ((go: () => void) => void) | null }
}) {
  const t = useT()
  useEffect(() => {
    const el = open ? document.getElementById(`${id}-title`) : null
    if (!el) return
    const box = scrollBox(el)
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
        leaveRef={leaveRef}
      />
    </Panel>
  )
}
