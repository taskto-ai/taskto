import { type FormEvent, type ReactNode, useEffect, useRef } from 'react'
import { BrandLogo } from '../BrandLogo'
import { Heading } from '../ui/Heading'
import './AuthLayout.css'

export const BRAND = 'taskto.ai'

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
  links?: ReactNode
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

export function AuthForm({
  onSubmit,
  failure,
  onEdit,
  children,
}: {
  onSubmit: (e: FormEvent) => void
  failure?: object | null
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

export function AuthDivider({ children }: { children: ReactNode }) {
  return (
    <p className="ui-auth-divider">
      <span>{children}</span>
    </p>
  )
}
