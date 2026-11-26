// What an app asks of an outside service, in plain words: the server's contract for one slot (the
// fields the app sends out, the payment links it asks for and when, and what each event the service
// reports may write to a record), said so that the person who asks for it to be approved and the admin
// who approves it read the same thing. The contract is the server's, from the draft: nothing here
// decides what it holds, and the words of the app's own (a link's description) are not in it.

import type { ContractField, IntegrationContract } from '../api'
import { type Format, has, type Key, useFormat, useT } from '../i18n'
import { ruleText, valueText } from './effectLines'
import './Integrations.css'

type Translate = ReturnType<typeof useT>

// A field's name, as the text puts it: in quotes.
const quoted = (t: Translate, name: string) => t('apps.integrations.contract.field', { name })

// A closed list's word, or the code itself for one the server has that this version doesn't know.
function word(t: Translate, group: string, code: string): string {
  const key = `apps.integrations.contract.${group}.${code}`
  return has(key) ? t(key as Key) : code
}

// What one write sets: a literal the contract names, or a source the server takes from the event.
function setLine(t: Translate, f: Format, set: IntegrationContract['handlers'][number]['sets'][number]): string {
  const field = quoted(t, set.field)
  return set.from
    ? t('apps.integrations.contract.setFrom', { field, source: word(t, 'source', set.from) })
    : t('apps.integrations.contract.setValue', { field, value: valueText(set.value, f) })
}

// What one field may hold, in a list of short phrases: its type, whether it may be left out, and each
// limit the contract binds. Raising a limit, adding an option or dropping a requirement is another
// contract, so every one of them is said. The labels of options are not in the contract: only values.
function fieldRules(t: Translate, f: Format, field: ContractField): string[] {
  const typeKey = `apps.fieldType.${field.type}`
  const rules = [has(typeKey) ? t(typeKey as Key) : field.type]
  if (field.required) rules.push(t('apps.integrations.contract.constraint.required'))
  else if (field.required_when)
    rules.push(t('apps.integrations.contract.constraint.requiredWhen', { rule: ruleText(field.required_when, f) }))
  else rules.push(t('apps.integrations.contract.constraint.optional'))
  if (field.max_length) rules.push(t('apps.integrations.contract.constraint.maxLength', { count: field.max_length }))
  if (field.min !== undefined)
    rules.push(t('apps.integrations.contract.constraint.min', { value: f.number(field.min) }))
  if (field.max !== undefined)
    rules.push(t('apps.integrations.contract.constraint.max', { value: f.number(field.max) }))
  if (field.options && field.options.length > 0)
    rules.push(
      t('apps.integrations.contract.constraint.options', {
        options: f.list(field.options.map((o) => quoted(t, o))),
      }),
    )
  if (field.format) rules.push(t('apps.integrations.contract.constraint.format', { format: field.format }))
  if (field.editable) rules.push(t('apps.integrations.contract.constraint.editable'))
  if (field.max_files) rules.push(t('apps.integrations.contract.constraint.maxFiles', { count: field.max_files }))
  if (field.max_bytes)
    rules.push(t('apps.integrations.contract.constraint.maxBytes', { size: f.bytes(field.max_bytes) }))
  return rules
}

export function ContractSummary({ contract }: { contract: IntegrationContract }) {
  const t = useT()
  const f = useFormat()
  const links = contract.links ?? []
  const handlers = contract.handlers ?? []
  const fields = contract.fields ?? []
  return (
    <div className="apps-contract">
      {contract.sends.length > 0 && (
        <p>
          {t('apps.integrations.contract.sends', {
            service: word(t, 'service', contract.kind),
            fields: f.list(contract.sends.map((n) => quoted(t, n))),
          })}
        </p>
      )}
      {links.length > 0 && (
        <ul className="apps-contract-list">
          {links.map((link) => {
            const currency = link.currency
              ? link.currency
              : t('apps.integrations.contract.linkCurrencyField', {
                  field: quoted(t, link.currency_field ?? ''),
                  options: f.list(link.currency_options ?? []),
                })
            const kindKey = `apps.integrations.contract.actionKind.${link.action_kind}`
            const asks = t('apps.integrations.contract.link', {
              action: quoted(t, link.action),
              kind: has(kindKey) ? t(kindKey as Key) : link.action_kind,
              amount: quoted(t, link.amount_field),
              currency,
              url: quoted(t, link.url_field),
              id: quoted(t, link.id_field),
            })
            // The fields of the record the description puts in the text of the link, and the record's
            // number when it names that too: text the service is sent.
            const named = [
              ...link.describes.map((n) => quoted(t, n)),
              ...(link.describes_id ? [t('apps.integrations.contract.recordNumber')] : []),
            ]
            const text = link.when
              ? t('apps.integrations.contract.linkWhen', { link: asks, rule: ruleText(link.when, f) })
              : asks
            return (
              <li key={`${link.action}-${link.amount_field}-${link.url_field}`}>
                {named.length > 0
                  ? `${text} ${t('apps.integrations.contract.describes', { what: f.list(named) })}`
                  : text}
              </li>
            )
          })}
        </ul>
      )}
      {handlers.length > 0 && <p>{t('apps.integrations.contract.handlersIntro')}</p>}
      {handlers.length > 0 && (
        <ul className="apps-contract-list">
          {handlers.map((h) => {
            const gate = (contract.only_if ?? []).find((r) => r.event === h.event)
            const event = word(t, 'events', h.event)
            return (
              <li key={h.event}>
                {gate
                  ? t('apps.integrations.contract.eventWhen', { event, rule: ruleText(gate.rule, f) })
                  : t('apps.integrations.contract.event', { event })}
                <ul className="apps-contract-list">
                  {h.sets.map((set) => (
                    <li key={set.field}>{setLine(t, f, set)}</li>
                  ))}
                </ul>
              </li>
            )
          })}
        </ul>
      )}
      {fields.length > 0 && <p>{t('apps.integrations.contract.fieldsIntro')}</p>}
      {fields.length > 0 && (
        <ul className="apps-contract-list">
          {fields.map((field) => (
            <li key={field.name}>
              {t('apps.integrations.contract.fieldLine', {
                field: quoted(t, field.name),
                rules: f.list(fieldRules(t, f, field), 'unit'),
              })}
            </li>
          ))}
        </ul>
      )}
      <p className="apps-contract-note">{t('apps.integrations.contract.limits')}</p>
    </div>
  )
}
