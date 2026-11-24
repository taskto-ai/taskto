// Spike tools: a pause that streams progress, once replay-safe and once not, to see what
// a crash in the middle of a tool call does to each. Only the integration tests use them (the
// scripted model calls them), so the sidecar installs them only when SPIKE_TOOLS_ENV is "1":
// the production model is never shown them.
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

/** The environment variable that turns the spike tools on; the integration tests set it. */
export const SPIKE_TOOLS_ENV = 'TASKTO_AGENT_SPIKE_TOOLS'

export function spikeToolsEnabled(env: NodeJS.ProcessEnv = process.env): boolean {
  return env[SPIKE_TOOLS_ENV] === '1'
}

export const SpikeTools = defineExtension({
  name: 'spike-tools',
  tools: [pause('pause_safe', 'safe'), pause('pause_once', 'unsafe')],
})
