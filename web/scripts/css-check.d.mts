// The types of what src/ui/elementRules.test.tsx takes from css-check.mjs (a plain script).

export interface CssRule {
  selector: string
  line: number
  properties: string[]
}

export const OPT_OUT: string
export const WRAPPERS: ReadonlySet<string>
export const EVERYWHERE: ReadonlyMap<string, { properties: string[]; reason: string }>
export function allRules(css: string): CssRule[]
export function compounds(selector: string): string[]
export function optsOut(compound: string): boolean
export function reachesAnywhere(selector: string): boolean
export function allowedEverywhere(rule: { selector: string; properties: string[] }): boolean
