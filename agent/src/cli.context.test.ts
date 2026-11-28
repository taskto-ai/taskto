import { describe, expect, it } from 'vitest'
import { splitTurn } from './cli.js'

const user = (text: string) => ({ role: 'user', content: text })
const answer = (text: string) => ({ role: 'assistant', content: [{ type: 'text', text }] })
const toolCall = (name: string) => ({ role: 'assistant', content: [{ type: 'toolCall', name, arguments: {} }] })
const result = (toolName: string, text: string) => ({
  role: 'toolResult',
  toolName,
  content: [{ type: 'text', text }],
})
const cliCall = { role: 'assistant', content: [] }
const missing = { role: 'toolResult', toolName: 'cli_turn', content: [], details: { reason: 'missing_result' } }

describe('splitTurn', () => {
  it('takes the latest user messages as the prompt and the earlier text as history', () => {
    const { prompt, history } = splitTurn([user('one'), answer('two'), user('three'), cliCall, missing])
    expect(prompt).toBe('three')
    expect(history).toEqual([
      { role: 'user', text: 'one' },
      { role: 'assistant', text: 'two' },
    ])
  })

  it('gives a CLI that continues after a finished tool call the request and the tool result', () => {
    const { prompt, history } = splitTurn([
      user('earlier'),
      answer('earlier answer'),
      user('check the disk'),
      toolCall('df'),
      result('df', '61G free'),
      cliCall,
      missing,
    ])
    expect(prompt).toContain('check the disk')
    expect(prompt).toContain('df: 61G free')
    expect(prompt).toMatch(/do not run them again/i)
    expect(history).toEqual([
      { role: 'user', text: 'earlier' },
      { role: 'assistant', text: 'earlier answer' },
    ])
  })

  it('keeps every finished result of a request, in order', () => {
    const { prompt } = splitTurn([
      user('go'),
      toolCall('a'),
      result('a', 'first'),
      toolCall('b'),
      result('b', 'second'),
      cliCall,
      missing,
    ])
    expect(prompt.indexOf('a: first')).toBeGreaterThan(prompt.indexOf('go'))
    expect(prompt.indexOf('b: second')).toBeGreaterThan(prompt.indexOf('a: first'))
  })

  it('never lists a call with no stored result among the finished ones', () => {
    const { prompt } = splitTurn([
      user('go'),
      {
        role: 'assistant',
        content: [
          { type: 'toolCall', name: 'a', arguments: {} },
          { type: 'toolCall', name: 'b', arguments: {} },
        ],
      },
      { role: 'toolResult', toolName: 'a', content: [], details: { reason: 'missing_result' } },
      result('b', 'second'),
      cliCall,
      missing,
    ])
    expect(prompt).toContain('b: second')
    expect(prompt).not.toMatch(/- a:/)
  })

  it('leaves the prompt empty when nothing but an answer follows the request', () => {
    expect(splitTurn([user('hi'), answer('hello')]).prompt).toBe('')
  })

  it('does not put the tool results of an earlier, answered request in the prompt', () => {
    const { prompt } = splitTurn([
      user('first'),
      toolCall('df'),
      result('df', 'old output'),
      answer('done'),
      user('second'),
      cliCall,
      missing,
    ])
    expect(prompt).toBe('second')
  })
})
