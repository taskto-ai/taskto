import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Panel } from './Panel'

export const title = 'Panel'

export default function PanelGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.panels')}>
      <div style={{ display: 'grid', gap: 'var(--space-4)', inlineSize: '100%' }}>
        <Panel padded>{t('gallery.layout.panelPadded')}</Panel>
        <Panel>
          <p style={{ margin: 0, padding: 'var(--space-3) var(--space-4)' }}>{t('gallery.layout.panelPlain')}</p>
        </Panel>
      </div>
    </Specimen>
  )
}
