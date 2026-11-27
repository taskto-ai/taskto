import { expect, test } from 'vitest'
import { LIVE_APPS, lastSavedApp, liveApps } from './savedApps'
import type { ChatMessage, ToolUse } from './transcript'

const save = (key: string, app: number): ToolUse => ({ id: `c${key}`, key, name: 'update_app', app: { space: 2, app } })
const message = (key: string, ...tools: ToolUse[]): ChatMessage => ({ key, role: 'assistant', text: '', tools })

test('each app shows launched once, at its latest save, for the most recently saved apps only', () => {
  const first = save('1', 1)
  const again = save('2', 1)
  const others = Array.from({ length: LIVE_APPS }, (_, i) => save(`${3 + i}`, 10 + i))
  const live = liveApps([message('1', first), message('2', again, ...others)])
  expect(live.has(first)).toBe(false)
  expect([...live]).toEqual([...others].reverse())
  expect(live.has(again)).toBe(false)
})

test('the last saved app, counting only saves after a given entry', () => {
  const messages = [message('1', save('1', 1)), message('4', save('4', 2), { id: 'c5', key: '5', name: 'search' })]
  expect(lastSavedApp(messages)).toEqual({ app: { space: 2, app: 2 }, save: '4' })
  expect(lastSavedApp(messages, 3)).toEqual({ app: { space: 2, app: 2 }, save: '4' })
  expect(lastSavedApp(messages, 4)).toBeNull()
  expect(lastSavedApp([])).toBeNull()
})
