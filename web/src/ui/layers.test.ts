import { afterEach, expect, test } from 'vitest'
import { modalClosed, modalOpened, modalReopened, topModal } from './layers'

const a = { id: 'a' } as unknown as HTMLElement
const b = { id: 'b' } as unknown as HTMLElement

afterEach(() => {
  modalClosed(a)
  modalClosed(b)
})

test('a modal the browser closed and that opened again is the topmost again', () => {
  modalOpened(a)
  modalOpened(b)
  expect(topModal()).toBe(b)
  modalReopened(a)
  expect(topModal()).toBe(a)
  modalClosed(a)
  expect(topModal()).toBe(b)
})
