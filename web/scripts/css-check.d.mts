// The types of what the tests take from css-check.mjs (a plain script).

export interface CssRule {
  selector: string
  line: number
  properties: string[]
}

export const OPT_OUT: string
export const WRAPPERS: ReadonlySet<string>
export const EVERYWHERE: ReadonlyMap<string, { properties: string[]; reason: string }>
export function allRules(css: string): CssRule[]
export function nestedSelector(parent: string, child: string): string
export function compounds(selector: string): string[]
export function optsOut(compound: string): boolean
export function reachesAnywhere(selector: string): boolean
export function allowedEverywhere(rule: { selector: string; properties: string[] }): boolean
export const OLD_STYLESHEETS: readonly string[]
export function oldStylesheets(web: string): { file: string; css: string }[]
export function topLevel(
  css: string,
): (
  | { kind: 'import'; url: string; rest: string; line: number }
  | { kind: 'block' | 'statement'; prelude: string; line: number }
)[]
