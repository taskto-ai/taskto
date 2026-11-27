import type { AnchorHTMLAttributes, ComponentPropsWithRef, MouseEvent, ReactNode } from 'react'
import { useT } from '../i18n'
import { Link } from '../router'
import { Icon, type IconName } from './Icon'
import { Spinner } from './Spinner'
import { VisuallyHidden } from './VisuallyHidden'
import './Button.css'

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type ButtonSize = 'md' | 'sm'

const VARIANT_CLASS = {
  primary: 'ui-button-primary',
  secondary: 'ui-button-secondary',
  ghost: 'ui-button-ghost',
  danger: 'ui-button-danger',
} as const

interface Look {
  variant?: ButtonVariant
  size?: ButtonSize
  icon?: IconName
}

export function buttonClass({ variant = 'secondary', size = 'md', className }: Look & { className?: string } = {}) {
  return ['ui-control', 'ui-button', VARIANT_CLASS[variant], size === 'sm' ? 'ui-button-sm' : '', className ?? '']
    .filter(Boolean)
    .join(' ')
}

function Content({ icon, size, children }: { icon?: IconName; size: ButtonSize; children?: ReactNode }) {
  return (
    <span className="ui-button-content">
      {icon && <Icon name={icon} size={size === 'sm' ? 16 : 18} />}
      {children !== undefined && children !== null && <span className="ui-button-label">{children}</span>}
    </span>
  )
}

export type ButtonProps = ComponentPropsWithRef<'button'> &
  Look & {
    loading?: boolean
  }

export function Button({
  variant,
  size = 'md',
  icon,
  loading = false,
  type = 'button',
  className,
  onClick,
  children,
  ...rest
}: ButtonProps) {
  const t = useT()
  const click = (e: MouseEvent<HTMLButtonElement>) => {
    if (loading) {
      e.preventDefault()
      return
    }
    onClick?.(e)
  }
  return (
    <button
      type={type}
      className={buttonClass({ variant, size, className })}
      onClick={click}
      {...rest}
      {...(loading ? { 'aria-busy': true, 'aria-disabled': true } : {})}
    >
      <Content icon={icon} size={size}>
        {children}
      </Content>
      {loading && (
        <span className="ui-button-busy">
          <Spinner size={size === 'sm' ? 'sm' : 'md'} />
          <VisuallyHidden>{t('ui.loading')}</VisuallyHidden>
        </span>
      )}
    </button>
  )
}

export type ButtonLinkProps = AnchorHTMLAttributes<HTMLAnchorElement> &
  Look & {
    href: string
    plain?: boolean
  }

export function ButtonLink({
  variant,
  size = 'md',
  icon,
  plain = false,
  className,
  children,
  ...rest
}: ButtonLinkProps) {
  const cls = buttonClass({ variant, size, className })
  const content = (
    <Content icon={icon} size={size}>
      {children}
    </Content>
  )
  if (plain || (rest.target && rest.target !== '_self'))
    return (
      <a
        className={cls}
        rel={rest.target === '_blank' && rest.rel === undefined ? 'noopener noreferrer' : undefined}
        {...rest}
      >
        {content}
      </a>
    )
  return (
    <Link className={cls} {...rest}>
      {content}
    </Link>
  )
}
