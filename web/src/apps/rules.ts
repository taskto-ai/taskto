// The rules of an app (internal/apps/rules.go): one field compared with a literal, combined with
// all and any. The browser evaluates them to show or hide an input; the server evaluates the same
// rules, and enforces conditional requirements on every insert and update whatever was shown, so
// this is never what decides what a row may hold. testdata/rules.json holds the cases both agree on.

export type Rule = {
  field?: string
  op?: 'eq' | 'ne' | 'lt' | 'lte' | 'gt' | 'gte'
  value?: unknown
  all?: Rule[]
  any?: Rule[]
}

// What the form or the record holds: field name → value, as the data model has it.
export type Values = Record<string, unknown>

// A value as the server reads it: text is trimmed and blank text is no value, a ChoicePicker's list
// of one choice is the choice, and a value is of the literal's type or it is no value (a number or a
// boolean typed in a form arrives as text).
function normalize(have: unknown, literal: unknown): string | number | boolean | undefined {
  let v = have
  if (Array.isArray(v)) {
    if (v.length !== 1) return undefined
    v = v[0]
  }
  if (typeof v === 'string') {
    v = v.trim()
    if (v === '') return undefined
  }
  if (typeof literal === 'number') {
    if (typeof v === 'string') v = Number(v)
    return typeof v === 'number' && Number.isFinite(v) ? v : undefined
  }
  if (typeof literal === 'boolean') {
    if (v === 'true') return true
    if (v === 'false') return false
    return typeof v === 'boolean' ? v : undefined
  }
  return typeof v === 'string' ? v : undefined
}

// Whether the rule holds of the values. No value equals nothing: every comparison but ne is false
// for it. A malformed rule holds of nothing.
export function holds(rule: Rule, values: Values): boolean {
  if (rule.all) return rule.all.every((r) => holds(r, values))
  if (rule.any) return rule.any.some((r) => holds(r, values))
  if (typeof rule.field !== 'string') return false
  const have = normalize(values[rule.field], rule.value)
  if (have === undefined) return rule.op === 'ne'
  const want: unknown = rule.value
  let order = 0
  if (typeof have === 'boolean') order = have === want ? 0 : 1
  else if (typeof have === 'number' && typeof want === 'number') order = Math.sign(have - want)
  else if (typeof have === 'string' && typeof want === 'string') order = have < want ? -1 : have > want ? 1 : 0
  else return false
  switch (rule.op) {
    case 'eq':
      return order === 0
    case 'ne':
      return order !== 0
    case 'lt':
      return order < 0
    case 'lte':
      return order <= 0
    case 'gt':
      return order > 0
    case 'gte':
      return order >= 0
    default:
      return false
  }
}
