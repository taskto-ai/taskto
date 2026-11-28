import { expect, test } from 'vitest'
import { agentChoices, agentLabel, agentToken, leadingAgent, matchingAgents, mentionQuery } from './agentMention'

const providers = [
  { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
  {
    provider: 'cli-codex',
    name: 'Codex',
    scope: 'cli',
    models: [
      { id: 'default', name: 'Default' },
      { id: 'gpt-5', name: 'GPT 5' },
    ],
  },
  { provider: 'u2-home', name: 'My Ollama', scope: 'user', models: [{ id: 'llama/3', name: 'Llama' }] },
]

test('the choices list the CLI agents first and leave out what a token cannot name', () => {
  const choices = agentChoices(providers, (p, m) => `${p}: ${m}`)
  expect(choices.map((c) => [c.label, c.menu, c.ref.provider, c.ref.model_id])).toEqual([
    ['Codex', 'Codex: Default', 'cli-codex', 'default'],
    ['Codex: GPT 5', 'Codex: GPT 5', 'cli-codex', 'gpt-5'],
    ['Scripted', 'Scripted', 'script', 'script-1'],
  ])
})

test('a token round-trips through the parser, which only reads one at the very start', () => {
  const [codex] = agentChoices(providers, (p, m) => `${p}: ${m}`)
  expect(agentToken(codex)).toBe('@[Codex](agent:cli-codex/default)')
  expect(leadingAgent(`${agentToken(codex)} review this`)).toEqual({
    label: 'Codex',
    ref: { provider: 'cli-codex', model_id: 'default' },
    rest: 'review this',
  })
  expect(leadingAgent(agentToken(codex))?.rest).toBe('')
  expect(leadingAgent(` ${agentToken(codex)} hi`)).toBeNull()
  expect(leadingAgent(`hi ${agentToken(codex)}`)).toBeNull()
  expect(leadingAgent(`${agentToken(codex)}hi`)).toBeNull()
  expect(leadingAgent('@[Codex](user:3) hi')).toBeNull()
})

test('a label is counted in characters, as the server counts it, not in UTF-16 units', () => {
  const emoji = (n: number) => '\u{1F600}'.repeat(n)
  const token = (label: string) => `@[${label}](agent:cli-claude/default) hi`
  expect(leadingAgent(token(emoji(51)))?.ref).toEqual({ provider: 'cli-claude', model_id: 'default' })
  expect(leadingAgent(token(emoji(100)))?.rest).toBe('hi')
  expect(leadingAgent(token(emoji(101)))).toBeNull()
  expect(leadingAgent(token(emoji(0)))).toBeNull()
})

test('a label never breaks the token', () => {
  expect(agentLabel('A [b]\\c\nd')).toBe('A  b  c d')
  expect(agentLabel('x'.repeat(150))).toHaveLength(100)
})

test('the menu filters by what follows the @', () => {
  const choices = agentChoices(providers, (p, m) => `${p}: ${m}`)
  expect(mentionQuery('@')).toBe('')
  expect(mentionQuery('@Co')).toBe('co')
  expect(mentionQuery('@Codex review')).toBeNull()
  expect(mentionQuery('hi @')).toBeNull()
  expect(mentionQuery('@[x](agent:a/b)')).toBeNull()
  expect(matchingAgents(choices, 'gpt').map((c) => c.label)).toEqual(['Codex: GPT 5'])
  expect(matchingAgents(choices, '')).toHaveLength(3)
})
