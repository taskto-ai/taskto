import { api, type Me, type Space, type SpaceMember, ws } from '../api'
import { useLoad } from '../load'

export type Person = { id: number; name: string }

const readers = (workspace: number, space: Space): Promise<Person[]> =>
  space.kind === 'private'
    ? api<Me>('/api/me').then((me) => [{ id: me.user.id, name: me.user.name }])
    : api<{ members: SpaceMember[] }>(`${ws(workspace)}/spaces/${space.id}/members`).then((r) => r.members)

export function usePeople(workspace: number, space: Space, canWrite: boolean) {
  return useLoad(
    () => (canWrite ? readers(workspace, space) : Promise.resolve([])),
    [workspace, space.id, space.kind, canWrite],
  )
}
