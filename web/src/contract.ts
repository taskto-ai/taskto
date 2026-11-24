// Type-level checks that what the web app declares of an API reply is what the server sends
// (api.contract.ts uses them, against the shapes api.gen.ts has from the Go structs).
//
// `Check<Server, App>` is `true` when the server's reply `Server` can be used as the web app's
// type `App`: every field `App` reads exists in `Server`, can't be missing or null there unless
// `App` allows it, and has a type that what the server sends is assignable to. A field renamed
// or retyped in Go, once api.gen.ts is regenerated, makes the check say which field. The web
// app's types may leave out fields it doesn't read, and may be wider than the server's. They
// may be narrower in one way only: a union of string literals for a `string` (an enum Go keeps
// in a plain string); a `true`, a `1 | 2` or a smaller object is not allowed. Nothing here
// exists at run time.
//
// `any` is refused on either side, and a server `unknown` (free-form JSON, tagged so in Go)
// is accepted only where the web app also says `unknown`: a web app that says what is in it
// has checked nothing. A server `Record<string, T>` is a map: the web app can't read a fixed
// key from it unless that key is optional.
//
// `Loose` names, by path ("assignee.id"), fields that may be required in App though the server
// omits them at times: the members of a union the web app tells apart by another field. A
// union in `App` is checked one member at a time, each against the server's shape.

type Strip<T> = Exclude<T, null | undefined>
type Join<P extends string, K extends string> = P extends '' ? K : `${P}.${K}`
type IsAny<T> = 0 extends 1 & T ? true : false

// Types nest at most this deep (a rule inside a rule is checked to here, and no further).
type Depth = [unknown, unknown, unknown, unknown, unknown, unknown, unknown, unknown]

// Why App's type can't stand for what the server sends (never when it can).
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
          ? never // the web app doesn't read it
          :
              | (null extends G ? (null extends U ? never : `${P}: the server can send null`) : never)
              | Shape<Strip<G>, Strip<U>, P, Loose, [...D, unknown]>

type Arr = readonly unknown[]
type Plain<T> = Exclude<Extract<T, object>, Arr>

// The server's side, a kind at a time, must be one the web app's type takes.
type Shape<G, U, P extends string, Loose extends string, D extends unknown[]> =
  | Primitives<Exclude<G, object>, Exclude<U, object>, P>
  | Arrays<Extract<G, Arr>, Extract<U, Arr>, P, Loose, D>
  | Objects<Plain<G>, Plain<U>, P, Loose, D>

// Assignable from the server's to the web app's, but a string may be told as a union of its
// literals (an enum), the one narrowing allowed.
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
    : false extends IsTuple<U> // some alternative of the web app's type is a list
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

// A type with an index signature is a map: `Record<string, T>`.
type IsMap<T> = string extends keyof T ? true : number extends keyof T ? true : false

// A tuple has a length of its own; a list does not.
type IsTuple<T> = T extends readonly unknown[] ? (number extends T['length'] ? false : true) : false

type Fields<G, U, P extends string, Loose extends string, D extends unknown[]> =
  IsMap<U> extends true
    ? Problem<G[keyof G], U[keyof U], `${P}[]`, Loose, D>
    : {
        [K in keyof U & string]-?: IsMap<G> extends true
          ? // biome-ignore lint/complexity/noBannedTypes: `{}` is how an optional key is told from a required one
            {} extends Pick<U, K>
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
      : // biome-ignore lint/complexity/noBannedTypes: `{}` is how an optional key is told from a required one
        {} extends Pick<G, K>
        ? // biome-ignore lint/complexity/noBannedTypes: as above
          {} extends Pick<U, K>
          ? never
          : `${P}: the server can leave it out`
        : never)
  | Problem<G[K], U[K], P, Loose, D>

/** `true` when the web app's type is what the server sends; else says what differs. */
export type Check<Server, App, Loose extends string = never> = [Problem<Server, App, '', Loose>] extends [never]
  ? true
  : Problem<Server, App, '', Loose>

// What the checks say, checked: each line fails to compile if the check stops saying it.
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
  // biome-ignore lint/suspicious/noExplicitAny: `any` is what the check refuses, so it is tested with one
  anything: any
}
type Loop = { next?: Loop; n: number }
export type Selftest = [
  Is<Check<Server, { id: number; name: string }>>,
  // Wider than the server's, and fields it doesn't read.
  Is<Check<Server, { id: number | string; note?: string | null; tags: readonly string[] }>>,
  // Narrower, the one way allowed: string literals for a string.
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
  // The direction: what the server sends must be what the web app takes. Narrower than the
  // server's is refused, but for a string told as its literals.
  Is<Says<Check<Server, { size: 1 | 2 }>, 'size: the server sends another type'>>,
  Is<Says<Check<Server, { flag: true }>, 'flag: the server sends another type'>>,
  Is<Says<Check<Server, { size: string }>, 'size: the server sends another type'>>,
  Is<Says<Check<Server, { who: string }>, 'who: the server sends an object'>>,
  Is<Check<Server, { tags: string[] | string }>>,
  Is<Says<Check<Server, { name: { a: string } }>, 'name: the server sends another type'>>,
  Is<Says<Check<Server, { who: string[] }>, 'who: the server sends an object'>>,
  // `any` is no type: on either side.
  // biome-ignore lint/suspicious/noExplicitAny: `any` is what the check refuses, so it is tested with one
  Is<Says<Check<Server, { name: any }>, "name: the web app's type is any">>,
  Is<Says<Check<Server, { anything: string }>, "anything: the server's type is any">>,
  // biome-ignore lint/suspicious/noExplicitAny: `any` is what the check refuses, so it is tested with one
  Is<Says<Check<Server, { who: { kind: any } }>, "who.kind: the web app's type is any">>,
  // biome-ignore lint/suspicious/noExplicitAny: `any` is what the check refuses, so it is tested with one
  Is<Says<Check<{ a: any }, { a: unknown }>, "a: the server's type is any">>,
  // Free-form JSON from the server is read as unknown or not at all.
  Is<Says<Check<Server, { opaque: { any: string } }>, 'opaque: the server sends any JSON'>>,
  Is<Says<Check<Server, { extra: Record<string, number> }>, 'extra[]: the server sends any JSON'>>,
  // A map is not an object with keys.
  Is<Says<Check<Server, { counts: { a: number } }>, 'counts.a: the server sends a map, which may not have it'>>,
  Is<Check<Server, { counts: { a?: number } }>>,
  Is<Check<Server, { counts: Record<string, number> }>>,
  Is<Says<Check<Server, { counts: Record<string, string> }>, 'counts[]: the server sends another type'>>,
  Is<Check<{ fixed: { a: number; b: number } }, { fixed: Record<string, number> }>>,
  Is<Says<Check<{ fixed: { a: number; b: string } }, { fixed: Record<string, number> }>, 'fixed[]'>>,
  // A number-keyed map is a map too, its values compared.
  Is<Says<Check<Server, { counts: Record<number, string> }>, 'counts[]: the server sends another type'>>,
  Is<Check<Server, { counts: Record<number, number> }>>,
  // A tuple has a length the server's list doesn't promise.
  Is<Says<Check<Server, { tags: [string] }>, 'tags: the server sends a list of any length'>>,
  Is<Says<Check<Server, { tags: readonly [string, string] }>, 'tags: the server sends a list of any length'>>,
  Is<Check<{ pair: [string, string] }, { pair: [string, string] }>>,
  // A list among the alternatives takes what the server sends.
  Is<Check<Server, { tags: string[] | [number] }>>,
  Is<Says<Check<Server, { tags: [number] | [string, string] }>, 'tags: the server sends a list of any length'>>,
]
