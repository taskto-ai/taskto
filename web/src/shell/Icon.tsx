// The shell's icons are the design system's (../ui/Icon.tsx). This path stays for the screens that
// import it, with the class they style (.tt-icon).

import { type IconProps, Icon as UiIcon } from '../ui/Icon'

export type { IconName, IconProps } from '../ui/Icon'

export function Icon(props: IconProps) {
  return <UiIcon {...props} className={['tt-icon', props.className].filter(Boolean).join(' ')} />
}
