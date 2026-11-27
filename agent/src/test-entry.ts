import type { ToolSpec } from './taskto-tools.js'
import { testHooks } from './test-hooks.js'

const noArguments = { type: 'object', properties: {}, additionalProperties: false } as ToolSpec['input_schema']

testHooks.spikeTools = true
testHooks.walBusyMs = 300
testHooks.extraToolSpecs = [
  {
    name: 'fixture_flagged',
    description: 'A fixture',
    read_only: true,
    conversation_only: true,
    input_schema: noArguments,
  },
  { name: 'browser_unflagged', description: 'A fixture', read_only: true, input_schema: noArguments },
]

await import('./index.js')
