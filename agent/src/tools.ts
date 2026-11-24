// Spike tools: a pause that streams progress, once replay-safe and once not, to see what
// a crash in the middle of a tool call does to each. Only the integration tests use them (the
// scripted model calls them), and only through test-entry.ts, which turns them on in-process
// (test-hooks.ts). No environment variable or setting of the server's does: the production model
// is never shown them.
import { defineExtension, defineTool } from '@earendil-works/pi-durable'
import { num, obj } from '@earendil-works/pi-schema'

function pause(name: string, replay: 'safe' | 'unsafe') {
  return defineTool({
    name,
    description: `Wait for ms milliseconds, reporting progress (replay: ${replay})`,
    parameters: obj({ ms: num() }),
    replay,
    execute: async (args, api, context) => {
      const step = 200
      for (let waited = 0; waited < args.ms; waited += step) {
        if (context.abortSignal?.aborted) throw new Error('aborted')
        api.output(`waited ${waited}ms\n`)
        await new Promise((resolve) => setTimeout(resolve, step))
      }
      return { output: [{ type: 'text', text: `paused ${args.ms}ms` }] }
    },
  })
}

export const SpikeTools = defineExtension({
  name: 'spike-tools',
  tools: [pause('pause_safe', 'safe'), pause('pause_once', 'unsafe')],
})
