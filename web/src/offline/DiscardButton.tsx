// "Discard" for a change that was never acknowledged (docs/offline.md, "Discarding"): the question
// is about the item as it was when it was asked, its version, its operation and its state, and
// that is what the answer removes. One that changed meanwhile (edited, sent, answered) is kept, and
// the person is told so; one the server acknowledged is never removed by it. The question is the
// library's confirmation dialog, drawn where the button is.

import { useLayoutEffect, useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { discard } from './choices'
import type { Runtime } from './runtime'
import { mayHaveBeenSent, type OutboxItem, type SeenAs, seenAs } from './store'

export function DiscardButton({
  runtime,
  item,
  name,
  onDone,
  onError,
  onClosed,
}: {
  runtime: Runtime
  item: OutboxItem
  name: string
  onDone: () => void
  onError: (message: string) => void
  // The question was open when this button went away: its row stopped offering a discard because
  // the record started sending, or because this tab is not the writer any more. Nothing was removed.
  onClosed?: () => void
}) {
  const t = useT()
  // The item as it was when the question was opened, never from a later render: set while the
  // question is on screen, and null otherwise.
  // `sent`: a send was started for it before (a claim, or one nobody answered): the server may
  // already have the record, which the question says.
  const [asked, setAsked] = useState<{ seen: SeenAs; sent: boolean } | null>(null)
  // What to say if the question is taken away from under the person (this button unmounts while it
  // is open: it is not answered, it is gone). Set by the person's own presses, never from a render:
  // an answer and a list read again can land in one render, and the answer must already count.
  const open = useRef(false)
  const closed = useRef(onClosed)
  closed.current = onClosed
  // A layout effect, because it must run before the dialog's own teardown, which cancels the
  // question (and would make it look answered).
  useLayoutEffect(
    () => () => {
      if (open.current) closed.current?.()
    },
    [],
  )
  const answer = (yes: boolean) => {
    const seen = asked?.seen
    open.current = false
    setAsked(null)
    if (!yes || !seen) return
    discard(runtime, item, seen).then(onDone, (e) => onError(errorText(e)))
  }
  const label = t('offline.queue.discard')
  return (
    <>
      <Button
        size="sm"
        variant="danger"
        aria-label={t('common.labelFor', { label, name })}
        onClick={() => {
          open.current = true
          setAsked({ seen: seenAs(item), sent: mayHaveBeenSent(item) })
        }}
      >
        {label}
      </Button>
      {/* Named: the page under the question has several rows like it. */}
      {asked && (
        <ConfirmDialog
          open
          title={t('offline.queue.discardTitle', { name })}
          confirmLabel={t('offline.queue.discardConfirm')}
          tone="danger"
          onConfirm={() => answer(true)}
          onCancel={() => answer(false)}
        >
          {t(asked.sent ? 'offline.queue.discardBodySent' : 'offline.queue.discardBody')}
        </ConfirmDialog>
      )}
    </>
  )
}
