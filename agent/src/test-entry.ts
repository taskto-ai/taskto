// The sidecar for the integration tests: the production sidecar (index.ts) with the pause tools
// and a few fixture tools added. Tests start this entry point (agent/dist/test-entry.js); the
// server never does unless an operator names it with -agent-entry, and the production entry,
// index.js, has no way to turn any of it on.
import type { ToolSpec } from './taskto-tools.js'
import { testHooks } from './test-hooks.js'

const noArguments = { type: 'object', properties: {}, additionalProperties: false } as ToolSpec['input_schema']

testHooks.spikeTools = true
testHooks.walBusyMs = 300
testHooks.extraToolSpecs = [
  // Conversation-only without a browser_ name: hidden while the agent browser is off.
  {
    name: 'fixture_flagged',
    description: 'A fixture',
    read_only: true,
    conversation_only: true,
    input_schema: noArguments,
  },
  // A browser_ name without the flag: offered whether or not the browser is on.
  { name: 'browser_unflagged', description: 'A fixture', read_only: true, input_schema: noArguments },
]

await import('./index.js')
