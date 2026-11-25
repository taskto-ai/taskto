import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Page } from './Page'
import { PageHeader } from './PageHeader'

export const title = 'Page'

const WIDTHS = ['reading', 'list', 'full'] as const

export default function PageGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.widths')}>
      <div style={{ display: 'grid', gap: 'var(--space-4)', inlineSize: '100%' }}>
        {WIDTHS.map((width) => (
          <div key={width} style={{ blockSize: '160px', border: '1px dashed var(--color-border-strong)' }}>
            <Page width={width}>
              <PageHeader level={2} title={width} help={t('gallery.layout.widthHelp')} />
              <div style={{ blockSize: '240px', background: 'var(--color-bg-sunken)' }} />
            </Page>
          </div>
        ))}
      </div>
    </Specimen>
  )
}
