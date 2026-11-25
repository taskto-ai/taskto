// What the apps' screens show while they load: the shape of what is coming, not a word.

import { useT } from '../i18n'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import './Apps.css'

// count rows of a list, each a square, a title and a line of detail.
export function LoadingRows({ count = 3 }: { count?: number }) {
  const t = useT()
  return (
    <div className="apps-loading" role="status">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {Array.from({ length: count }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholders with nothing to tell them apart
        <div key={i} className="apps-loading-row">
          <Skeleton shape="block" width="2.5rem" height="2.5rem" />
          <span className="apps-loading-lines">
            <Skeleton width="40%" />
            <Skeleton width="25%" />
          </span>
        </div>
      ))}
    </div>
  )
}
