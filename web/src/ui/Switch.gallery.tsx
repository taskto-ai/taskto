import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Switch } from './Switch'

export const title = 'Switch'

export default function SwitchGallery() {
  const t = useT()
  return (
    <Specimen
      caption={`${t('gallery.state.default')} · ${t('gallery.state.checked')} · ${t('gallery.state.disabled')}`}
    >
      <Switch label={t('gallery.sample.semantic')} />
      <Switch label={t('gallery.sample.semantic')} defaultChecked />
      <Switch label={t('gallery.sample.push')} disabled />
      <Switch label={t('gallery.sample.push')} disabled defaultChecked />
    </Specimen>
  )
}
