import { createModels } from '@earendil-works/pi-ai/models'
import { afterEach, describe, expect, it } from 'vitest'
import { installScriptModel, SCRIPT_MODEL_ID } from './model.js'
import { testHooks } from './test-hooks.js'

async function reply(prompt: string) {
  const models = createModels()
  installScriptModel(models, 'script')
  const model = models.getModel('script', SCRIPT_MODEL_ID)
  if (!model) throw new Error('no scripted model')
  return models.completeSimple(model, { messages: [{ role: 'user', content: prompt, timestamp: Date.now() }] })
}

describe('the scripted model', () => {
  afterEach(() => {
    testHooks.scriptFailures = false
  })

  it('only echoes a failure prompt when the test entry has not turned failures on', async () => {
    const message = await reply('fail:script')
    expect(message.stopReason).not.toBe('error')
    expect(JSON.stringify(message.content)).toContain('echo: fail:script')
  })

  it('fails on a failure prompt once the test entry has turned failures on', async () => {
    testHooks.scriptFailures = true
    const message = await reply('fail:script')
    expect(message.stopReason).toBe('error')
    expect(message.errorMessage).toBe('rate limit reached')
  })

  it('fails after a finished tool call only once the test entry has turned failures on', async () => {
    const asked = [
      { role: 'user' as const, content: 'fail-after-tool:script;tool:pause_safe:10', timestamp: 1 },
      {
        role: 'toolResult' as const,
        toolCallId: 't',
        toolName: 'pause_safe',
        content: [],
        isError: false,
        timestamp: 2,
      },
    ]
    const models = createModels()
    installScriptModel(models, 'script')
    const model = models.getModel('script', SCRIPT_MODEL_ID)
    if (!model) throw new Error('no scripted model')
    const plain = await models.completeSimple(model, { messages: asked })
    expect(plain.stopReason).not.toBe('error')
    testHooks.scriptFailures = true
    const failing = await models.completeSimple(model, { messages: asked })
    expect(failing.stopReason).toBe('error')
  })

  it('leaves the test entry alone: only it turns failures on', async () => {
    expect(testHooks.scriptFailures).toBe(false)
  })
})
