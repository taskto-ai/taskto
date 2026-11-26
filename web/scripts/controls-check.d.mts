// The types of what the tests take from controls-check.mjs (a plain script).

export const ELEMENTS: readonly string[]
export const ALLOWED: ReadonlyMap<string, { reason: string } & Partial<Record<string, number | string>>>
export function controls(source: string): { element: string; line: number }[]
export function check(web: string, allowed?: ReadonlyMap<string, unknown>): string[]
