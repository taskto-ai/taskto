import { useState } from 'react'
import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Tabs, tabPanelProps } from './Tabs'

export const title = 'Tabs'

export default function TabsGallery() {
  const t = useT()
  const [value, setValue] = useState('members')
  return (
    <>
      <Specimen caption={t('gallery.layout.tabsButtons')}>
        <div style={{ inlineSize: '100%' }}>
          <Tabs
            id="gallery-tabs"
            label={t('gallery.layout.people')}
            value={value}
            onChange={setValue}
            items={[
              { id: 'members', label: t('gallery.layout.members'), count: 12 },
              { id: 'invites', label: t('gallery.layout.invites'), count: 2 },
              { id: 'roles', label: t('gallery.layout.roles') },
            ]}
          />
          <div {...tabPanelProps('gallery-tabs', value)} style={{ paddingBlock: 'var(--space-4)' }}>
            {t('gallery.layout.tabPanel')}
          </div>
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.tabsLinks')}>
        <div style={{ inlineSize: '100%' }}>
          <Tabs
            label={t('gallery.layout.settings')}
            value="general"
            items={[
              { id: 'general', label: t('gallery.layout.general'), href: '#general' },
              { id: 'members', label: t('gallery.layout.members'), href: '#members' },
              { id: 'search', label: t('gallery.layout.searchTab'), href: '#search' },
            ]}
          />
        </div>
      </Specimen>
    </>
  )
}
