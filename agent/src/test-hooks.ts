import type { ToolSpec } from './taskto-tools.js'

export const testHooks = {
  scriptFailures: false,
  walBusyMs: undefined as number | undefined,
  spikeTools: false,
  extraToolSpecs: [] as ToolSpec[],
  beforeModelWrite: undefined as (() => Promise<void>) | undefined,
  beforeContinueWrite: undefined as (() => Promise<void>) | undefined,
}
