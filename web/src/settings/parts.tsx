// What the settings pages share: a tab's frame (its page and header) and its sections, running a
// change and showing its error, the shape of what is loading, a read-only value to copy, a list of
// facts, and dates. Every tab is built from these and the components in ../ui, so they read alike.

import { type ReactNode, type Ref, useCallback, useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { format, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import './Settings.css'

// Runs one change at a time; a failure is kept, to show next to what failed.
// `labels` names a form's fields as the person sees them, for an error the server says of one
// ("the api_key isn't valid" reads "the Secret key isn't valid").
export function useAction(labels?: ReadonlyMap<string, string>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const named = useRef(labels)
  named.current = labels
  const run = useCallback(async (change: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true)
    setError(null)
    try {
      await change()
      return true
    } catch (e) {
      setError(errorText(e, named.current))
      return false
    } finally {
      setBusy(false)
    }
  }, [])
  return { busy, error, run }
}

// What failed, where it failed: a danger notice, said at once.
export function Alert({ error }: { error: string | null | undefined }) {
  if (!error) return null
  return <Notice tone="danger">{error}</Notice>
}

// A form's error, in a long form (a dialog that scrolls): the notice takes the focus when it
// appears, which also scrolls it into view, so a refused save is seen wherever the form was
// scrolled to, and a screen reader is taken to it.
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

// A tab: the page at the reading width, its name and a line on what it is for, then its sections.
// Above its name, "Settings", shown only where the side list (whose title says it) gives way to the
// picker: on a phone the page's head is the one place the section's name appears.
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

// A part of a tab: its heading, a line of help under it, the section's own actions at the end, and
// what it holds. `level` 3 for a section inside another.
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
  // The section's name when it has no visible title.
  label?: string
  level?: 2 | 3
  // The title, to move the keyboard to when what had it is gone (a row removed, an empty state
  // replaced); given one, the title takes the focus from a script, not from Tab.
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

// The shape of what is still loading, with "Loading" said to a screen reader.
export function Loading({ lines = 3 }: { lines?: number }) {
  const t = useT()
  return (
    <div className="settings-loading" role="status" aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {Array.from({ length: lines }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholders with no identity of their own.
        <Skeleton key={i} width={i === lines - 1 ? '60%' : '100%'} />
      ))}
    </div>
  )
}

// A date, in the user's language.
export function day(time: string): string {
  const d = new Date(time)
  return Number.isNaN(d.getTime()) ? time : format().date(d)
}

// Copies text to the clipboard; without one (plain http, or refused) the text is still selectable.
export function CopyButton({ text, name, size = 'md' }: { text: string; name?: string; size?: 'md' | 'sm' }) {
  const t = useT()
  const [copied, setCopied] = useState(false)
  // "Copied" is for that copy, not from then on; the timer goes with the button.
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 2000)
    return () => clearTimeout(timer)
  }, [copied])
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch {
      // No clipboard (plain http, or refused): the text is selectable.
    }
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

// A value shown once, to copy: a read-only field that selects itself, and the copy button.
export function CopyField({ value, label, name }: { value: string; label: string; name?: string }) {
  return (
    <div className="settings-copy">
      <Input readOnly value={value} aria-label={label} onFocus={(e) => e.target.select()} spellCheck={false} />
      <CopyButton text={value} name={name} />
    </div>
  )
}

// Facts about a thing, a term and its value per line: an endpoint, the server's email.
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

// A form's actions: its one primary button first, then the rest.
export function FormActions({ children }: { children: ReactNode }) {
  return <div className="settings-actions">{children}</div>
}

// After a change that takes away what had the keyboard (a row removed, the empty state replaced by
// the first row), the keyboard goes to a section's title once the list has been read again: call
// `after()` with the change; the title is focused when `data` next changes.
export function useRefocus(data: unknown) {
  const heading = useRef<HTMLHeadingElement>(null)
  const [pending, setPending] = useState(false)
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs when the list is read again
  useEffect(() => {
    if (!pending) return
    setPending(false)
    heading.current?.focus()
  }, [data])
  return { heading, after: () => setPending(true) }
}
