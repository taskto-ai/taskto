// A ring that turns while something is under way, in the text colour. It says nothing to a screen
// reader: what holds it says that it is busy (a button's "Loading", aria-busy).

import './Spinner.css'

export function Spinner({ size = 'md' }: { size?: 'sm' | 'md' }) {
  return <span className={size === 'sm' ? 'ui-spinner ui-spinner-sm' : 'ui-spinner'} aria-hidden="true" />
}
