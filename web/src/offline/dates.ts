const CALENDAR_DATE = /^(\d{4})-(\d{2})-(\d{2})$/

function isLeapYear(year: number): boolean {
  return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0
}

function daysInMonth(year: number, month: number): number {
  if (month === 2) return isLeapYear(year) ? 29 : 28
  return month === 4 || month === 6 || month === 9 || month === 11 ? 30 : 31
}

export function isCalendarDate(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const parts = CALENDAR_DATE.exec(value)
  if (!parts) return false
  const year = Number(parts[1])
  const month = Number(parts[2])
  const day = Number(parts[3])
  return month >= 1 && month <= 12 && day >= 1 && day <= daysInMonth(year, month)
}

export function isInMonth(value: unknown, month: string): boolean {
  return isCalendarDate(value) && value.startsWith(`${month}-`)
}
