import { useState } from 'react'
import { useT } from '../i18n'
import { Button, ButtonLink } from './Button'
import { Specimen } from './GalleryParts'
import { IconButton } from './IconButton'
import { Input } from './Input'
import { SegmentedControl } from './SegmentedControl'
import { Select } from './Select'

export const title = 'Button'

export default function ButtonGallery() {
  const t = useT()
  const [view, setView] = useState<'list' | 'board'>('list')
  return (
    <>
      <Specimen caption={t('gallery.state.regular')}>
        <Button variant="primary" icon="plus">
          {t('gallery.sample.primary')}
        </Button>
        <Button>{t('gallery.sample.secondary')}</Button>
        <Button variant="ghost">{t('gallery.sample.ghost')}</Button>
        <Button variant="danger">{t('gallery.sample.delete')}</Button>
      </Specimen>
      <Specimen caption={t('gallery.state.small')}>
        <Button variant="primary" size="sm" icon="plus">
          {t('gallery.sample.primary')}
        </Button>
        <Button size="sm">{t('gallery.sample.secondary')}</Button>
        <Button variant="ghost" size="sm">
          {t('gallery.sample.ghost')}
        </Button>
        <Button variant="danger" size="sm">
          {t('gallery.sample.delete')}
        </Button>
      </Specimen>
      <Specimen caption={t('gallery.state.disabled')}>
        <Button variant="primary" disabled>
          {t('gallery.sample.primary')}
        </Button>
        <Button disabled>{t('gallery.sample.secondary')}</Button>
        <Button variant="ghost" disabled>
          {t('gallery.sample.ghost')}
        </Button>
        <Button variant="danger" disabled>
          {t('gallery.sample.delete')}
        </Button>
      </Specimen>
      <Specimen caption={t('gallery.state.loading')}>
        <Button variant="primary" loading>
          {t('gallery.sample.save')}
        </Button>
        <Button loading>{t('gallery.sample.save')}</Button>
        <Button size="sm" loading>
          {t('gallery.sample.save')}
        </Button>
      </Specimen>
      <Specimen caption={t('gallery.state.asLink')}>
        <ButtonLink href="/_gallery" icon="boards">
          {t('gallery.sample.open')}
        </ButtonLink>
        <ButtonLink href="/_gallery" variant="ghost" size="sm" icon="chevron-end">
          {t('gallery.sample.open')}
        </ButtonLink>
      </Specimen>
      <Specimen caption={t('gallery.state.inARow')}>
        <Input
          aria-label={t('gallery.sample.search')}
          placeholder={t('gallery.sample.search')}
          style={{ inlineSize: 'auto' }}
        />
        <Select aria-label={t('gallery.sample.role')} style={{ inlineSize: 'auto' }}>
          <option>{t('gallery.sample.member')}</option>
        </Select>
        <SegmentedControl
          label={t('gallery.sample.view')}
          value={view}
          onChange={setView}
          options={[
            { value: 'list', label: t('gallery.sample.list') },
            { value: 'board', label: t('gallery.sample.board') },
          ]}
        />
        <Button>{t('gallery.sample.secondary')}</Button>
        <Button variant="primary">{t('gallery.sample.primary')}</Button>
        <IconButton icon="more" label={t('gallery.sample.more')} />
      </Specimen>
      <Specimen caption={t('gallery.state.inARow')}>
        <Input
          size="sm"
          aria-label={t('gallery.sample.search')}
          placeholder={t('gallery.sample.search')}
          style={{ inlineSize: 'auto' }}
        />
        <Select size="sm" aria-label={t('gallery.sample.role')} style={{ inlineSize: 'auto' }}>
          <option>{t('gallery.sample.member')}</option>
        </Select>
        <SegmentedControl
          size="sm"
          label={t('gallery.sample.view')}
          value={view}
          onChange={setView}
          options={[
            { value: 'list', label: t('gallery.sample.list') },
            { value: 'board', label: t('gallery.sample.board') },
          ]}
        />
        <Button size="sm">{t('gallery.sample.secondary')}</Button>
        <Button size="sm" variant="primary">
          {t('gallery.sample.primary')}
        </Button>
        <IconButton size="sm" icon="more" label={t('gallery.sample.more')} />
      </Specimen>
    </>
  )
}
