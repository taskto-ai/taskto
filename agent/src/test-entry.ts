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

const modelPauseMs = Number(process.env.TASKTO_AGENT_TEST_MODEL_PAUSE_MS ?? 0)
if (modelPauseMs > 0) {
  testHooks.beforeModelWrite = () => {
    console.error('test: the model write is waiting before its commit')
    return new Promise((resolve) => setTimeout(resolve, modelPauseMs))
  }
}

await import('./index.js')
