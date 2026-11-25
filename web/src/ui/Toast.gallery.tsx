import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { toast } from './Toast'

export const title = 'Toast'

export default function ToastGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.toasts')}>
      <Button onClick={() => toast(t('gallery.layout.linkCopied'))}>{t('gallery.layout.neutral')}</Button>
      <Button onClick={() => toast({ message: t('gallery.layout.appMoved'), tone: 'success' })}>
        {t('gallery.layout.success')}
      </Button>
      <Button onClick={() => toast({ message: t('gallery.layout.moveFailed'), tone: 'danger' })}>
        {t('gallery.layout.danger')}
      </Button>
      <Button
        onClick={() =>
          toast({
            message: t('gallery.layout.cardArchived'),
            action: { label: t('gallery.layout.undo'), onClick: () => toast(t('gallery.layout.cardRestored')) },
          })
        }
      >
        {t('gallery.layout.withAction')}
      </Button>
    </Specimen>
  )
}
