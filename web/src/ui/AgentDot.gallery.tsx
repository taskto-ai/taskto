import { useT } from '../i18n'
import { AgentDot } from './AgentDot'
import { Specimen } from './GalleryParts'

export const title = 'AgentDot'

export default function AgentDotGallery() {
  const t = useT()
  return (
    <>
      <Specimen caption={t('gallery.state.default')}>
        <AgentDot />
        <span>{t('gallery.sample.agentSaid')}</span>
      </Specimen>
      <Specimen caption={t('gallery.state.ringed')}>
        <AgentDot ringed label={t('gallery.sample.unread')} />
      </Specimen>
    </>
  )
}
