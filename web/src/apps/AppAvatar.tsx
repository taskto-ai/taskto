import { Avatar, type AvatarProps } from '../ui/Avatar'
import './AppAvatar.css'

export interface AppAvatarProps extends Omit<AvatarProps, 'shape'> {
  icon?: string
  shape?: AvatarProps['shape']
}

export function AppAvatar({ icon, letters, shape = 'square', className, ...rest }: AppAvatarProps) {
  const cls = [icon ? 'apps-avatar-icon' : '', className ?? ''].filter(Boolean).join(' ') || undefined
  return <Avatar {...rest} shape={shape} letters={icon || letters} className={cls} />
}
