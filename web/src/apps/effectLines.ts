// What an app's effects do outside its records, said in words for the panel that comes before a
// publish ("What it can read and change"): each effect of the draft on its own line, with where it
// goes as the person may see it (the names the server told, or that it is something they can't
// see) and, when it has a rule, when it applies. The server decides what an effect may do; this
// only tells people what the draft says.

import type { App, AppDraft, DraftEffect, EffectTarget } from '../api'
import { type Format, type Key, t } from '../i18n'
import type { Rule } from './rules'

// A board as the person may see it: "Maintenance (Field team)", or that it is one they can't see,
// or that none is chosen yet.
function boardOf(e: DraftEffect | undefined, targets: EffectTarget[]): string {
  if (!e?.space_id || !e.board_id) return t('apps.builder.can.target.boardUnset')
  const at = targets.find((x) => x.space_id === e.space_id && x.board_id === e.board_id)
  if (!at?.board || !at.space) return t('apps.builder.can.target.boardHidden')
  return t('apps.builder.can.target.board', { board: at.board, space: spaceName(at) })
}

function spaceOf(e: DraftEffect, targets: EffectTarget[]): string {
  if (!e.space_id) return t('apps.builder.can.target.spaceUnset')
  const at = targets.find((x) => x.space_id === e.space_id)
  return at?.space ? spaceName(at) : t('apps.builder.can.target.spaceHidden')
}

// A private space is said in the user's language, as everywhere.
function spaceName(at: EffectTarget): string {
  return at.space_kind === 'private' ? t('knowledge.spaces.private') : (at.space ?? '')
}

const COMPARE: Record<string, Key> = {
  eq: 'apps.builder.rule.eq',
  ne: 'apps.builder.rule.ne',
  lt: 'apps.builder.rule.lt',
  lte: 'apps.builder.rule.lte',
  gt: 'apps.builder.rule.gt',
  gte: 'apps.builder.rule.gte',
}

// A rule in words: "result is “fail” and status is not “closed”". A rule it can't read is "its rule
// holds", which is true of any rule.
export function ruleText(rule: Rule, f: Pick<Format, 'list' | 'number'>, nested = false): string {
  const group = (rules: Rule[], type: 'conjunction' | 'disjunction') => {
    const parts = rules.map((r) => ruleText(r, f, true))
    const text = f.list(parts, type)
    return nested && parts.length > 1 ? t('apps.builder.rule.group', { rule: text }) : text
  }
  if (rule.all && rule.all.length > 0) return group(rule.all, 'conjunction')
  if (rule.any && rule.any.length > 0) return group(rule.any, 'disjunction')
  const key = rule.op && COMPARE[rule.op]
  if (typeof rule.field !== 'string' || !key) return t('apps.builder.rule.unknown')
  return t(key, { field: rule.field, value: valueText(rule.value, f) })
}

// A literal as people read it: text quoted, a number in the locale's format, a boolean in words.
function valueText(value: unknown, f: Pick<Format, 'number'>): string {
  if (typeof value === 'string') return t('apps.builder.rule.text', { value })
  if (typeof value === 'number') return f.number(value)
  if (typeof value === 'boolean') return t(value ? 'apps.builder.rule.yes' : 'apps.builder.rule.no')
  return String(value)
}

function column(name: string | undefined): string {
  const key = `boards.columns.${name}`
  return name && COLUMNS.has(name) ? t(key as Key) : (name ?? '')
}

const COLUMNS = new Set(['backlog', 'todo', 'doing', 'review', 'done'])

// The lines of the effects the draft declares, by action (in the order of the actions' names) and
// then by effect: empty when it declares none.
export function effectLines(
  app: Pick<App, 'effect_targets'>,
  draft: AppDraft | undefined,
  f: Pick<Format, 'list' | 'number'>,
): string[] {
  const targets = app.effect_targets ?? []
  const actions = draft?.actions ?? {}
  const all = Object.keys(actions)
    .sort()
    .flatMap((name) => actions[name].effects ?? [])
  return all.map((e) => {
    let line: string
    switch (e.kind) {
      case 'create_card':
        line = t('apps.builder.can.effect.createCard', { board: boardOf(e, targets) })
        break
      // Moving, and linking to, a card act on the card the record already has: the one made where the
      // create_card effect pointed when the record's was made (perhaps by an older release, on another
      // board), so the board now chosen isn't said.
      case 'update_card':
        line = t('apps.builder.can.effect.updateCard', { column: column(e.column) })
        break
      case 'create_document':
        line = t('apps.builder.can.effect.createDocument', { path: e.path ?? '', space: spaceOf(e, targets) })
        break
      default:
        if (e.route === 'board') line = t('apps.builder.can.effect.openBoard', { board: boardOf(e, targets) })
        else if (e.route === 'card') line = t('apps.builder.can.effect.openCard')
        else line = t('apps.builder.can.effect.openDocument', { path: e.path ?? '', space: spaceOf(e, targets) })
    }
    return e.when
      ? t('apps.builder.can.effect.when', { effect: line, rule: ruleText(e.when, f) })
      : t('apps.builder.can.effect.always', { effect: line })
  })
}
