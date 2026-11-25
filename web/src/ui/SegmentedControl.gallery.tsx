import { useState } from 'react'
import { useT } from '../i18n'
import { Specimen } from './GalleryParts'
import { SegmentedControl } from './SegmentedControl'

export const title = 'SegmentedControl'

export default function SegmentedControlGallery() {
  const t = useT()
  const [mode, setMode] = useState<'write' | 'preview'>('write')
  const [view, setView] = useState<'list' | 'board'>('board')
  const modes = [
    { value: 'write' as const, label: t('gallery.sample.write') },
    { value: 'preview' as const, label: t('gallery.sample.preview') },
  ]
  return (
    <>
      <Specimen caption={t('gallery.state.regular')}>
        <SegmentedControl label={t('gallery.sample.view')} value={mode} onChange={setMode} options={modes} />
        <SegmentedControl
          label={t('gallery.sample.view')}
          value={view}
          onChange={setView}
          options={[
            { value: 'list', label: t('gallery.sample.list'), icon: 'filter' },
            { value: 'board', label: t('gallery.sample.board'), icon: 'boards' },
          ]}
        />
      </Specimen>
      <Specimen caption={`${t('gallery.state.small')} · ${t('gallery.state.disabled')}`}>
        <SegmentedControl size="sm" label={t('gallery.sample.view')} value={mode} onChange={setMode} options={modes} />
        <SegmentedControl disabled label={t('gallery.sample.view')} value={mode} onChange={setMode} options={modes} />
      </Specimen>
    </>
  )
}
