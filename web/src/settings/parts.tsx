import { type ReactNode, type Ref, useCallback, useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { format, useT } from '../i18n'
import { useMounted } from '../mounted'
import { sameSession } from '../session'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import './Settings.css'
import { placeholders } from '../byPlace'

export function useAction(labels?: ReadonlyMap<string, string>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const named = useRef(labels)
  named.current = labels
  const mounted = useMounted()
  const run = useCallback(
    async (change: () => Promise<unknown>): Promise<boolean> => {
      setBusy(true)
      setError(null)
      const same = sameSession()
      try {
        await change()
        return mounted.current && same()
      } catch (e) {
        if (mounted.current && same()) setError(errorText(e, named.current))
        return false
      } finally {
        if (mounted.current) setBusy(false)
      }
    },
    [mounted],
  )
  return { busy, error, run }
}

export function Alert({ error }: { error: string | null | undefined }) {
  if (!error) return null
  return <Notice tone="danger">{error}</Notice>
}

export function FormError({ error }: { error: string | null | undefined }) {
  const box = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (error) box.current?.focus()
  }, [error])
  if (!error) return null
  return (
    <div ref={box} tabIndex={-1} className="settings-form-error">
      <Notice tone="danger">{error}</Notice>
    </div>
  )
}

export function SettingsTab({
  title,
  help,
  actions,
  children,
}: {
  title: string
  help?: ReactNode
  actions?: ReactNode
  children: ReactNode
}) {
  const t = useT()
  return (
    <Page width="reading" className="settings-page">
      <PageHeader
        path={<span className="settings-path">{t('settings.title')}</span>}
        title={title}
        help={help}
        actions={actions}
      />
      {children}
    </Page>
  )
}

export function Section({
  title,
  help,
  actions,
  label,
  level = 2,
  headingRef,
  children,
}: {
  title?: ReactNode
  help?: ReactNode
  actions?: ReactNode
  label?: string
  level?: 2 | 3
  headingRef?: Ref<HTMLHeadingElement>
  children?: ReactNode
}) {
  const id = useId()
  return (
    <section
      className="settings-section"
      data-level={level}
      aria-labelledby={title ? `${id}-title` : undefined}
      aria-label={title ? undefined : label}
    >
      {(title || actions) && (
        <header className="settings-section-head">
          <div className="settings-section-titles">
            {title && (
              <Heading
                level={level}
                size={level === 2 ? 'section' : 'subsection'}
                id={`${id}-title`}
                ref={headingRef}
                tabIndex={headingRef ? -1 : undefined}
              >
                {title}
              </Heading>
            )}
            {help && <p className="settings-section-help">{help}</p>}
          </div>
          {actions && <div className="settings-section-actions">{actions}</div>}
        </header>
      )}
      {!title && !actions && help && <p className="settings-section-help">{help}</p>}
      {children}
    </section>
  )
}

export function Loading({ lines = 3 }: { lines?: number }) {
  const t = useT()
  return (
    <div className="settings-loading" role="status" aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {placeholders(lines).map(({ key, place: i }) => (
        <Skeleton key={key} width={i === lines - 1 ? '60%' : '100%'} />
      ))}
    </div>
  )
}

export function day(time: string): string {
  const d = new Date(time)
  return Number.isNaN(d.getTime()) ? time : format().date(d)
}

export function CopyButton({ text, name, size = 'md' }: { text: string; name?: string; size?: 'md' | 'sm' }) {
  const t = useT()
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 2000)
    return () => clearTimeout(timer)
  }, [copied])
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch {}
  }
  return (
    <Button
      size={size}
      icon={copied ? 'check' : 'copy'}
      aria-label={name ? t('common.copyNamed', { name }) : undefined}
      onClick={copy}
    >
      {copied ? t('common.copied') : t('common.copy')}
    </Button>
  )
}

export function CopyField({ value, label, name }: { value: string; label: string; name?: string }) {
  return (
    <div className="settings-copy">
      <Input readOnly value={value} aria-label={label} onFocus={(e) => e.target.select()} spellCheck={false} />
      <CopyButton text={value} name={name} />
    </div>
  )
}

export function Facts({ items }: { items: { term: ReactNode; value: ReactNode; key?: string }[] }) {
  return (
    <dl className="settings-facts">
      {items.map((x, i) => (
        <div key={x.key ?? i} className="settings-fact">
          <dt>{x.term}</dt>
          <dd>{x.value}</dd>
        </div>
      ))}
    </dl>
  )
}

export function FormActions({ children }: { children: ReactNode }) {
  return <div className="settings-actions">{children}</div>
}

export function useRefocus(data: unknown) {
  const heading = useRef<HTMLHeadingElement>(null)
  const [pending, setPending] = useState<{ shown: unknown } | null>(null)
  useEffect(() => {
    if (!pending || Object.is(pending.shown, data)) return
    setPending(null)
    heading.current?.focus()
  }, [pending, data])
  return { heading, after: () => setPending({ shown: data }) }
}
