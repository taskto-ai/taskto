import { useT } from '../i18n'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import './Apps.css'
import { placeholders } from '../byPlace'

export function LoadingRows({ count = 3 }: { count?: number }) {
  const t = useT()
  return (
    <div className="apps-loading" role="status">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {placeholders(count).map(({ key }) => (
        <div key={key} className="apps-loading-row">
          <Skeleton shape="block" width="var(--control-md)" height="var(--control-md)" />
          <span className="apps-loading-lines">
            <Skeleton width="40%" />
            <Skeleton width="25%" />
          </span>
        </div>
      ))}
    </div>
  )
}
