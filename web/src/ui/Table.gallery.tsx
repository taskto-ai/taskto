import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { MenuItem, MoreMenu } from './Menu'
import { Panel } from './Panel'
import { type Column, Table } from './Table'

export const title = 'Table'

interface Member {
  name: string
  role: string
  docs: number
  storage: string
}

export default function TableGallery() {
  const t = useT()
  const rows: Member[] = [
    { name: t('gallery.sample.personA'), role: t('gallery.sample.admin'), docs: 1284, storage: '1.2 GB' },
    { name: t('gallery.sample.personB'), role: t('gallery.sample.member'), docs: 37, storage: '48 MB' },
    { name: t('gallery.sample.personC'), role: t('gallery.sample.member'), docs: 5, storage: '960 kB' },
  ]
  const columns: Column<Member>[] = [
    { key: 'name', header: t('gallery.layout.person'), rowHeader: true, cell: (r) => r.name },
    { key: 'role', header: t('gallery.sample.role'), cell: (r) => r.role },
    { key: 'docs', header: t('gallery.layout.documents'), numeric: true, cell: (r) => r.docs },
    { key: 'storage', header: t('gallery.layout.storage'), numeric: true, cell: (r) => r.storage },
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
      <div style={{ display: 'grid', gap: 'var(--space-4)', inlineSize: '100%' }}>
        <Panel>
          <Table
            caption={t('gallery.layout.members')}
            rows={rows}
            rowKey={(r) => r.name}
            columns={[...columns, more]}
          />
        </Panel>
        <div style={{ maxInlineSize: '320px' }}>
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
