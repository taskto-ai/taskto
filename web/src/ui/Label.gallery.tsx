import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Label } from './Label'

export const title = 'Label'

const NAMES = ['onboarding', 'writing', 'copy', 'blocked', 'launch', 'design', 'i18n', 'bug', 'urgent'] as const

export default function LabelGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.state.hues')}>
      {NAMES.map((n) => (
        <Label key={n} name={n}>
          {t(`gallery.labels.${n}`)}
        </Label>
      ))}
    </Specimen>
  )
}
