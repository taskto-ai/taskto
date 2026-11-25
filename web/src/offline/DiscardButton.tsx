// "Discard" for a change that was never acknowledged (docs/offline.md, "Discarding"): the question
// is about the item as it was when it was asked, its version, its operation and its state, and
// that is what the answer removes. One that changed meanwhile (edited, sent, answered) is kept, and
// the person is told so; one the server acknowledged is never removed by it.

import { useRef } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { ConfirmButton } from './ConfirmButton'
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
  // Captured when the question is opened, never from a later render.
  const asked = useRef<SeenAs | null>(null)
  return (
    <ConfirmButton
      label={t('offline.queue.discard')}
      name={name}
      question={t('offline.queue.discardQuestion')}
      confirm={t('offline.queue.discardConfirm')}
      onOpen={() => {
        asked.current = seenAs(item)
      }}
      onConfirm={() => {
        const seen = asked.current
        if (!seen) return
        discard(runtime, item, seen).then(onDone, (e) => onError(errorText(e)))
      }}
    />
  )
}
