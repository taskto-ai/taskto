import { useT } from '../i18n'
import { Checkbox } from './Checkbox'
import { Specimen } from './GalleryParts'

export const title = 'Checkbox'

export default function CheckboxGallery() {
  const t = useT()
  return (
    <Specimen
      caption={`${t('gallery.state.default')} · ${t('gallery.state.checked')} · ${t('gallery.state.mixed')} · ${t('gallery.state.disabled')} · ${t('gallery.state.error')}`}
    >
      <Checkbox label={t('gallery.sample.overdue')} />
      <Checkbox label={t('gallery.sample.overdue')} defaultChecked />
      <Checkbox label={t('gallery.sample.all')} indeterminate />
      <Checkbox label={t('gallery.sample.archived')} disabled />
      <Checkbox label={t('gallery.sample.archived')} disabled defaultChecked />
      <Checkbox label={t('gallery.sample.overdue')} aria-invalid />
    </Specimen>
  )
}
