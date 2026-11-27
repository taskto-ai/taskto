import type { JsonValue } from '@earendil-works/chord/json'

export type TasktoInputEntry = { kind: 'taskto.input'; data: Record<string, JsonValue> }

export function validInputEntry(value: unknown): value is TasktoInputEntry {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const entry = value as Record<string, unknown>
  if (entry.kind !== 'taskto.input' || Object.keys(entry).some((key) => key !== 'kind' && key !== 'data')) return false
  if (typeof entry.data !== 'object' || entry.data === null || Array.isArray(entry.data)) return false
  try {
    const json = JSON.stringify(entry.data)
    return typeof json === 'string' && new TextEncoder().encode(json).byteLength <= 4096
  } catch {
    return false
  }
}
