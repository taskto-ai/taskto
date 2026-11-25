import { useT } from '../i18n'
import { Badge } from './Badge'
import { Specimen } from './GalleryParts'

export const title = 'Badge'

export default function BadgeGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.state.tones')}>
      <Badge tone="success" dot>
        {t('gallery.sample.live')}
      </Badge>
      <Badge tone="warning" dot>
        {t('gallery.sample.draft')}
      </Badge>
      <Badge tone="danger" icon="clock">
        {t('gallery.sample.overdueBadge')}
      </Badge>
      <Badge tone="info" dot>
        {t('gallery.sample.syncing')}
      </Badge>
      <Badge icon="offline">{t('gallery.sample.offline')}</Badge>
    </Specimen>
  )
}
