import { useT } from '../i18n'
import { Avatar, initialsAmong } from './Avatar'
import { Specimen } from './GalleryParts'

export const title = 'Avatar'

export default function AvatarGallery() {
  const t = useT()
  const people = [
    t('gallery.sample.personA'),
    t('gallery.sample.personB'),
    t('gallery.sample.personC'),
    t('gallery.sample.personD'),
  ]
  return (
    <>
      <Specimen caption={t('gallery.state.sizes')}>
        {people.map((p) => (
          <Avatar key={p} name={p} size="sm" />
        ))}
        {people.map((p) => (
          <Avatar key={p} name={p} />
        ))}
        {people.map((p) => (
          <Avatar key={p} name={p} size="lg" />
        ))}
      </Specimen>
      <Specimen caption={t('gallery.state.shapes')}>
        <Avatar name={t('gallery.sample.workspaceName')} shape="square" size="sm" />
        <Avatar name={t('gallery.sample.workspaceName')} shape="square" />
        <Avatar name={t('gallery.sample.workspaceName')} shape="square" size="lg" />
      </Specimen>
      <Specimen caption={t('gallery.state.distinctInitials')}>
        {initialsAmong(people).map((letters, i) => (
          <Avatar key={people[i]} name={people[i]} letters={letters} shape="square" size="sm" />
        ))}
      </Specimen>
    </>
  )
}
