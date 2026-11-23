// @vitest-environment jsdom
//
// Apps with several screens, drawn from the golden messages the Go server sends for the bundled Task
// planner (internal/apps/screens_test.go): the host draws the screen the data model names,
// navigating only changes that, so the form keeps what was typed, and Show hides an input until
// its rule holds.

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import openDetail from '../../../internal/apps/testdata/planner.open-detail.json'
import openNew from '../../../internal/apps/testdata/planner.open-new.json'
import openTasks from '../../../internal/apps/testdata/planner.open-tasks.json'
import surface from '../../../internal/apps/testdata/planner.surface.json'
import { type ActionPayload, AppView } from './AppView'
import { DEFINITION_VERSION } from './catalog'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// What the server answers each navigate action with.
const answers: Record<string, unknown[]> = {
  new_task: openNew,
  back_to_tasks: openTasks,
  open_task: openDetail,
}

function planner(sent: ActionPayload[] = []) {
  return (
    <AppView
      surfaceId="app-1"
      label="Task planner"
      release={{ definition_version: DEFINITION_VERSION, requires: ['component.Screens', 'component.Show'] }}
      transport={{
        // Fresh messages each time, as a response is: the data model keeps the objects it is given.
        load: async () => structuredClone(surface),
        act: async (action) => {
          sent.push(action)
          return structuredClone(answers[action.name] ?? [])
        },
      }}
    />
  )
}

// A2UI's inputs don't link their <label> to their <input>: find the input by its label's text.
function input(label: string): HTMLInputElement {
  const found = screen.getByText(label).parentElement?.querySelector('input')
  if (!found) throw new Error(`no input for ${label}`)
  return found as HTMLInputElement
}

test('draws the entry screen of an app with screens, and no other', async () => {
  render(planner())
  await screen.findByText('Tasks')
  expect(screen.getByText('Pay rent')).toBeTruthy()
  expect(screen.getByText('Fix the shelf')).toBeTruthy()
  // The open count is summarised over the rows not done.
  expect(screen.getByText('Open tasks').closest('dl')?.querySelector('dd')?.textContent).toBe('1')
  expect(screen.queryByText('Add task')).toBeNull()
  expect(screen.queryByText('Mark done')).toBeNull()
})

test('navigating opens the screen the server names, and the form keeps what was typed', async () => {
  const sent: ActionPayload[] = []
  render(planner(sent))
  await screen.findByText('Tasks')

  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  await screen.findByRole('heading', { name: 'New task' })
  expect(screen.queryByText('Open tasks')).toBeNull()
  fireEvent.input(input('Title'), { target: { value: 'Draft a plan' } })
  await waitFor(() => expect(input('Title').value).toBe('Draft a plan'))

  // Back to the list and again to the form: the draft is where it was left.
  fireEvent.click(screen.getByRole('button', { name: 'Back' }))
  await screen.findByText('Open tasks')
  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  await screen.findByRole('heading', { name: 'New task' })
  expect(input('Title').value).toBe('Draft a plan')
  expect(sent.map((a) => a.name)).toEqual(['new_task', 'back_to_tasks', 'new_task'])
})

test('a row opens its detail screen with its id, and the screen shows the record', async () => {
  const sent: ActionPayload[] = []
  render(planner(sent))
  await screen.findByText('Pay rent')
  fireEvent.click(screen.getAllByRole('button', { name: 'Open' })[1])
  await screen.findByRole('heading', { name: 'Fix the shelf' })
  expect(sent[0]).toMatchObject({ name: 'open_task', context: { id: 2 } })
  // The record's category is "other", so the rule shows its note.
  expect(screen.getByText('Kind').closest('dl')?.querySelector('dd')?.textContent).toBe('errand')
  expect(screen.getByRole('button', { name: 'Mark done' })).toBeTruthy()
})

test('an input is shown only while its rule holds', async () => {
  render(planner())
  await screen.findByText('Tasks')
  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  await screen.findByRole('heading', { name: 'New task' })
  expect(screen.queryByText('What kind of task?')).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Other' }))
  await screen.findByText('What kind of task?')
  fireEvent.click(screen.getByRole('button', { name: 'Home' }))
  await waitFor(() => expect(screen.queryByText('What kind of task?')).toBeNull())
})

test('the time and date inputs are native, labelled inputs', async () => {
  render(planner())
  await screen.findByText('Tasks')
  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  await screen.findByRole('heading', { name: 'New task' })
  expect(screen.getByLabelText('Due time')).toHaveProperty('type', 'time')
  expect(screen.getByLabelText('Due date')).toHaveProperty('type', 'date')
})

test('the choices are a named group, and every input has the name of its label', async () => {
  render(planner())
  await screen.findByText('Tasks')
  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  await screen.findByRole('heading', { name: 'New task' })
  expect(screen.getByRole('group', { name: 'Category' })).toBeTruthy()
  expect(screen.getByRole('textbox', { name: 'Title' })).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Other' }))
  expect(await screen.findByRole('textbox', { name: 'What kind of task?' })).toBeTruthy()
})

test('a release of definition version 1 is still drawn by a client that reads version 2', async () => {
  render(
    <AppView
      surfaceId="app-1"
      label="Task planner"
      release={{ definition_version: 1, requires: ['component.Text'] }}
      transport={{ load: async () => structuredClone(surface), act: async () => [] }}
    />,
  )
  await screen.findByText('Tasks')
  expect(screen.queryByText('Update required')).toBeNull()
})

test('a client that reads version 2 asks for an update for version 3, and for a capability it lacks', async () => {
  render(
    <AppView
      surfaceId="app-1"
      label="Task planner"
      release={{ definition_version: DEFINITION_VERSION + 1, requires: ['component.Screens', 'component.Timeline'] }}
      transport={{ load: async () => structuredClone(surface), act: async () => [] }}
    />,
  )
  await screen.findByText('Update required')
  expect(screen.getByText(`Needs: definition_version.${DEFINITION_VERSION + 1}, component.Timeline`)).toBeTruthy()
})
