import { useRef } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Icon } from './Icon'
import { Menu, MenuButton, MenuItem, MenuList, MoreMenu } from './Menu'
import { PageHeader } from './PageHeader'

export const title = 'PageHeader'

export default function PageHeaderGallery() {
  const t = useT()
  const full = { inlineSize: '100%' }
  const titleRef = useRef<HTMLHeadingElement>(null)
  return (
    <>
      <Specimen caption={t('gallery.layout.headerBasic')}>
        <div style={full}>
          <PageHeader
            level={2}
            title={t('gallery.layout.appsTitle')}
            help={t('gallery.layout.appsHelp')}
            actions={
              <Button variant="primary" icon="plus">
                {t('gallery.layout.newApp')}
              </Button>
            }
          />
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.headerPath')}>
        <div style={full}>
          <PageHeader
            level={2}
            path={
              <>
                {t('gallery.layout.private')} <Icon name="chevron-end" size={14} /> {t('gallery.layout.boards')}
              </>
            }
            title={t('gallery.layout.personal')}
            titleAction={
              <Menu align="start">
                <MenuButton label={t('gallery.layout.switchBoard')} icon="chevron-updown" size="sm" />
                <MenuList>
                  <MenuItem icon="check" onSelect={() => {}}>
                    {t('gallery.layout.personal')}
                  </MenuItem>
                  <MenuItem onSelect={() => {}}>{t('gallery.layout.boards')}</MenuItem>
                </MenuList>
              </Menu>
            }
            actions={
              <>
                <Button icon="filter">{t('gallery.layout.filter')}</Button>
                <Button variant="primary" icon="plus">
                  {t('gallery.sample.newCard')}
                </Button>
                <MoreMenu>
                  <MenuItem icon="edit" onSelect={() => {}}>
                    {t('gallery.layout.renameBoard')}
                  </MenuItem>
                </MoreMenu>
              </>
            }
          />
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.headerLong')}>
        <div style={full}>
          <PageHeader
            level={2}
            title={t('gallery.layout.longTitle')}
            actions={<Button>{t('gallery.sample.edit')}</Button>}
          />
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.focusTarget')}>
        <div style={full}>
          <PageHeader
            level={2}
            title={t('gallery.layout.personal')}
            titleRef={titleRef}
            actions={
              <Button size="sm" onClick={() => titleRef.current?.focus()}>
                {t('gallery.layout.focusIt')}
              </Button>
            }
          />
        </div>
      </Specimen>
    </>
  )
}
