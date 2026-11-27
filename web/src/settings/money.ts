import type { Format } from '../i18n'

export type Exponents = Record<string, number>

export function money(f: Format, exponents: Exponents, minor: number, currency: string): string {
  const digits = exponents[currency] ?? 2
  return f.number(minor / 10 ** digits, {
    style: 'currency',
    currency,
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })
}

export function decimalMoney(f: Format, amount: string, currency: string): string {
  const value = Number(amount)
  if (!Number.isFinite(value)) return `${amount} ${currency}`
  const digits = amount.includes('.') ? amount.length - amount.indexOf('.') - 1 : 0
  return f.number(value, { style: 'currency', currency, minimumFractionDigits: digits, maximumFractionDigits: digits })
}

export function amounts(f: Format, exponents: Exponents, minor: number, currencies: string[]): string {
  return f.list(currencies.map((c) => money(f, exponents, minor, c)))
}
