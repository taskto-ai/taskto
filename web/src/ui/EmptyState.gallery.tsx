import { useT } from '../i18n'
import { Button } from './Button'
import { EmptyState } from './EmptyState'
import { Specimen } from './GalleryParts'
import { Icon } from './Icon'

export const title = 'EmptyState'

export default function EmptyStateGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.empties')}>
      <div style={{ display: 'grid', gap: 'var(--space-4)', inlineSize: '100%' }}>
        <EmptyState
          title={t('gallery.layout.noApps')}
          actions={
            <>
              <Button size="sm">{t('gallery.layout.fromTemplate')}</Button>
              <Button size="sm">{t('gallery.layout.buildApp')}</Button>
            </>
          }
        >
          {t('gallery.layout.noAppsHelp')}
        </EmptyState>
        <EmptyState icon={<Icon name="search" size={24} />} title={t('gallery.layout.noMatch')}>
          {t('gallery.layout.noMatchHelp')}
        </EmptyState>
      </div>
    </Specimen>
  )
}
