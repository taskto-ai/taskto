import { useT } from '../i18n'
import { Badge } from './Badge'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Icon } from './Icon'
import { MenuItem, MenuSeparator, MoreMenu } from './Menu'
import { Panel } from './Panel'
import { Row, Rows } from './Rows'

export const title = 'Rows'

function More() {
  const t = useT()
  return (
    <MoreMenu>
      <MenuItem icon="clock" onSelect={() => {}}>
        {t('gallery.layout.history')}
      </MenuItem>
      <MenuItem icon="edit" onSelect={() => {}}>
        {t('gallery.sample.edit')}
      </MenuItem>
      <MenuItem icon="copy" onSelect={() => {}}>
        {t('gallery.layout.duplicate')}
      </MenuItem>
      <MenuItem icon="move" onSelect={() => {}}>
        {t('gallery.layout.moveTo')}
      </MenuItem>
      <MenuSeparator />
      <MenuItem icon="archive" tone="danger" onSelect={() => {}}>
        {t('gallery.sample.delete')}
      </MenuItem>
    </MoreMenu>
  )
}

export default function RowsGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.rows')}>
      <div style={{ inlineSize: '100%' }}>
        <Panel>
          <Rows label={t('gallery.layout.appsTitle')}>
            <Row
              leading={<Icon name="apps" />}
              title={t('gallery.layout.equipment')}
              detail={t('gallery.layout.equipmentDetail')}
              status={
                <Badge tone="success" dot>
                  {t('gallery.layout.liveRelease')}
                </Badge>
              }
              actions={
                <>
                  <Button size="sm" variant="ghost">
                    {t('gallery.layout.editWithAi')}
                  </Button>
                  <Button size="sm">{t('gallery.layout.openApp')}</Button>
                  <More />
                </>
              }
            />
            <Row
              leading={<Icon name="apps" />}
              title={t('gallery.layout.expenses')}
              detail={t('gallery.layout.expensesDetail')}
              status={
                <Badge tone="warning" dot>
                  {t('gallery.sample.draft')}
                </Badge>
              }
              href="#expenses"
              actions={<More />}
            />
            <Row
              leading={<Icon name="search" />}
              title={t('gallery.layout.inPlaceRow')}
              detail={t('gallery.layout.inPlaceDetail')}
              onOpen={() => {}}
            />
            <Row
              leading={<Icon name="file" />}
              title={t('gallery.layout.currentRow')}
              detail={t('gallery.layout.currentDetail')}
              current
            />
          </Rows>
        </Panel>
      </div>
    </Specimen>
  )
}
