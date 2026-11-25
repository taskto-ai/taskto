import { Specimen } from './GalleryParts'
import { ICON_NAMES, Icon } from './Icon'
import './Icon.gallery.css'

export const title = 'Icon'

export default function IconGallery() {
  return (
    <>
      <Specimen caption="20">
        {ICON_NAMES.map((name) => (
          <span key={name} className="ui-gallery-icon" title={name}>
            <Icon name={name} />
            <code>{name}</code>
          </span>
        ))}
      </Specimen>
      <Specimen caption="12 · 14 · 16 · 18 · 20 · 24">
        {([12, 14, 16, 18, 20, 24] as const).map((size) => (
          <Icon key={size} name="clock" size={size} />
        ))}
      </Specimen>
    </>
  )
}
