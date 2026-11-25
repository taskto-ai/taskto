import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Tooltip } from './Tooltip'

export const title = 'Tooltip'

export default function TooltipGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.state.default')}>
      <Tooltip content={t('gallery.sample.tooltipText')}>
        <Button icon="copy">{t('gallery.sample.tooltipButton')}</Button>
      </Tooltip>
    </Specimen>
  )
}
