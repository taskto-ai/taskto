import { useFormat, useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { MenuItem, MoreMenu } from './Menu'
import { Panel } from './Panel'
import { type Column, Table } from './Table'

export const title = 'Table'

interface Member {
  name: string
  role: string
  docs: number
  storage: number
}

export default function TableGallery() {
  const t = useT()
  const format = useFormat()
  const rows: Member[] = [
    { name: t('gallery.sample.personA'), role: t('gallery.sample.admin'), docs: 1284, storage: 1_288_490_189 },
    { name: t('gallery.sample.personB'), role: t('gallery.sample.member'), docs: 37, storage: 50_331_648 },
    { name: t('gallery.sample.personC'), role: t('gallery.sample.member'), docs: 5, storage: 983_040 },
  ]
  const columns: Column<Member>[] = [
    { key: 'name', header: t('gallery.layout.person'), cell: (r) => r.name },
    { key: 'role', header: t('gallery.sample.role'), cell: (r) => r.role },
    { key: 'docs', header: t('gallery.layout.documents'), numeric: true, cell: (r) => format.number(r.docs) },
    { key: 'storage', header: t('gallery.layout.storage'), numeric: true, cell: (r) => format.bytes(r.storage) },
  ]
  const more: Column<Member> = {
    key: 'more',
    header: t('gallery.layout.actions'),
    hiddenHeader: true,
    cell: () => (
      <MoreMenu>
        <MenuItem onSelect={() => {}}>{t('gallery.layout.resetPassword')}</MenuItem>
        <MenuItem tone="danger" onSelect={() => {}}>
          {t('gallery.layout.remove')}
        </MenuItem>
      </MoreMenu>
    ),
  }
  return (
    <Specimen caption={t('gallery.layout.tables')}>
      <div
        style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr)', gap: 'var(--space-4)', inlineSize: '100%' }}
      >
        <Panel>
          <Table
            caption={t('gallery.layout.members')}
            rows={rows}
            rowKey={(r) => r.name}
            columns={[...columns, more]}
          />
        </Panel>
        <div style={{ maxInlineSize: '320px', minInlineSize: 0 }}>
          <Panel>
            <Table
              caption={t('gallery.layout.narrowTable')}
              showCaption
              rows={rows}
              rowKey={(r) => r.name}
              columns={columns}
            />
          </Panel>
        </div>
      </div>
    </Specimen>
  )
}
