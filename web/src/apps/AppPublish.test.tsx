// @vitest-environment jsdom
//
// The panel before a publish says what the draft's effects do outside its records, from the
// Inspection template's own actions, with where they go as the person may see it.

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import inspection from '../../../internal/apps/templates/inspection.json'
import type { App, AppDraft, EffectTarget, Space } from '../api'
import { PublishPanel } from './AppPublish'

afterEach(cleanup)

const team: Space = {
  id: 3,
  workspace_id: 1,
  kind: 'shared',
  name: 'Team',
  archived_at: null,
  role: 'editor',
  access: 'write',
}

const all = {
  roles: ['builder', 'publisher', 'user', 'data_manager', 'reader'],
  restricted: false,
  view: true,
  design: true,
  publish: true,
  submit: true,
  manage_roles: true,
  read_records: 'all',
  modify_records: 'all',
}

// The Inspection template with its board chosen: a board 12 of space 7.
function inspections(): AppDraft {
  const draft = structuredClone(inspection.definition) as unknown as AppDraft
  for (const action of Object.values(draft.actions ?? {})) {
    for (const e of action.effects ?? []) {
      if (e.kind === 'create_card') Object.assign(e, { space_id: 7, board_id: 12 })
    }
  }
  return draft
}

function panel(draft: AppDraft, targets?: EffectTarget[]) {
  const app = {
    id: 9,
    space_id: 3,
    name: 'Inspection',
    archived_at: null,
    version: 1,
    rows: 0,
    access: all,
    live: null,
    unpublished_changes: true,
    draft,
    effect_targets: targets,
  } as unknown as App
  render(<PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={team} onChanged={() => undefined} />)
}

const NOTHING_ELSE = 'It can’t reach anything else: no other app, document or account.'

test('an app with effects lists each, and doesn’t say it reaches nothing else', () => {
  panel(inspections(), [{ space_id: 7, space: 'Field team', space_kind: 'shared', board_id: 12, board: 'Maintenance' }])
  expect(screen.getByText('Creates a card on Maintenance (Field team), only when result is “fail”.')).toBeTruthy()
  expect(screen.queryByText(NOTHING_ELSE)).toBeNull()
})

test('a target the person can’t read is a space or board they can’t see', () => {
  panel(inspections(), [{ space_id: 7, board_id: 12 }])
  expect(screen.getByText('Creates a card on a board you can’t see, only when result is “fail”.')).toBeTruthy()
})

test('an app without effects still says it reaches nothing else', () => {
  const draft = inspections()
  for (const action of Object.values(draft.actions ?? {})) delete action.effects
  panel(draft)
  expect(screen.getByText(NOTHING_ELSE)).toBeTruthy()
})

test('every kind of effect is told, with its rule or its place', () => {
  const draft = inspections()
  draft.actions = {
    a: {
      kind: 'insert',
      effects: [
        {
          kind: 'create_card',
          name: 'fix',
          space_id: 7,
          board_id: 12,
          when: {
            all: [
              { field: 'result', op: 'eq', value: 'fail' },
              {
                any: [
                  { field: 'level', op: 'gte', value: 3 },
                  { field: 'urgent', op: 'eq', value: true },
                ],
              },
            ],
          },
        },
        { kind: 'create_document', space_id: 7, path: 'inspections/{{id}}.md' },
        { kind: 'open_route', route: 'card', card: 'fix' },
        { kind: 'open_route', route: 'board', space_id: 8, board_id: 1 },
        { kind: 'open_route', route: 'document', space_id: 7, path: 'inspections/index.md' },
      ],
    },
    b: { kind: 'update', effects: [{ kind: 'update_card', card: 'fix', column: 'done' }] },
  }
  panel(draft, [
    { space_id: 7, space: 'Field team', space_kind: 'shared', board_id: 12, board: 'Maintenance' },
    { space_id: 7, space: 'Field team', space_kind: 'shared' },
    { space_id: 8 },
  ])
  const lines = screen.getAllByRole('listitem').map((li) => li.textContent)
  expect(lines).toEqual(
    expect.arrayContaining([
      'Creates a card on Maintenance (Field team), only when result is “fail” and (level is at least 3 or urgent is true).',
      'Creates a document at inspections/{{id}}.md in Field team.',
      'Opens the card it made on Maintenance (Field team).',
      'Opens a board you can’t see.',
      'Opens the document inspections/index.md in Field team.',
      'Moves the card it made on Maintenance (Field team) to Done.',
    ]),
  )
})
