export type Placed<T> = { readonly key: string; readonly item: T; readonly place: number }

export function byPlace<T>(items: readonly T[]): Placed<T>[] {
  return items.map((item, place) => ({ key: String(place), item, place }))
}

export function placeholders(count: number): Placed<number>[] {
  return byPlace(Array.from({ length: count }, (_, place) => place))
}
