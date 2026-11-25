// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'
import { AgentDot } from './AgentDot'
import { Avatar, initials } from './Avatar'
import { Badge } from './Badge'
import { Label, labelHue } from './Label'
import { LABEL_HUES } from './roles'
import { Skeleton } from './Skeleton'

afterEach(cleanup)

describe('labelHue', () => {
  test('is one of the six hues, the same for the same name however it is written', () => {
    expect(LABEL_HUES).toContain(labelHue('launch'))
    expect(labelHue('Launch')).toBe(labelHue('launch'))
    expect(labelHue('  launch ')).toBe(labelHue('launch'))
    // "é" precomposed and as "e" with a combining accent.
    expect(labelHue('café')).toBe(labelHue('café'))
  })

  test('is fixed: a change to the function would recolour every board', () => {
    expect(
      ['launch', 'writing', 'design', 'copy', 'onboarding', 'i18n', 'bug', 'urgent'].map((n) => labelHue(n)),
    ).toEqual(['amber', 'pink', 'teal', 'violet', 'green', 'blue', 'blue', 'green'])
  })

  test("doesn't depend on the locale: Turkish dotted and dotless i are lower-cased the same everywhere", () => {
    // toLowerCase, not toLocaleLowerCase: "I" is "i" on every machine.
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
    // Every hue is used, and none takes more than about twice its share.
    expect([...counts.keys()].sort()).toEqual([...LABEL_HUES].sort())
    for (const [hue, count] of counts) expect(count, hue).toBeLessThanOrEqual((names.length / LABEL_HUES.length) * 2)
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
