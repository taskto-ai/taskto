import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { FieldDictation } from './FieldDictation'

type Target = { id: number; host: HTMLElement; input: HTMLInputElement | HTMLTextAreaElement; slot: HTMLElement }

let ids = 0

const FIELDS = 'a2ui-basic-textfield, .ui-app-markdown-input'

export function workspaceOf(base: string | undefined): number | null {
  const m = base ? /\/workspaces\/(\d+)\//.exec(base) : null
  return m ? Number(m[1]) : null
}

export function insertText(input: HTMLInputElement | HTMLTextAreaElement, text: string) {
  const at = input.selectionStart ?? input.value.length
  const end = input.selectionEnd ?? at
  const before = input.value.slice(0, at)
  const space = before && !/\s$/.test(before) ? ' ' : ''
  const next = before + space + text + input.value.slice(end)
  const set = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), 'value')?.set
  if (set) set.call(input, next)
  else input.value = next
  const cursor = (before + space + text).length
  input.setSelectionRange?.(cursor, cursor)
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

function scan(root: HTMLElement, known: Target[]): Target[] {
  const next: Target[] = []
  for (const host of root.querySelectorAll<HTMLElement>(FIELDS)) {
    const input = host.querySelector<HTMLInputElement | HTMLTextAreaElement>('textarea, input')
    const text = input instanceof HTMLTextAreaElement || (input instanceof HTMLInputElement && input.type === 'text')
    if (!input || !text) continue
    const had = known.find((k) => k.host === host && k.input === input)
    if (had?.slot.isConnected) {
      next.push(had)
      continue
    }
    const slot = document.createElement('div')
    slot.className = 'ui-dictate-slot'
    host.appendChild(slot)
    next.push({ id: ++ids, host, input, slot })
  }
  return next
}

export function TextDictation({ root, workspace }: { root: HTMLElement | null; workspace: number }) {
  const [targets, setTargets] = useState<Target[]>([])
  useEffect(() => {
    if (!root) return
    let current: Target[] = []
    const refresh = () => {
      const next = scan(root, current)
      for (const gone of current) if (!next.includes(gone)) gone.slot.remove()
      const changed = next.length !== current.length || next.some((n, i) => n !== current[i])
      current = next
      if (changed) setTargets(next)
    }
    refresh()
    const observer = new MutationObserver(refresh)
    observer.observe(root, { childList: true, subtree: true })
    return () => {
      observer.disconnect()
      for (const t of current) t.slot.remove()
      setTargets([])
    }
  }, [root])
  return (
    <>
      {targets.map(({ id, host, input, slot }) => (
        <Dictate key={id} host={host} input={input} slot={slot} workspace={workspace} />
      ))}
    </>
  )
}

function Dictate({
  host,
  input,
  slot,
  workspace,
}: {
  host: HTMLElement
  input: HTMLInputElement | HTMLTextAreaElement
  slot: HTMLElement
  workspace: number
}) {
  const field = host.querySelector('label')?.textContent?.trim() ?? ''
  return createPortal(
    <FieldDictation workspace={workspace} field={field} onText={(text) => insertText(input, text)} />,
    slot,
  )
}
