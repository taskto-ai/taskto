import './Spinner.css'

export function Spinner({ size = 'md' }: { size?: 'sm' | 'md' }) {
  return <span className={size === 'sm' ? 'ui-spinner ui-spinner-sm' : 'ui-spinner'} aria-hidden="true" />
}
