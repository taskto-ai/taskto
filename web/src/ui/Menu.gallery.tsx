import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { Menu, MenuButton, MenuItem, MenuLink, MenuList, MenuSeparator, MoreMenu } from './Menu'

export const title = 'Menu'

function Items() {
  const t = useT()
  return (
    <>
      <MenuItem icon="clock" onSelect={() => {}}>
        {t('gallery.layout.history')}
      </MenuItem>
      <MenuItem icon="edit" onSelect={() => {}}>
        {t('gallery.sample.edit')}
      </MenuItem>
      <MenuLink icon="apps" href="/_gallery">
        {t('gallery.layout.openPage')}
      </MenuLink>
      <MenuItem icon="copy" onSelect={() => {}} disabled>
        {t('gallery.layout.duplicateDisabled')}
      </MenuItem>
      <MenuSeparator />
      <MenuItem icon="archive" tone="danger" onSelect={() => {}}>
        {t('gallery.sample.delete')}
      </MenuItem>
    </>
  )
}

export default function MenuGallery() {
  const t = useT()
  return (
    <>
      <Specimen caption={t('gallery.layout.menuClip')}>
        {/* A small box that clips: an open menu is in the top layer, so it is not cut. */}
        <div
          data-gallery="clip"
          style={{
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
            inlineSize: '100%',
            blockSize: '56px',
            overflow: 'hidden',
            padding: 'var(--space-2)',
            border: '1px solid var(--color-border)',
            borderRadius: 'var(--radius-md)',
          }}
        >
          <MoreMenu align="start">
            <Items />
          </MoreMenu>
          <Menu>
            <MenuButton label={t('gallery.layout.sortBy')} variant="secondary" size="sm">
              {t('gallery.layout.newest')}
            </MenuButton>
            <MenuList>
              <MenuItem onSelect={() => {}}>{t('gallery.layout.newest')}</MenuItem>
              <MenuItem onSelect={() => {}}>{t('gallery.layout.oldest')}</MenuItem>
              <MenuItem onSelect={() => {}}>{t('gallery.layout.name')}</MenuItem>
            </MenuList>
          </Menu>
          <MoreMenu>
            <Items />
          </MoreMenu>
        </div>
      </Specimen>
      <Specimen caption={t('gallery.layout.menuItems')}>
        <MoreMenu align="start">
          <Items />
        </MoreMenu>
      </Specimen>
    </>
  )
}
