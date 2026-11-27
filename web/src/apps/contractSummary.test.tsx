// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { contract } from '../test/integrations'
import { ContractSummary } from './ContractSummary'

afterEach(cleanup)

test('two links with the same action and fields are two lines, with no duplicate key', () => {
  const error = vi.spyOn(console, 'error').mockImplementation(() => {})
  try {
    const link = contract.links?.[0]
    if (!link) throw new Error('the fixture has a link')
    render(<ContractSummary contract={{ ...contract, links: [link, { ...link, when: undefined }] }} />)
    expect(screen.getAllByText(/it asks for a payment link for the amount in “total”/)).toHaveLength(2)
    const keyWarnings = error.mock.calls.filter((c) => String(c[0]).includes('same key'))
    expect(keyWarnings).toEqual([])
  } finally {
    error.mockRestore()
  }
})
