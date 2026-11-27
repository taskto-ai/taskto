import { useLayoutEffect, useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { discard } from './choices'
import type { Runtime } from './runtime'
import { kindOf, mayHaveBeenSent, type OutboxItem, type SeenAs, seenAs } from './store'

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
  onClosed?: () => void
}) {
  const t = useT()
  const [asked, setAsked] = useState<{ seen: SeenAs; sent: boolean; removal: boolean } | null>(null)
  const open = useRef(false)
  const closed = useRef(onClosed)
  closed.current = onClosed
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
          setAsked({ seen: seenAs(item), sent: mayHaveBeenSent(item), removal: kindOf(item) === 'delete' })
        }}
      >
        {label}
      </Button>
      {asked && (
        <ConfirmDialog
          open
          title={t('offline.queue.discardTitle', { name })}
          confirmLabel={t('offline.queue.discardConfirm')}
          tone="danger"
          onConfirm={() => answer(true)}
          onCancel={() => answer(false)}
        >
          {t(
            !asked.sent
              ? 'offline.queue.discardBody'
              : asked.removal
                ? 'offline.queue.discardBodySentDelete'
                : 'offline.queue.discardBodySent',
          )}
        </ConfirmDialog>
      )}
    </>
  )
}
