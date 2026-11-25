// The app's buttons (docs/design.md, "The buttons"): pills, in four variants.
//
// - primary: the one thing a screen asks you to press, at most one per screen. Mint with ink on it,
//   an ink outline and the short hard lift that presses in.
// - secondary (the default): a white pill with the control's edge.
// - ghost: no ground until hovered, for the quieter actions of a row or a toolbar.
// - danger: what deletes or can't be undone, in the danger colour.
//
// Two sizes, md (36px) and sm (28px); on a touch screen md grows to 44px and sm keeps its look
// with a 44px target around it. A disabled button has a look of its own (a flat grey pill), never
// a faded copy of its variant. A loading one keeps its width and its name, shows a spinner over
// its label, says "Loading" to a screen reader and can't be pressed, but keeps the focus it has.
// `type` is "button" unless said otherwise, so a button in a form doesn't submit it by accident.
//
// ButtonLink is the same look for a link: the app's router Link for a path in the app, or a plain
// anchor (`plain`) for a download, another site or a page outside the app.

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
  // An icon before the label.
  icon?: IconName
}

// The classes of a button's look, for an element that must be something else (a <summary>, a
// <label> over a file input).
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
  // While loading, a press does nothing: not the handler, and not a form's submission.
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
    // A plain anchor, not the app's router: a download, another site, a page outside the app.
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
  if (plain)
    return (
      <a className={cls} {...rest}>
        {content}
      </a>
    )
  return (
    <Link className={cls} {...rest}>
      {content}
    </Link>
  )
}
