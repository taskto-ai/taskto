import { type KeyboardEvent, useId, useLayoutEffect, useRef, useState } from 'react'
import { useFormat, useT } from '../i18n'
import { Markdown } from '../markdown'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Tabs, tabPanelProps } from '../ui/Tabs'
import { Textarea } from '../ui/Textarea'
import { CHECKLIST_PREFIX, type Edit, HEADING_PREFIX, LIST_PREFIX, link, prefixLines, wrap } from './markdownEdit'
import './MarkdownInput.css'

export const COUNTER_SHARE = 0.8
const ADDRESS = 'https://'

type Tool = 'bold' | 'italic' | 'heading' | 'list' | 'checklist' | 'link'

const TOOLS: readonly { tool: Tool; glyph: string }[] = [
  { tool: 'bold', glyph: 'B' },
  { tool: 'italic', glyph: 'I' },
  { tool: 'heading', glyph: 'H' },
  { tool: 'list', glyph: '•' },
  { tool: 'checklist', glyph: '☐' },
  { tool: 'link', glyph: '↗' },
]

export function MarkdownField({
  label,
  value,
  onChange,
  maxLength,
  error,
}: {
  label?: string
  value: string
  onChange: (value: string) => void
  maxLength?: number
  error?: string
}) {
  const t = useT()
  const { number } = useFormat()
  const ids = useId()
  const box = useRef<HTMLTextAreaElement>(null)
  const pending = useRef<Edit | null>(null)
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const [active, setActive] = useState<Tool>('bold')
  const toolbar = useRef<HTMLDivElement>(null)

  useLayoutEffect(() => {
    const edit = pending.current
    const el = box.current
    if (!edit || !el) return
    pending.current = null
    el.focus()
    el.setSelectionRange(edit.start, edit.end)
  })

  const apply = (edit: Edit) => {
    pending.current = edit
    onChange(edit.text)
  }

  const run = (tool: Tool) => {
    const el = box.current
    if (!el) return
    const { selectionStart: from, selectionEnd: to } = el
    if (tool === 'bold') apply(wrap(value, from, to, '**', t('apps.markdownInput.boldSample')))
    else if (tool === 'italic') apply(wrap(value, from, to, '_', t('apps.markdownInput.italicSample')))
    else if (tool === 'heading') apply(prefixLines(value, from, to, HEADING_PREFIX))
    else if (tool === 'list') apply(prefixLines(value, from, to, LIST_PREFIX))
    else if (tool === 'checklist') apply(prefixLines(value, from, to, CHECKLIST_PREFIX))
    else apply(link(value, from, to, t('apps.markdownInput.linkSample'), ADDRESS))
  }

  const shortcut = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (!(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey) return
    const tool = e.key === 'b' || e.key === 'B' ? 'bold' : e.key === 'i' || e.key === 'I' ? 'italic' : null
    if (!tool) return
    e.preventDefault()
    run(tool)
  }

  const roving = (e: KeyboardEvent<HTMLDivElement>) => {
    const buttons = [...(toolbar.current?.querySelectorAll<HTMLButtonElement>('button') ?? [])]
    const from = buttons.indexOf(document.activeElement as HTMLButtonElement)
    if (from < 0) return
    const rtl = getComputedStyle(e.currentTarget).direction === 'rtl'
    const step =
      e.key === (rtl ? 'ArrowLeft' : 'ArrowRight') ? 1 : e.key === (rtl ? 'ArrowRight' : 'ArrowLeft') ? -1 : 0
    const to =
      e.key === 'Home' ? 0 : e.key === 'End' ? buttons.length - 1 : (from + step + buttons.length) % buttons.length
    if (step === 0 && e.key !== 'Home' && e.key !== 'End') return
    e.preventDefault()
    buttons[to].focus()
    setActive(TOOLS[to].tool)
  }

  const over = maxLength !== undefined && value.length > maxLength * COUNTER_SHARE
  const tabs = [
    { id: 'write', label: t('apps.markdownInput.write') },
    { id: 'preview', label: t('apps.markdownInput.preview') },
  ]

  return (
    <div className="ui-app-markdown-input">
      <Tabs
        label={t('apps.markdownInput.modes')}
        items={tabs}
        value={tab}
        onChange={(id) => setTab(id === 'preview' ? 'preview' : 'write')}
        id={ids}
      />
      {tab === 'write' ? (
        <div {...tabPanelProps(ids, 'write')} className="ui-app-markdown-input-panel">
          <div
            ref={toolbar}
            className="ui-app-markdown-toolbar"
            role="toolbar"
            aria-label={t('apps.markdownInput.toolbar')}
            onKeyDown={roving}
          >
            {TOOLS.map(({ tool, glyph }) => (
              <Button
                key={tool}
                variant="ghost"
                size="sm"
                className="ui-app-markdown-tool"
                aria-label={t(`apps.markdownInput.${tool}`)}
                tabIndex={active === tool ? 0 : -1}
                onFocus={() => setActive(tool)}
                onClick={() => run(tool)}
              >
                <span aria-hidden="true" className={`ui-app-markdown-glyph ui-app-markdown-glyph-${tool}`}>
                  {glyph}
                </span>
              </Button>
            ))}
          </div>
          <Field label={label ?? t('apps.markdownInput.label')} error={error}>
            <Textarea
              ref={box}
              rows={8}
              className="ui-app-markdown-textarea"
              value={value}
              onChange={(e) => onChange(e.target.value)}
              onKeyDown={shortcut}
            />
          </Field>
        </div>
      ) : (
        <div {...tabPanelProps(ids, 'preview')} className="ui-app-markdown-input-panel">
          {value.trim() === '' ? (
            <p className="ui-app-markdown-empty">{t('apps.markdownInput.empty')}</p>
          ) : (
            <div className="ui-app-markdown">
              <Markdown text={value} headingShift={2} links="text" />
            </div>
          )}
        </div>
      )}
      {over && maxLength !== undefined && (
        <p className="ui-app-markdown-count" aria-live="polite" data-full={value.length > maxLength ? '' : undefined}>
          {t('apps.markdownInput.count', { used: number(value.length), max: number(maxLength) })}
        </p>
      )}
    </div>
  )
}
