import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { ConfirmDialog, useConfirm } from './ConfirmDialog'
import { Specimen } from './GalleryParts'
import { Notice } from './Notice'
import { toast } from './Toast'

export const title = 'ConfirmDialog'

function BusyThenFails() {
  const t = useT()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [failed, setFailed] = useState(false)
  return (
    <>
      <Button
        onClick={() => {
          setFailed(false)
          setOpen(true)
        }}
      >
        {t('gallery.layout.busyAsk')}
      </Button>
      <ConfirmDialog
        open={open}
        busy={busy}
        title={t('gallery.layout.deleteTitle')}
        confirmLabel={t('gallery.layout.deleteApp')}
        tone="danger"
        onCancel={() => setOpen(false)}
        onConfirm={() => {
          setBusy(true)
          setTimeout(() => {
            setBusy(false)
            setFailed(true)
          }, 3000)
        }}
      >
        {failed ? <Notice tone="danger">{t('gallery.layout.busyFailed')}</Notice> : t('gallery.layout.deleteBody')}
      </ConfirmDialog>
    </>
  )
}

export default function ConfirmDialogGallery() {
  const t = useT()
  const [confirmDialog, confirm] = useConfirm()
  const said = (yes: boolean) => toast(yes ? t('gallery.layout.confirmed') : t('gallery.layout.cancelled'))
  return (
    <Specimen caption={t('gallery.layout.confirms')}>
      <Button
        variant="danger"
        onClick={async () =>
          said(
            await confirm({
              title: t('gallery.layout.deleteTitle'),
              body: t('gallery.layout.deleteBody'),
              confirmLabel: t('gallery.layout.deleteApp'),
              tone: 'danger',
            }),
          )
        }
      >
        {t('gallery.layout.deleteAsk')}
      </Button>
      <Button
        onClick={async () =>
          said(
            await confirm({
              title: t('gallery.layout.publishTitle'),
              body: t('gallery.layout.publishBody'),
              confirmLabel: t('gallery.layout.publish'),
            }),
          )
        }
      >
        {t('gallery.layout.publishAsk')}
      </Button>
      <BusyThenFails />
      {confirmDialog}
    </Specimen>
  )
}
