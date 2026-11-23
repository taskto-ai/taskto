// What an app's effects do outside its records, said in words for the panel that comes before a
// publish ("What it can read and change"): each effect of the draft on its own line, with where it
// goes as the person may see it (the names the server told, or that it is something they can't
// see) and, when it has a rule, when it applies. The server decides what an effect may do; this
// only tells people what the draft says.

import type { App, AppDraft, DraftEffect, EffectTarget } from '../api'
import { type Key, t } from '../i18n'
import type { Rule } from './rules'

type Said = (items: readonly string[], type?: 'conjunction' | 'disjunction') => string

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
export function ruleText(rule: Rule, said: Said, nested = false): string {
  const group = (rules: Rule[], type: 'conjunction' | 'disjunction') => {
    const parts = rules.map((r) => ruleText(r, said, true))
    const text = said(parts, type)
    return nested && parts.length > 1 ? t('apps.builder.rule.group', { rule: text }) : text
  }
  if (rule.all && rule.all.length > 0) return group(rule.all, 'conjunction')
  if (rule.any && rule.any.length > 0) return group(rule.any, 'disjunction')
  const key = rule.op && COMPARE[rule.op]
  if (typeof rule.field !== 'string' || !key) return t('apps.builder.rule.unknown')
  const value = typeof rule.value === 'string' ? t('apps.builder.rule.text', { value: rule.value }) : String(rule.value)
  return t(key, { field: rule.field, value })
}

function column(name: string | undefined): string {
  const key = `boards.columns.${name}`
  return name && name in COLUMNS ? t(key as Key) : (name ?? '')
}

const COLUMNS: Record<string, true> = { backlog: true, todo: true, doing: true, review: true, done: true }

// The lines of the effects the draft declares, by action (in the order of the actions' names) and
// then by effect: empty when it declares none.
export function effectLines(app: Pick<App, 'effect_targets'>, draft: AppDraft | undefined, said: Said): string[] {
  const targets = app.effect_targets ?? []
  const actions = draft?.actions ?? {}
  const all = Object.keys(actions)
    .sort()
    .flatMap((name) => actions[name].effects ?? [])
  const maker = (card: string | undefined) => all.find((e) => e.kind === 'create_card' && e.name === card)
  return all.map((e) => {
    let line: string
    switch (e.kind) {
      case 'create_card':
        line = t('apps.builder.can.effect.createCard', { board: boardOf(e, targets) })
        break
      case 'update_card': {
        const made = maker(e.card)
        line = made
          ? t('apps.builder.can.effect.updateCard', { board: boardOf(made, targets), column: column(e.column) })
          : t('apps.builder.can.effect.updateCardUnknown', { column: column(e.column) })
        break
      }
      case 'create_document':
        line = t('apps.builder.can.effect.createDocument', { path: e.path ?? '', space: spaceOf(e, targets) })
        break
      default:
        if (e.route === 'board') line = t('apps.builder.can.effect.openBoard', { board: boardOf(e, targets) })
        else if (e.route === 'card') {
          const made = maker(e.card)
          line = made
            ? t('apps.builder.can.effect.openCard', { board: boardOf(made, targets) })
            : t('apps.builder.can.effect.openCardUnknown')
        } else line = t('apps.builder.can.effect.openDocument', { path: e.path ?? '', space: spaceOf(e, targets) })
    }
    return e.when
      ? t('apps.builder.can.effect.when', { effect: line, rule: ruleText(e.when, said) })
      : t('apps.builder.can.effect.always', { effect: line })
  })
}
