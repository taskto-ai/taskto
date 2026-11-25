import { useState } from 'react'
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
    { key: 'name', header: t('gallery.layout.person'), cell: (r) => r.name },
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
        <Panel>
          <SortedTable rows={rows} columns={columns} />
        </Panel>
      </div>
    </Specimen>
  )
}

// Sortable columns: a header is a button, and its cell says the order the rows are in.
function SortedTable({ rows, columns }: { rows: Member[]; columns: Column<Member>[] }) {
  const t = useT()
  const [by, setBy] = useState<{ key: string; dir: 'ascending' | 'descending' }>({ key: 'docs', dir: 'descending' })
  const sorted = [...rows].sort((a, b) => {
    const x = a[by.key as keyof Member]
    const y = b[by.key as keyof Member]
    const order = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
    return by.dir === 'ascending' ? order : -order
  })
  return (
    <Table
      caption={t('gallery.layout.sortableTable')}
      showCaption
      rows={sorted}
      rowKey={(r) => r.name}
      columns={columns.map((c) => ({
        ...c,
        sort: {
          direction: by.key === c.key ? by.dir : 'none',
          onSort: () =>
            setBy((cur) =>
              cur.key === c.key
                ? { key: c.key, dir: cur.dir === 'ascending' ? 'descending' : 'ascending' }
                : { key: c.key, dir: 'ascending' },
            ),
        },
      }))}
    />
  )
}
