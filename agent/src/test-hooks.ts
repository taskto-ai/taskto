// What the integration tests can add to the sidecar. They are in-process switches, set only by
// test-entry.ts before it starts the sidecar: the production entry (index.js) never sets them, and
// no environment variable or setting of the server's does, so the production model is never shown
// a test tool.
import type { ToolSpec } from './taskto-tools.js'

export const testHooks = {
  /** Install the pause tools (tools.ts). */
  spikeTools: false,
  /** Tools added to the server's list, to test how the sidecar picks among them. */
  extraToolSpecs: [] as ToolSpec[],
}
