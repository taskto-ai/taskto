type Strip<T> = Exclude<T, null | undefined>
type Join<P extends string, K extends string> = P extends '' ? K : `${P}.${K}`
type IsAny<T> = 0 extends 1 & T ? true : false

type Depth = [unknown, unknown, unknown, unknown, unknown, unknown, unknown, unknown]

export type Problem<G, U, P extends string, Loose extends string, D extends unknown[] = []> = D extends Depth
  ? never
  : IsAny<G> extends true
    ? `${P}: the server's type is any`
    : IsAny<U> extends true
      ? `${P}: the web app's type is any: say unknown, or what it is`
      : unknown extends G
        ? unknown extends U
          ? never
          : `${P}: the server sends any JSON here (unknown): the web app's type must say unknown`
        : unknown extends U
          ? never
          :
              | (null extends G ? (null extends U ? never : `${P}: the server can send null`) : never)
              | Shape<Strip<G>, Strip<U>, P, Loose, [...D, unknown]>

type Arr = readonly unknown[]
type Plain<T> = Exclude<Extract<T, object>, Arr>

type Shape<G, U, P extends string, Loose extends string, D extends unknown[]> =
  | Primitives<Exclude<G, object>, Exclude<U, object>, P>
  | Arrays<Extract<G, Arr>, Extract<U, Arr>, P, Loose, D>
  | Objects<Plain<G>, Plain<U>, P, Loose, D>

type Takes<GP, UP> = string extends GP ? (UP extends string ? string : UP) : UP

type Primitives<G, U, P extends string> = [G] extends [never]
  ? never
  : [G] extends [Takes<G, U>]
    ? never
    : `${P}: the server sends another type`

type Arrays<G, U, P extends string, Loose extends string, D extends unknown[]> = [G] extends [never]
  ? never
  : [U] extends [never]
    ? `${P}: the server sends an array`
    : false extends IsTuple<U>
      ? Elements<G, U, P, Loose, D>
      : false extends IsTuple<G>
        ? `${P}: the server sends a list of any length, and the web app's type is a tuple`
        : Elements<G, U, P, Loose, D>

type Elements<G, U, P extends string, Loose extends string, D extends unknown[]> = [G] extends [readonly (infer GE)[]]
  ? [U] extends [readonly (infer UE)[]]
    ? Problem<GE, UE, `${P}[]`, Loose, D>
    : never
  : never

type Objects<G, U, P extends string, Loose extends string, D extends unknown[]> = [G] extends [never]
  ? never
  : [U] extends [never]
    ? `${P}: the server sends an object`
    : U extends unknown
      ? Fields<G, U, P, Loose, D>
      : never

type Empty = Record<never, never>

type IsMap<T> = string extends keyof T ? true : number extends keyof T ? true : false

type IsTuple<T> = T extends readonly unknown[] ? (number extends T['length'] ? false : true) : false

type Fields<G, U, P extends string, Loose extends string, D extends unknown[]> =
  IsMap<U> extends true
    ? Problem<G[keyof G], U[keyof U], `${P}[]`, Loose, D>
    : {
        [K in keyof U & string]-?: IsMap<G> extends true
          ? Empty extends Pick<U, K>
            ? Problem<G[keyof G], U[K], Join<P, K>, Loose, D>
            : `${Join<P, K>}: the server sends a map, which may not have it`
          : K extends keyof G
            ? FieldProblem<G, U, K, Join<P, K>, Loose, D>
            : `${Join<P, K>}: the server doesn't send it`
      }[keyof U & string]

type FieldProblem<
  G,
  U,
  K extends keyof G & keyof U & string,
  P extends string,
  Loose extends string,
  D extends unknown[],
> =
  | (P extends Loose
      ? never
      : Empty extends Pick<G, K>
        ? Empty extends Pick<U, K>
          ? never
          : `${P}: the server can leave it out`
        : never)
  | Problem<G[K], U[K], P, Loose, D>

export type Check<Server, App, Loose extends string = never> = [Problem<Server, App, '', Loose>] extends [never]
  ? true
  : Problem<Server, App, '', Loose>

type AnyJSON = ReturnType<typeof JSON.parse>
type Is<T extends true> = T
type Says<T, Text extends string> = T extends `${string}${Text}${string}` ? true : false
type Server = {
  id: number
  name: string
  note?: string
  owner: { id: number; email: string } | null
  who: { kind: string; id?: number }
  tags: string[]
  kind: string
  extra: Record<string, unknown>
  opaque: unknown
  counts: Record<string, number>
  flag: boolean
  size: number
  anything: AnyJSON
}
type Loop = { next?: Loop; n: number }
export type Selftest = [
  Is<Check<Server, { id: number; name: string }>>,
  Is<Check<Server, { id: number | string; note?: string | null; tags: readonly string[] }>>,
  Is<Check<Server, { kind: 'a' | 'b'; tags: ('x' | 'y')[] }>>,
  Is<Check<Server, { opaque: unknown; extra: Record<string, unknown> }>>,
  Is<Check<Loop, Loop>>,
  Is<Says<Check<Server, { title: string }>, "title: the server doesn't send it">>,
  Is<Says<Check<Server, { id: string }>, 'id: the server sends another type'>>,
  Is<Says<Check<Server, { note: string }>, 'note: the server can leave it out'>>,
  Is<Says<Check<Server, { owner: { id: number } }>, 'owner: the server can send null'>>,
  Is<Says<Check<Server, { owner: { id: number; mail: string } | null }>, "owner.mail: the server doesn't send it">>,
  Is<Says<Check<Server, { tags: { id: number }[] }>, 'tags[]'>>,
  Is<Says<Check<Server, { tags: string }>, 'tags: the server sends an array'>>,
  Is<Says<Check<Server, { who: { kind: string; id: number } }>, 'who.id: the server can leave it out'>>,
  Is<Says<Check<Server, { who: { kind: string; ID: number } }>, "who.ID: the server doesn't send it">>,
  Is<Check<Server, { note: string }, 'note'>>,
  Is<Check<Server, { who: { kind: string; id: number } }, 'who.id'>>,
  Is<Says<Check<Server, { size: 1 | 2 }>, 'size: the server sends another type'>>,
  Is<Says<Check<Server, { flag: true }>, 'flag: the server sends another type'>>,
  Is<Says<Check<Server, { size: string }>, 'size: the server sends another type'>>,
  Is<Says<Check<Server, { who: string }>, 'who: the server sends an object'>>,
  Is<Check<Server, { tags: string[] | string }>>,
  Is<Says<Check<Server, { name: { a: string } }>, 'name: the server sends another type'>>,
  Is<Says<Check<Server, { who: string[] }>, 'who: the server sends an object'>>,
  Is<Says<Check<Server, { name: AnyJSON }>, "name: the web app's type is any">>,
  Is<Says<Check<Server, { anything: string }>, "anything: the server's type is any">>,
  Is<Says<Check<Server, { who: { kind: AnyJSON } }>, "who.kind: the web app's type is any">>,
  Is<Says<Check<{ a: AnyJSON }, { a: unknown }>, "a: the server's type is any">>,
  Is<Says<Check<Server, { opaque: { any: string } }>, 'opaque: the server sends any JSON'>>,
  Is<Says<Check<Server, { extra: Record<string, number> }>, 'extra[]: the server sends any JSON'>>,
  Is<Says<Check<Server, { counts: { a: number } }>, 'counts.a: the server sends a map, which may not have it'>>,
  Is<Check<Server, { counts: { a?: number } }>>,
  Is<Check<Server, { counts: Record<string, number> }>>,
  Is<Says<Check<Server, { counts: Record<string, string> }>, 'counts[]: the server sends another type'>>,
  Is<Check<{ fixed: { a: number; b: number } }, { fixed: Record<string, number> }>>,
  Is<Says<Check<{ fixed: { a: number; b: string } }, { fixed: Record<string, number> }>, 'fixed[]'>>,
  Is<Says<Check<Server, { counts: Record<number, string> }>, 'counts[]: the server sends another type'>>,
  Is<Check<Server, { counts: Record<number, number> }>>,
  Is<Says<Check<Server, { tags: [string] }>, 'tags: the server sends a list of any length'>>,
  Is<Says<Check<Server, { tags: readonly [string, string] }>, 'tags: the server sends a list of any length'>>,
  Is<Check<{ pair: [string, string] }, { pair: [string, string] }>>,
  Is<Check<Server, { tags: string[] | [number] }>>,
  Is<Says<Check<Server, { tags: [number] | [string, string] }>, 'tags: the server sends a list of any length'>>,
]
