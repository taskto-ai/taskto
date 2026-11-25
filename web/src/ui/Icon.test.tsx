// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'
import { Icon as ShellIcon } from '../shell/Icon'
import { ICON_NAMES, Icon, isDirectional } from './Icon'

afterEach(cleanup)

describe('Icon', () => {
  test('is decoration, hidden from screen readers, unless it has a label', () => {
    const { container } = render(<Icon name="check" />)
    expect(container.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
    render(<Icon name="alert" label="Overdue" />)
    expect(screen.getByRole('img', { name: 'Overdue' })).toBeTruthy()
  })

  test('draws the same line at every size', () => {
    const widths = ([12, 14, 16, 18, 20, 24] as const).map((size) => {
      const { container } = render(<Icon name="plus" size={size} />)
      const svg = container.querySelector('svg') as SVGElement
      expect(svg.getAttribute('width')).toBe(String(size))
      // On screen: the stroke in the grid's units, scaled by size / 24.
      return (Number(svg.getAttribute('stroke-width')) * size) / 24
    })
    for (const w of widths) expect(w).toBeCloseTo(1.5)
  })

  test("has the prototype's icons", () => {
    for (const name of [
      'chevron-down',
      'chevron-end',
      'more',
      'check',
      'clock',
      'block',
      'filter',
      'search',
      'folder',
      'file',
      'upload',
      'info',
      'alert',
      'copy',
      'move',
      'offline',
    ]) {
      expect(ICON_NAMES).toContain(name)
    }
  })

  test('every icon is one path on the 24×24 grid', () => {
    for (const name of ICON_NAMES) {
      const { container } = render(<Icon name={name} />)
      const paths = container.querySelectorAll('path')
      expect(paths.length, name).toBe(1)
      const numbers = (paths[0].getAttribute('d') ?? '').match(/-?\d*\.?\d+/g)?.map(Number) ?? []
      expect(numbers.length, name).toBeGreaterThan(0)
    }
  })

  test('arrows along the line are mirrored right to left, others are not', () => {
    expect(isDirectional('back')).toBe(true)
    expect(isDirectional('chevron-end')).toBe(true)
    expect(isDirectional('send')).toBe(true)
    expect(isDirectional('chevron-down')).toBe(false)
    expect(isDirectional('upload')).toBe(false)
    const { container } = render(<Icon name="back" />)
    expect(container.querySelector('svg')?.classList.contains('ui-icon-directional')).toBe(true)
  })

  test('the shell path still works, with the class its screens style', () => {
    const { container } = render(<ShellIcon name="menu" />)
    const svg = container.querySelector('svg') as SVGElement
    expect(svg.classList.contains('tt-icon')).toBe(true)
    expect(svg.classList.contains('ui-icon')).toBe(true)
    // As before: 20px, a 1.8 stroke in the grid's units.
    expect(svg.getAttribute('width')).toBe('20')
    expect(svg.getAttribute('stroke-width')).toBe('1.8')
  })
})
