// The frame of every screen outside the app: first-run setup, logging in, invites, password reset,
// and what is shown before the app starts. Centred on the page, the logo at a size of its own, a
// title in the display face, a line of help, then the form; the paths that lead elsewhere (back,
// forgot the password) are quieter, under it. It keeps clear of the status and home bars of an
// installed app, and fits a 320px screen.

import { type FormEvent, type ReactNode, useEffect, useRef } from 'react'
import { BrandLogo } from '../BrandLogo'
import { Heading } from '../ui/Heading'
import './AuthLayout.css'

// The product's name, the same in every language.
export const BRAND = 'TaskTo.AI'

export function AuthLayout({
  title,
  help,
  children,
  links,
  busy,
}: {
  title?: ReactNode
  help?: ReactNode
  children?: ReactNode
  // Links (or quiet buttons) to the other paths: back, forgot your password.
  links?: ReactNode
  // The screen is waiting for something before it can be used.
  busy?: boolean
}) {
  return (
    <main className="ui-auth" id="main" aria-busy={busy || undefined}>
      <div className="ui-auth-card">
        <div className="ui-auth-brand">
          <BrandLogo name={BRAND} height="var(--space-10)" />
        </div>
        {(title || help) && (
          <div className="ui-auth-head">
            {title && (
              <Heading level={1} size="display">
                {title}
              </Heading>
            )}
            {help && <div className="ui-auth-help">{help}</div>}
          </div>
        )}
        {children}
        {links && <div className="ui-auth-links">{links}</div>}
      </div>
    </main>
  )
}

// A form on an auth screen: its fields, then the one primary button, full width. Each is drawn with
// a key of its own (the step it is): two forms drawn at the same place, such as an invite's new
// account and its log-in, would otherwise share their fields, and a password shown in one would
// start shown in the next.
export function AuthForm({
  onSubmit,
  failure,
  onEdit,
  children,
}: {
  onSubmit: (e: FormEvent) => void
  // The form's last failure: when one comes, the focus goes to the first field it marked
  // invalid, or else to the form's error, so the person lands where there is something to do.
  failure?: object | null
  // A field of the form changed, by its name: a failure about it can go.
  onEdit?: (name: string) => void
  children: ReactNode
}) {
  const ref = useRef<HTMLFormElement>(null)
  useEffect(() => {
    const form = ref.current
    if (!failure || !form) return
    const target =
      form.querySelector<HTMLElement>('[aria-invalid="true"]') ?? form.querySelector<HTMLElement>('.ui-auth-error')
    target?.focus()
  }, [failure])
  return (
    <form
      ref={ref}
      className="ui-auth-form"
      onSubmit={onSubmit}
      onChange={(e) => {
        if (e.target instanceof HTMLInputElement && e.target.name) onEdit?.(e.target.name)
      }}
    >
      {children}
    </form>
  )
}

// The quiet "or" between the single sign-on buttons and the password form.
export function AuthDivider({ children }: { children: ReactNode }) {
  return (
    <p className="ui-auth-divider">
      <span>{children}</span>
    </p>
  )
}
