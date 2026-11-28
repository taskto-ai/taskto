import { type KeyboardEvent, useId, useRef, useState } from 'react'
import { useFormat, useT } from '../i18n'
import { Markdown } from '../markdown'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Icon } from '../ui/Icon'
import { Tabs, tabPanelProps } from '../ui/Tabs'
import { Textarea } from '../ui/Textarea'
import {
  CHECKLIST_PREFIX,
  type Edit,
  HEADING_PREFIX,
  keepBreaks,
  LIST_PREFIX,
  link,
  prefixLines,
  splice,
  wrap,
} from './markdownEdit'
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

function typeInto(el: HTMLTextAreaElement, edit: Edit) {
  const before = el.value
  const { at, removed, insert } = splice(before, edit.text)
  el.focus()
  el.setSelectionRange(at, at + removed)
  const done =
    typeof document.execCommand === 'function' && document.execCommand(insert ? 'insertText' : 'delete', false, insert)
  if (!done || el.value === before) {
    el.setRangeText(insert, at, at + removed, 'end')
    el.dispatchEvent(
      new InputEvent('input', {
        bubbles: true,
        inputType: insert ? 'insertText' : 'deleteContentBackward',
        data: insert || null,
      }),
    )
  }
  el.setSelectionRange(edit.start, edit.end)
}

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
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const [active, setActive] = useState<Tool>('bold')
  const toolbar = useRef<HTMLDivElement>(null)

  const run = (tool: Tool) => {
    const el = box.current
    if (!el) return
    const { selectionStart: from, selectionEnd: to, value: text } = el
    const apply = (edit: Edit) => typeInto(el, edit)
    if (tool === 'bold') apply(wrap(text, from, to, '**', t('apps.markdownInput.boldSample')))
    else if (tool === 'italic') apply(wrap(text, from, to, '_', t('apps.markdownInput.italicSample'), '*'))
    else if (tool === 'heading') apply(prefixLines(text, from, to, HEADING_PREFIX))
    else if (tool === 'list') apply(prefixLines(text, from, to, LIST_PREFIX))
    else if (tool === 'checklist') apply(prefixLines(text, from, to, CHECKLIST_PREFIX))
    else apply(link(text, from, to, t('apps.markdownInput.linkSample'), ADDRESS))
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

  const used = Array.from(value).length
  const over = maxLength !== undefined && used > maxLength * COUNTER_SHARE
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
      <div {...tabPanelProps(ids, 'write')} hidden={tab !== 'write'} className="ui-app-markdown-input-panel">
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
        <Field label={label ?? t('apps.markdownInput.label')} error={tab === 'write' ? error : undefined}>
          <Textarea
            ref={box}
            rows={8}
            className="ui-app-markdown-textarea"
            value={value}
            onChange={(e) => onChange(keepBreaks(value, e.target.value))}
            onKeyDown={shortcut}
          />
        </Field>
      </div>
      <div {...tabPanelProps(ids, 'preview')} hidden={tab !== 'preview'} className="ui-app-markdown-input-panel">
        {tab === 'preview' &&
          (value.trim() === '' ? (
            <p className="ui-app-markdown-empty">{t('apps.markdownInput.empty')}</p>
          ) : (
            <div className="ui-app-markdown">
              <Markdown text={value} headingShift={2} links="text" />
            </div>
          ))}
        {tab === 'preview' && error && (
          <div className="ui-field-live" aria-live="polite">
            <p className="ui-field-error">
              <Icon name="alert" size={14} />
              <span>{error}</span>
            </p>
          </div>
        )}
      </div>
      {over && maxLength !== undefined && (
        <p className="ui-app-markdown-count" aria-live="polite" data-full={used > maxLength ? '' : undefined}>
          {t('apps.markdownInput.count', { used: number(used), max: number(maxLength) })}
        </p>
      )}
    </div>
  )
}
