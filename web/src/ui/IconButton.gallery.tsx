import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { IconButton } from './IconButton'

export const title = 'IconButton'

export default function IconButtonGallery() {
  const t = useT()
  return (
    <>
      <Specimen caption={t('gallery.state.regular')}>
        <IconButton icon="edit" label={t('gallery.sample.edit')} />
        <IconButton icon="more" label={t('gallery.sample.more')} />
        <IconButton icon="close" label={t('gallery.sample.close')} />
        <IconButton icon="pin" label={t('gallery.sample.pin')} aria-pressed />
        <IconButton icon="archive" label={t('gallery.sample.archive')} disabled />
      </Specimen>
      <Specimen caption={t('gallery.state.small')}>
        <IconButton size="sm" icon="edit" label={t('gallery.sample.edit')} />
        <IconButton size="sm" icon="more" label={t('gallery.sample.more')} />
        <IconButton size="sm" icon="plus" label={t('gallery.sample.newCard')} />
        <IconButton size="sm" icon="pin" label={t('gallery.sample.pin')} aria-pressed />
        <IconButton size="sm" icon="archive" label={t('gallery.sample.archive')} disabled />
      </Specimen>
    </>
  )
}
