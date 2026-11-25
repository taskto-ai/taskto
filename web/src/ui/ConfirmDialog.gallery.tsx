import { useT } from '../i18n'
import { Button } from './Button'
import { useConfirm } from './ConfirmDialog'
import { Specimen } from './GalleryParts'
import { toast } from './Toast'

export const title = 'ConfirmDialog'

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
      {confirmDialog}
    </Specimen>
  )
}
