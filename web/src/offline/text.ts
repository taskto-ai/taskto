const EDGE_SPACE = /^\p{White_Space}+|\p{White_Space}+$/gu

export function trimSpace(text: string): string {
  return text.replace(EDGE_SPACE, '')
}

export function numberFromText(text: string): number {
  return text === text.trim() ? Number(text) : Number.NaN
}
