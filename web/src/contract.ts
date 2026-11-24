// Type-level checks that what the web app declares of an API reply is what the server sends
// (api.contract.ts uses them, against the shapes api.gen.ts has from the Go structs).
//
// `Check<Server, App>` is `true` when every field the web app's type `App` reads exists in the
// server's reply `Server`, can't be missing or null there unless App allows it, and has a type
// the server's fits: a field renamed or retyped in Go, once api.gen.ts is regenerated, makes
// the check say which field. The web app's types may be narrower than the server's (a union of
// strings for a `string`) and may leave out fields it doesn't read. Nothing here exists at
// run time.
//
// `Loose` names, by path ("assignee.id"), fields that may be required in App though the server
// omits them at times: the members of a union the web app tells apart by another field.

type Strip<T> = Exclude<T, null | undefined>
type Join<P extends string, K extends string> = P extends '' ? K : `${P}.${K}`

// Types nest at most this deep (a rule inside a rule is checked to here, and no further).
type Depth = [unknown, unknown, unknown, unknown, unknown, unknown, unknown, unknown]

// Why App's type can't stand for what the server sends (never when it can).
export type Problem<G, U, P extends string, Loose extends string, D extends unknown[] = []> = D extends Depth
  ? never
  : unknown extends G
    ? never // the server's side is anything: nothing to say
    : unknown extends U
      ? never // the web app doesn't read it
      :
          | (null extends G ? (null extends U ? never : `${P}: the server can send null`) : never)
          | Shape<Strip<G>, Strip<U>, P, Loose, [...D, unknown]>

type Shape<G, U, P extends string, Loose extends string, D extends unknown[]> = [U] extends [never]
  ? never
  : U extends readonly (infer UE)[]
    ? [G] extends [readonly (infer GE)[]]
      ? Problem<GE, UE, `${P}[]`, Loose, D>
      : `${P}: the server doesn't send an array`
    : U extends object
      ? [G] extends [object]
        ? Fields<G, U, P, Loose, D>
        : `${P}: the server doesn't send an object`
      : [U] extends [G]
        ? never
        : `${P}: the server sends another type`

type Fields<G, U, P extends string, Loose extends string, D extends unknown[]> = {
  [K in keyof U & string]-?: K extends keyof G
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
}
type Loop = { next?: Loop; n: number }
export type Selftest = [
  Is<Check<Server, { id: number; name: string }>>,
  // Narrower than the server's, and fields it doesn't read.
  Is<Check<Server, { kind: 'a' | 'b'; note?: string; tags: ('x' | 'y')[]; extra: Record<string, number> }>>,
  Is<Check<Server, { opaque: { any: string } }>>,
  Is<Check<Server, { extra: unknown }>>,
  Is<Check<Loop, Loop>>,
  Is<Says<Check<Server, { title: string }>, "title: the server doesn't send it">>,
  Is<Says<Check<Server, { id: string }>, 'id: the server sends another type'>>,
  Is<Says<Check<Server, { note: string }>, 'note: the server can leave it out'>>,
  Is<Says<Check<Server, { owner: { id: number } }>, 'owner: the server can send null'>>,
  Is<Says<Check<Server, { owner: { id: number; mail: string } | null }>, "owner.mail: the server doesn't send it">>,
  Is<Says<Check<Server, { tags: { id: number }[] }>, 'tags[]'>>,
  Is<Says<Check<Server, { tags: string }>, 'tags: the server sends another type'>>,
  Is<Says<Check<Server, { who: { kind: string; id: number } }>, 'who.id: the server can leave it out'>>,
  Is<Says<Check<Server, { who: { kind: string; ID: number } }>, "who.ID: the server doesn't send it">>,
  Is<Check<Server, { note: string }, 'note'>>,
  Is<Check<Server, { who: { kind: string; id: number } }, 'who.id'>>,
]
