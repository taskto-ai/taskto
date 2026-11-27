import { defineDoc } from '@earendil-works/pi-durable'

type ContinueState = { consumed: number; submission: number }

export const ContinuePin = defineDoc<ContinueState>({
  kind: 'taskto.continue-pin',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ consumed: 0, submission: 0 }),
  checkpointWhen: () => true,
})

export function pinHolds(pin: ContinueState | undefined, live: { run?: { inputs: readonly number[] } } | undefined) {
  return (
    pin !== undefined && pin.submission !== 0 && live?.run?.inputs.some((id) => Number(id) === pin.submission) === true
  )
}
