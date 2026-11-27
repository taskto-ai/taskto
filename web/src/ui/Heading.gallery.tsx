import { useRef } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Heading, type HeadingSize } from './Heading'

export const title = 'Heading'

const SIZES: HeadingSize[] = ['display', 'title', 'section', 'subsection']

export default function HeadingGallery() {
  const t = useT()
  const target = useRef<HTMLHeadingElement>(null)
  return (
    <>
      <Specimen caption={t('gallery.layout.headings')}>
        <div style={{ display: 'grid', gap: 'var(--space-3)' }}>
          {SIZES.map((size) => (
            <Heading key={size} level={3} size={size}>
              {`${size} · ${t('gallery.typeSection')}`}
            </Heading>
          ))}
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.focusTarget')}>
        <div style={{ display: 'grid', gap: 'var(--space-3)', justifyItems: 'start' }}>
          <Heading level={3} ref={target} tabIndex={-1}>
            {t('gallery.typeSection')}
          </Heading>
          <Button size="sm" onClick={() => target.current?.focus()}>
            {t('gallery.layout.focusIt')}
          </Button>
        </div>
      </Specimen>
    </>
  )
}
