// Amounts of money as the integration screens write them: the server's own table of each
// currency's decimals (a ceiling or an event's amount is in the currency's smallest unit), never the
// browser's, since they differ for a few currencies, such as the peso and the forint.

import type { Format } from '../i18n'

export type Exponents = Record<string, number>

// An amount in a currency's smallest unit, written as people write money.
export function money(f: Format, exponents: Exponents, minor: number, currency: string): string {
  const digits = exponents[currency] ?? 2
  return f.number(minor / 10 ** digits, {
    style: 'currency',
    currency,
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })
}

// An amount the server wrote as decimal text with the currency's own decimals ("120.00"), written as
// people write money, with the same number of decimals.
export function decimalMoney(f: Format, amount: string, currency: string): string {
  const value = Number(amount)
  if (!Number.isFinite(value)) return `${amount} ${currency}`
  const digits = amount.includes('.') ? amount.length - amount.indexOf('.') - 1 : 0
  return f.number(value, { style: 'currency', currency, minimumFractionDigits: digits, maximumFractionDigits: digits })
}

// What a ceiling comes to in each of the currencies: the same number of smallest units is not the
// same sum in euros and in yen, so the page says each.
export function amounts(f: Format, exponents: Exponents, minor: number, currencies: string[]): string {
  return f.list(currencies.map((c) => money(f, exponents, minor, c)))
}
