// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'
import { AgentDot } from './AgentDot'
import { Avatar, initials } from './Avatar'
import { Badge } from './Badge'
import { Label, labelHue, normalizeLabelName } from './Label'
import { LABEL_HUES } from './roles'
import { Skeleton } from './Skeleton'

afterEach(cleanup)

describe('labelHue', () => {
  test('is one of the six hues, the same for the same name however it is written', () => {
    expect(LABEL_HUES).toContain(labelHue('launch'))
    expect(labelHue('Launch')).toBe(labelHue('launch'))
    expect(labelHue('  launch ')).toBe(labelHue('launch'))
    expect(labelHue('café')).toBe(labelHue('café'))
  })

  test('is fixed: a change to the function would recolour every board', () => {
    expect(
      ['launch', 'writing', 'design', 'copy', 'onboarding', 'i18n', 'bug', 'urgent'].map((n) => labelHue(n)),
    ).toEqual(['amber', 'pink', 'teal', 'violet', 'green', 'blue', 'blue', 'green'])
  })

  test("doesn't depend on the locale: Turkish dotted and dotless i are lower-cased the same everywhere", () => {
    expect(labelHue('IDEAS')).toBe(labelHue('ideas'))
  })

  test('spreads typical names over all six hues', () => {
    const names = [
      'bug',
      'feature',
      'design',
      'copy',
      'launch',
      'writing',
      'onboarding',
      'i18n',
      'urgent',
      'blocked',
      'backend',
      'frontend',
      'mobile',
      'docs',
      'research',
      'sales',
      'marketing',
      'legal',
      'finance',
      'ops',
      'security',
      'performance',
      'accessibility',
      'billing',
      'support',
      'infra',
      'q1',
      'q2',
      'v2',
      'hiring',
      'customer',
      'idea',
      'later',
      'tech debt',
      'cliente',
      'campanha',
    ]
    const counts = new Map<string, number>()
    for (const n of names) counts.set(labelHue(n), (counts.get(labelHue(n)) ?? 0) + 1)
    expect([...counts.keys()].sort()).toEqual([...LABEL_HUES].sort())
    for (const [hue, count] of counts) expect(count, hue).toBeLessThanOrEqual((names.length / LABEL_HUES.length) * 2)
  })
})

describe('normalizeLabelName', () => {
  const SERVER: [string, string][] = [
    [' Tech\u0009Debt\u000a', 'tech debt'],
    ['K', 'k'],
    ['STRASSE', 'strasse'],
    ['Stra\u00dfe', 'strasse'],
    ['cafe\u0301', 'caf\u00e9'],
    ['caf\u00e9', 'caf\u00e9'],
    ['tech  debt', 'tech debt'],
    ['tech\u00a0debt', 'tech debt'],
    ['tech\u3000debt', 'tech debt'],
    ['\u00b5', '\u03bc'],
    ['\u0130stanbul', 'i\u0307stanbul'],
    ['\u0131i', '\u0131i'],
    ['\u017f', 's'],
    ['\u01c4', '\u01c6'],
    ['\u03a3\u038a\u03a3\u03a5\u03a6\u039f\u03a3', '\u03c3\u03af\u03c3\u03c5\u03c6\u03bf\u03c3'],
    ['\u03c3\u03af\u03c3\u03c5\u03c6\u03bf\u03c2', '\u03c3\u03af\u03c3\u03c5\u03c6\u03bf\u03c3'],
    ['\u1e9e', 'ss'],
    ['\ufb03', 'ffi'],
  ]

  test('is what the server keeps: white space, full case folding, NFC', () => {
    for (const [name, kept] of SERVER) expect(normalizeLabelName(name), JSON.stringify(name)).toBe(kept)
  })

  test('names the server keeps as one label get one hue', () => {
    expect(labelHue('tech  debt')).toBe(labelHue('tech debt'))
    expect(labelHue('tech\u00a0debt')).toBe(labelHue('tech debt'))
    expect(labelHue('Stra\u00dfe')).toBe(labelHue('STRASSE'))
    expect(labelHue('\u03c3\u03af\u03c3\u03c5\u03c6\u03bf\u03c2')).toBe(
      labelHue('\u03a3\u038a\u03a3\u03a5\u03a6\u039f\u03a3'),
    )
  })
})

describe('Label', () => {
  test('shows its name in its hue', () => {
    render(<Label name="launch" />)
    const el = screen.getByText('launch')
    expect(el.className).toContain(`ui-hue-${labelHue('launch')}`)
  })
})

describe('Badge', () => {
  test('says its status in words, with the dot hidden', () => {
    const { container } = render(
      <Badge tone="success" dot>
        Live
      </Badge>,
    )
    expect(screen.getByText('Live').className).toContain('ui-badge-success')
    expect(container.querySelector('.ui-badge-dot')?.getAttribute('aria-hidden')).toBe('true')
  })

  test('is neutral by default', () => {
    render(<Badge>Offline</Badge>)
    expect(screen.getByText('Offline').className).toContain('ui-badge-neutral')
  })
})

describe('Avatar', () => {
  test('initials: the first letters of the first and last words, whole characters', () => {
    expect(initials('Ana Ribeiro')).toBe('AR')
    expect(initials('Ana Paula Ribeiro')).toBe('AR')
    expect(initials('ana')).toBe('A')
    expect(initials('  ')).toBe('')
    expect(initials('Élodie Ödegaard')).toBe('ÉÖ')
    expect(initials('😀 Smile')).toBe('😀S')
    expect(initials('E\u0301lodie O\u0308degaard')).toBe('E\u0301O\u0308')
    expect(initials('👩\u200d💻 Smith')).toBe('👩\u200d💻S')
  })

  test('is named for a screen reader, or decorative beside the name', () => {
    render(<Avatar name="Ana Ribeiro" />)
    expect(screen.getByRole('img', { name: 'Ana Ribeiro' }).textContent).toBe('AR')
    cleanup()
    const { container } = render(<Avatar name="Ana Ribeiro" decorative shape="square" size="lg" />)
    const el = container.firstElementChild as HTMLElement
    expect(el.getAttribute('aria-hidden')).toBe('true')
    expect(el.className).toContain('ui-avatar-square')
    expect(el.className).toContain('ui-avatar-lg')
  })

  test("takes its name's hue, as a label of that name does", () => {
    const { container } = render(<Avatar name="Ana Ribeiro" />)
    expect(container.firstElementChild?.className).toContain(`ui-hue-${labelHue('Ana Ribeiro')}`)
  })
})

describe('AgentDot and Skeleton', () => {
  test('the dot is hidden unless it stands alone with a label', () => {
    const { container } = render(<AgentDot />)
    expect(container.firstElementChild?.getAttribute('aria-hidden')).toBe('true')
    render(<AgentDot label="The agent" />)
    expect(screen.getByRole('img', { name: 'The agent' })).toBeTruthy()
  })

  test('a skeleton is hidden and takes its width', () => {
    const { container } = render(<Skeleton width="70%" />)
    const el = container.firstElementChild as HTMLElement
    expect(el.getAttribute('aria-hidden')).toBe('true')
    expect(el.style.inlineSize).toBe('70%')
  })
})
