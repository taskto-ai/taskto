// "Discard" for a change that was never acknowledged (docs/offline.md, "Discarding"): the question
// is about the item as it was when it was asked, its version, its operation and its state, and
// that is what the answer removes. One that changed meanwhile (edited, sent, answered) is kept, and
// the person is told so; one the server acknowledged is never removed by it. The question is the
// library's confirmation dialog, drawn where the button is.

import { useRef } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { discard } from './choices'
import type { Runtime } from './runtime'
import { type OutboxItem, type SeenAs, seenAs } from './store'

export function DiscardButton({
  runtime,
  item,
  name,
  onDone,
  onError,
}: {
  runtime: Runtime
  item: OutboxItem
  name: string
  onDone: () => void
  onError: (message: string) => void
}) {
  const t = useT()
  const [dialog, confirm] = useConfirm()
  // Captured when the question is opened, never from a later render.
  const asked = useRef<SeenAs | null>(null)
  const ask = async () => {
    asked.current = seenAs(item)
    const yes = await confirm({
      title: t('offline.queue.discardQuestion'),
      confirmLabel: t('offline.queue.discardConfirm'),
      tone: 'danger',
    })
    const seen = asked.current
    if (!yes || !seen) return
    discard(runtime, item, seen).then(onDone, (e) => onError(errorText(e)))
  }
  const label = t('offline.queue.discard')
  return (
    <>
      <Button size="sm" variant="danger" aria-label={t('common.labelFor', { label, name })} onClick={() => void ask()}>
        {label}
      </Button>
      {dialog}
    </>
  )
}
