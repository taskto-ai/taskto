import type { App, AppDraft, DraftEffect, EffectTarget } from '../api'
import { type Format, type Key, t } from '../i18n'
import type { Rule } from './rules'

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

export function valueText(value: unknown, f: Pick<Format, 'number'>): string {
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
      case 'update_card':
        line = t('apps.builder.can.effect.updateCard', { column: column(e.column) })
        break
      case 'create_document':
        line = t('apps.builder.can.effect.createDocument', { path: e.path ?? '', space: spaceOf(e, targets) })
        break
      case 'create_payment_link': {
        const slot = {
          slot: e.integration ?? '',
          amount: e.amount_field ?? '',
          url: e.url_field ?? '',
          id: e.id_field ?? '',
        }
        line = e.currency_field
          ? t('apps.builder.can.effect.createPaymentLinkField', { ...slot, currencyField: e.currency_field })
          : t('apps.builder.can.effect.createPaymentLink', { ...slot, currency: e.currency ?? '' })
        break
      }
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
