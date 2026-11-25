// The frame of every screen outside the app: first-run setup, logging in, invites, password reset,
// and what is shown before the app starts. Centred on the page, the logo at a size of its own, a
// title in the display face, a line of help, then the form; the paths that lead elsewhere (back,
// forgot the password) are quieter, under it. It keeps clear of the status and home bars of an
// installed app, and fits a 320px screen.

import type { FormEvent, ReactNode } from 'react'
import { BrandLogo } from '../BrandLogo'
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
              // The type is the span's: styles.css sets h1's outside the cascade layers, which
              // would win over any rule in one.
              <h1 className="ui-auth-title">
                <span className="ui-auth-title-text">{title}</span>
              </h1>
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

// A form on an auth screen: its fields, then the one primary button, full width.
export function AuthForm({ onSubmit, children }: { onSubmit: (e: FormEvent) => void; children: ReactNode }) {
  return (
    <form className="ui-auth-form" onSubmit={onSubmit}>
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
