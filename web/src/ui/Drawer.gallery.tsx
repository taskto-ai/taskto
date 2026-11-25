import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Drawer } from './Drawer'
import { Specimen } from './GalleryParts'
import { MenuItem, MoreMenu } from './Menu'

export const title = 'Drawer'

const LINES = Array.from({ length: 12 }, (_, i) => i)

export default function DrawerGallery() {
  const t = useT()
  const [open, setOpen] = useState(false)
  return (
    <Specimen caption={t('gallery.layout.drawer')}>
      <Button onClick={() => setOpen(true)}>{t('gallery.layout.openCard')}</Button>
      <Drawer
        open={open}
        onClose={() => setOpen(false)}
        title={t('gallery.layout.cardTitle')}
        headerActions={
          <MoreMenu>
            <MenuItem icon="copy" onSelect={() => {}}>
              {t('gallery.layout.copyLink')}
            </MenuItem>
            <MenuItem icon="archive" onSelect={() => {}}>
              {t('gallery.sample.archive')}
            </MenuItem>
          </MoreMenu>
        }
        footer={
          <>
            <Button variant="primary">{t('gallery.layout.comment')}</Button>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              {t('gallery.layout.done')}
            </Button>
          </>
        }
      >
        <p style={{ margin: 0 }}>{t('gallery.layout.cardBody')}</p>
        {LINES.map((i) => (
          <p key={i} style={{ margin: 0, color: 'var(--color-text-muted)' }}>
            {t('gallery.layout.threadLine')}
          </p>
        ))}
      </Drawer>
    </Specimen>
  )
}
