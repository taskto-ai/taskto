const SPACE = /\p{White_Space}/u

export function trimSpace(text: string): string {
  let start = 0
  let end = text.length
  while (start < end && SPACE.test(text.charAt(start))) start++
  while (end > start && SPACE.test(text.charAt(end - 1))) end--
  return text.slice(start, end)
}

export function numberFromText(text: string): number {
  return text === text.trim() ? Number(text) : Number.NaN
}
