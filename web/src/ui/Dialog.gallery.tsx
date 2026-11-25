import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Dialog } from './Dialog'
import { Field } from './Field'
import { Specimen } from './GalleryParts'
import { Input } from './Input'
import { toast } from './Toast'

export const title = 'Dialog'

export default function DialogGallery() {
  const t = useT()
  const [open, setOpen] = useState(false)
  return (
    <Specimen caption={t('gallery.layout.dialog')}>
      <Button onClick={() => setOpen(true)}>{t('gallery.layout.openDialog')}</Button>
      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        title={t('gallery.layout.renameApp')}
        description={t('gallery.layout.renameHelp')}
        actions={
          <>
            <Button variant="secondary" onClick={() => setOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button variant="primary" onClick={() => setOpen(false)}>
              {t('gallery.sample.save')}
            </Button>
          </>
        }
      >
        <Field label={t('gallery.layout.name')}>
          <Input defaultValue={t('gallery.layout.equipment')} />
        </Field>
        <Button size="sm" variant="ghost" onClick={() => toast(t('gallery.layout.toastOverDialog'))}>
          {t('gallery.layout.showToastOver')}
        </Button>
      </Dialog>
    </Specimen>
  )
}
