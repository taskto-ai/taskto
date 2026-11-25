import { useT } from '../i18n'
import { Button } from './Button'
import { Specimen } from './GalleryParts'
import { Notice } from './Notice'

export const title = 'Notice'

export default function NoticeGallery() {
  const t = useT()
  return (
    <Specimen caption={t('gallery.layout.notices')}>
      <div style={{ display: 'grid', gap: 'var(--space-3)', inlineSize: '100%' }}>
        <Notice tone="info">{t('gallery.layout.noticeInfo')}</Notice>
        <Notice tone="success" title={t('gallery.layout.noticeSuccessTitle')}>
          {t('gallery.layout.noticeSuccess')}
        </Notice>
        <Notice tone="warning" action={<Button size="sm">{t('gallery.layout.sendNow')}</Button>}>
          {t('gallery.layout.noticeWarning')}
        </Notice>
        <Notice tone="danger">{t('gallery.layout.noticeDanger')}</Notice>
        <Notice tone="neutral">
          {t('gallery.layout.noticeNeutral')} <a href="#email">{t('gallery.layout.howToSetUp')}</a>
        </Notice>
      </div>
    </Specimen>
  )
}
