import { useT } from '../i18n'
import { Specimen, SpecimenGrid } from './GalleryParts'
import { Skeleton } from './Skeleton'

export const title = 'Skeleton'

export default function SkeletonGallery() {
  const t = useT()
  return (
    <SpecimenGrid>
      <Specimen caption={t('gallery.state.loading')}>
        <div style={{ display: 'grid', gap: 'var(--space-2)', inlineSize: '100%' }}>
          <Skeleton width="70%" />
          <Skeleton width="92%" />
          <Skeleton width="48%" />
        </div>
      </Specimen>
      <Specimen caption={t('gallery.state.loading')}>
        <Skeleton shape="block" />
      </Specimen>
    </SpecimenGrid>
  )
}
