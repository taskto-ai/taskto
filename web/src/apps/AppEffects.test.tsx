// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { AppEffect } from '../api'
import { fakeFetch } from '../test/fakes'
import { AppEffects, EffectPreview } from './AppEffects'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

const base = '/api/workspaces/1/spaces/2/apps/9'

const effect = (id: number, over: Partial<AppEffect> = {}): AppEffect => ({
  id,
  operation: 'op-0000001',
  position: 0,
  kind: 'create_card',
  action: 'submit_inspection',
  record: 14,
  record_revision: 1,
  status: 'pending',
  attempts: 0,
  next_attempt_at: null,
  error: null,
  detail: { title: 'Fix East gate', column: 'todo' },
  done_at: null,
  can_retry: false,
  ...over,
})

test('an app that set nothing going shows nothing of effects', async () => {
  fakeFetch({ [`GET ${base}/effects?limit=50`]: { body: { effects: [], active: 0, failed: 0 } } })
  const { container } = render(<AppEffects workspace={1} base={base} writable />)
  await waitFor(() => expect(container.querySelector('.tt-effects')).toBeNull())
  expect(screen.queryByRole('button', { name: 'Effects' })).toBeNull()
})

test('shows what waits and what failed, and why', async () => {
  fakeFetch({
    [`GET ${base}/effects?limit=50`]: {
      body: {
        effects: [
          effect(3, { kind: 'create_document', detail: { path: 'inspections/14.md' }, status: 'pending' }),
          effect(2, {
            status: 'failed',
            attempts: 5,
            error: { code: 'app_effect_target_access' },
            can_retry: true,
          }),
          effect(1, {
            status: 'done',
            result: { space_id: 7, board_id: 12, card_id: 88 },
            done_at: '2026-11-02T10:00:00.000Z',
          }),
        ],
        active: 1,
        failed: 1,
      },
    },
  })
  render(<AppEffects workspace={1} base={base} writable />)
  // Collapsed, a failure is still said.
  await screen.findByRole('button', { name: /Effects/ })
  expect(screen.getByRole('status').textContent).toContain('1 failed')
  fireEvent.click(screen.getByRole('button', { name: /Effects/ }))

  const items = within(await screen.findByRole('list', { name: 'The app’s effects' })).getAllByRole('listitem')
  expect(items).toHaveLength(3)
  expect(items[0].textContent).toContain('Create a document')
  expect(items[0].textContent).toContain('Waiting')
  expect(items[0].textContent).toContain('inspections/14.md')
  expect(items[1].textContent).toContain('Failed')
  expect(items[1].textContent).toContain('can no longer write to the board or space')
  expect(items[1].textContent).toContain('5 tries')
  expect(items[2].textContent).toContain('Done')
  const link = within(items[2]).getByRole('link', { name: 'Card 88: open the boards' })
  expect(link.getAttribute('href')).toBe('/w/1/boards')
})

test('retries a failed effect on purpose, and tells the list to reload', async () => {
  let failed = true
  const calls = fakeFetch({
    [`GET ${base}/effects?limit=50`]: () => ({
      body: {
        effects: [
          effect(2, {
            status: failed ? 'failed' : 'pending',
            error: failed ? { code: 'app_effect_gave_up' } : null,
            can_retry: failed,
          }),
        ],
        active: failed ? 0 : 1,
        failed: failed ? 1 : 0,
      },
    }),
    [`POST ${base}/effects/2/retry`]: () => {
      failed = false
      return { body: effect(2, { status: 'pending' }) }
    },
  })
  render(<AppEffects workspace={1} base={base} writable />)
  fireEvent.click(await screen.findByRole('button', { name: /Effects/ }))
  fireEvent.click(await screen.findByRole('button', { name: 'Retry: Fix East gate' }))
  await screen.findByText('It will be tried again shortly.')
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({})
  await waitFor(() => expect(screen.queryByRole('button', { name: /Retry/ })).toBeNull())
  expect(screen.getByText('Waiting')).toBeTruthy()
})

test('offers a retry only to the person who acted, in a space they can write', async () => {
  fakeFetch({
    [`GET ${base}/effects?limit=50`]: {
      body: {
        effects: [
          effect(2, { status: 'failed', error: { code: 'app_effect_gave_up' }, can_retry: false }),
          effect(3, { status: 'failed', error: { code: 'app_effect_gave_up' }, can_retry: true }),
        ],
        active: 0,
        failed: 2,
      },
    },
  })
  const { unmount } = render(<AppEffects workspace={1} base={base} writable />)
  fireEvent.click(await screen.findByRole('button', { name: /Effects/ }))
  await screen.findByRole('list')
  expect(screen.getAllByRole('button', { name: /Retry/ })).toHaveLength(1)
  unmount()

  // A read-only space: none.
  render(<AppEffects workspace={1} base={base} writable={false} />)
  fireEvent.click(await screen.findByRole('button', { name: /Effects/ }))
  await screen.findByRole('list')
  expect(screen.queryByRole('button', { name: /Retry/ })).toBeNull()
})

test('a retry the server refuses is told, translated', async () => {
  fakeFetch({
    [`GET ${base}/effects?limit=50`]: {
      body: {
        effects: [effect(2, { status: 'failed', error: { code: 'app_effect_no_card' }, can_retry: true })],
        active: 0,
        failed: 1,
      },
    },
    [`POST ${base}/effects/2/retry`]: {
      status: 409,
      body: { error: 'only a failed effect can be retried', code: 'app_effect_not_failed' },
    },
  })
  render(<AppEffects workspace={1} base={base} writable />)
  fireEvent.click(await screen.findByRole('button', { name: /Effects/ }))
  fireEvent.click(await screen.findByRole('button', { name: /Retry/ }))
  await waitFor(() =>
    expect(screen.getAllByRole('alert').some((a) => a.textContent === 'Only a failed effect can be retried.')).toBe(
      true,
    ),
  )
})

test('keeps asking while an effect waits, and stops when none does', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  let polls = 0
  fakeFetch({
    [`GET ${base}/effects?limit=50`]: () => {
      polls++
      return polls < 2
        ? { body: { effects: [effect(1)], active: 1, failed: 0 } }
        : {
            body: {
              effects: [effect(1, { status: 'done', result: { space_id: 7, card_id: 1 } })],
              active: 0,
              failed: 0,
            },
          }
    },
  })
  render(<AppEffects workspace={1} base={base} writable />)
  fireEvent.click(await screen.findByRole('button', { name: /Effects/ }))
  await screen.findByText('Waiting')
  await vi.advanceTimersByTimeAsync(3100)
  await screen.findByText('Done')
  const seen = polls
  await vi.advanceTimersByTimeAsync(7000)
  expect(polls).toBe(seen)
})

test('a preview says that nothing was saved, and what would be set going', () => {
  const { unmount } = render(
    <EffectPreview
      effects={[
        effect(0, { status: 'simulated', detail: { title: 'Fix East gate (inspection new)' } }),
        effect(0, {
          status: 'simulated',
          kind: 'create_document',
          detail: { path: 'inspections/new.md' },
          error: { code: 'app_effect_target_access' },
        }),
      ]}
    />,
  )
  expect(screen.getByText('Preview: nothing was saved')).toBeTruthy()
  const items = screen.getAllByRole('listitem')
  expect(items[0].textContent).toContain('Simulated')
  expect(items[0].textContent).toContain('Fix East gate (inspection new)')
  expect(items[1].textContent).toContain('It would fail for you')
  expect(screen.queryByRole('button', { name: /Retry/ })).toBeNull()
  unmount()
  render(<EffectPreview effects={[]} />)
  expect(screen.getByText('This action would set nothing going.')).toBeTruthy()
})
