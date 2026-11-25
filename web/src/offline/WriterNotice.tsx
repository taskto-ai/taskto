// What a tab that is not the offline writer says (docs/offline.md, "One writer"): the records and the
// queue are read here, and changes are made in the tab that holds the right, or here once the person
// takes it over.

import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { useWriter } from './hooks'
import type { Runtime } from './runtime'

export function WriterNotice({ runtime }: { runtime: Runtime }) {
  const t = useT()
  const writer = useWriter(runtime)
  const [waiting, setWaiting] = useState(false)
  if (writer) return null
  return (
    <Notice
      tone="info"
      action={
        <Button
          size="sm"
          loading={waiting}
          disabled={waiting}
          onClick={() => {
            setWaiting(true)
            void runtime.writer.takeOver().finally(() => setWaiting(false))
          }}
        >
          {t('offline.writer.takeOver')}
        </Button>
      }
    >
      {waiting ? t('offline.writer.waiting') : t('offline.writer.elsewhere')}
    </Notice>
  )
}
