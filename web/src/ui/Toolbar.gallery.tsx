import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Toolbar, ToolbarSpacer } from './Toolbar'

export const title = 'Toolbar'

export default function ToolbarGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.toolbar')}>
      <div style={{ inlineSize: '100%' }}>
        <Toolbar label={t('gallery.layout.filters')}>
          <Button size="sm" icon="filter">
            {t('gallery.layout.labels')}
          </Button>
          <Button size="sm" variant="ghost">
            {t('gallery.sample.overdue')}
          </Button>
          <Button size="sm" variant="ghost">
            {t('gallery.layout.assignedToMe')}
          </Button>
          <ToolbarSpacer />
          <Button size="sm" icon="plus">
            {t('gallery.layout.addColumn')}
          </Button>
        </Toolbar>
      </div>
    </Specimen>
  )
}
