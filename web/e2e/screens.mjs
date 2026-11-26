// Captures every screen of the web app, for reviews of UI changes (`make screens`,
// docs/design.md#capturing-the-screens): the built server on an empty temporary data directory,
// sample data seeded through the HTTP API, and a real Chromium (Playwright) that visits each
// screen at three sizes in a light and a dark theme.
//
//   node web/e2e/screens.mjs [--out DIR] [--only TEXT[,TEXT...]] [--list] [--strict]
//
//   --out DIR      where the PNGs go (default web/screens/, which git ignores); TASKTO_SCREENS_OUT
//   --only TEXT    only the captures whose name (`desktop-light/boards`) contains one of the
//                  comma-separated texts; TASKTO_SCREENS_ONLY
//   --list         print the capture names and stop
//   --strict       also exit non-zero when the browser logged an error or a request failed
//   --unsafe-agent start the server with -unsafe-privileged-agents when it refuses the agent
//                  runtime (see below); TASKTO_SCREENS_UNSAFE_AGENT=1
//
// The files are named `<size>-<theme>/<screen>.png`, the same on every run, so two runs (before
// and after a change) can be compared file by file. Whatever the outcome, an interruption (Ctrl-C,
// SIGTERM, SIGHUP) included, the server is stopped and reaped, the browser closed and the data
// directory removed. It never runs in CI: it needs a built server (`make build`) and takes
// minutes; its parts (starting and stopping the server, the cleanup, the HTTP client, the output
// folder, the capture of one screen) are exported and tested without a browser (screens.test.mjs).
//
// A capture is only saved once the screen's own ready check is on the page (a heading, a control
// or a seeded item that only that screen shows), so a wrong page is a failed capture, never a
// picture. The script owns only the PNGs it writes, listed in `.screens-manifest.json` in the
// output folder: it never deletes another file.
//
// The agent runtime: a conversation with replies needs the server started with -agent, where a
// scripted offline model answers. The server refuses that when its user can run sudo without a
// password (docs/agent-safety.md). Then the script starts the server without the agent, captures
// the conversation list (empty), skips the conversation detail (it has no replies to show, so
// no PNG is made of it), and says so. --unsafe-agent (or TASKTO_SCREENS_UNSAFE_AGENT=1) passes
// -unsafe-privileged-agents, which turns that refusal into a warning: for a development machine
// only, never a server people use.
//
// Everything seeded is invented: example.com addresses and made-up names.

import { spawn } from 'node:child_process'
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { constants as osConstants, tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { parseArgs } from 'node:util'

const repo = fileURLToPath(new URL('../..', import.meta.url))

export const SIZES = [
  { name: 'desktop', width: 1440, height: 900 },
  { name: 'tablet', width: 820, height: 1100 },
  { name: 'phone', width: 390, height: 844 },
]
export const THEMES = ['light', 'dark']

const owner = {
  email: 'ana@example.com',
  name: 'Ana Ribeiro',
  password: 'sample-password-for-screens',
  workspace: 'Acme Studio',
}

// The second person of the sample data, who tells the owner of a card.
const colleague = { name: 'Bruno Costa', password: 'sample-colleague-password-for-screens' }
const INBOX_CARD = 'Review the launch checklist'

// An app whose one screen uses every component of the renderer the server's catalog allows (the
// renderer's theme is web/src/ui/a2ui.css): numbers, every kind of field, choices as chips and as
// boxes, a check box, a slider, dates, tabs, a card, a divider, a list, an icon, a modal and the
// three kinds of button, one of them disabled until the form is valid.
const COMPONENTS_APP_NAME = 'Every component'
const C = (id, component, props = {}) => ({ id, component, ...props })
const label = (id, text) => C(id, 'Text', { text })
const COMPONENTS_APP = {
  screens: [
    {
      name: 'main',
      root: 'main-root',
      components: [
        C('main-root', 'Column', {
          children: ['title', 'intro', 'stats', 'form-card', 'tabs', 'divider', 'list', 'more', 'actions'],
        }),
        C('title', 'Text', { text: 'Field visit', variant: 'h2' }),
        C('intro', 'Text', { text: 'Every control an app can draw, as the library draws it.', variant: 'caption' }),
        C('stats', 'Row', { children: ['stat-count', 'stat-total', 'stat-latest'] }),
        C('stat-count', 'Stat', { label: 'Visits', value: { path: '/count' } }),
        C('stat-total', 'Stat', { label: 'Hours logged', value: { path: '/total_hours' } }),
        C('stat-latest', 'Stat', { label: 'Status', value: 'On track' }),
        C('form-card', 'Card', { child: 'form' }),
        C('form', 'Column', {
          children: ['f-site', 'f-hours', 'f-notes', 'f-code', 'f-dates', 'f-kind', 'f-tools', 'f-urgent', 'f-effort'],
        }),
        C('f-site', 'TextField', {
          label: 'Site',
          value: { path: '/draft/site' },
          checks: [{ condition: { call: 'required', args: { value: { path: '/draft/site' } } }, message: 'Say where' }],
        }),
        C('f-hours', 'TextField', { label: 'Hours', variant: 'number', value: { path: '/draft/hours' } }),
        C('f-notes', 'TextField', { label: 'Notes', variant: 'longText', value: { path: '/draft/notes' } }),
        C('f-code', 'TextField', { label: 'Access code', variant: 'obscured', value: { path: '/draft/code' } }),
        C('f-dates', 'Row', { children: ['f-day', 'f-time'] }),
        C('f-day', 'DateTimeInput', { label: 'Day', enableDate: true, value: { path: '/draft/day' } }),
        C('f-time', 'DateTimeInput', { label: 'Start', enableTime: true, value: { path: '/draft/start' } }),
        C('f-kind', 'ChoicePicker', {
          label: 'Kind',
          variant: 'mutuallyExclusive',
          displayStyle: 'chips',
          options: [
            { label: 'Inspection', value: 'inspection' },
            { label: 'Repair', value: 'repair' },
            { label: 'Delivery', value: 'delivery' },
          ],
          value: { path: '/draft/kind' },
        }),
        C('f-tools', 'ChoicePicker', {
          label: 'Tools',
          variant: 'multipleSelection',
          displayStyle: 'checkbox',
          filterable: true,
          options: [
            { label: 'Ladder', value: 'ladder' },
            { label: 'Drill', value: 'drill' },
            { label: 'Meter', value: 'meter' },
          ],
          value: { path: '/draft/tools' },
        }),
        C('f-urgent', 'CheckBox', { label: 'Urgent', value: { path: '/draft/urgent' } }),
        C('f-effort', 'Slider', { label: 'Effort', min: 0, max: 10, value: { path: '/draft/effort' } }),
        C('tabs', 'Tabs', {
          tabs: [
            { title: 'Checklist', child: 'tab-checklist' },
            { title: 'History', child: 'tab-history' },
          ],
        }),
        C('tab-checklist', 'Text', { text: 'Check the meter, then the valves.' }),
        C('tab-history', 'Text', { text: 'No earlier visits.', variant: 'caption' }),
        C('divider', 'Divider'),
        C('list', 'List', { children: ['list-one', 'list-two'] }),
        C('list-one', 'Row', { align: 'center', children: ['list-icon', 'list-text'] }),
        C('list-icon', 'Icon', { name: { svgPath: 'M9 16.2 4.8 12l-1.4 1.4L9 19 21 7l-1.4-1.4z' } }),
        C('list-text', 'Text', { text: 'Meter read' }),
        C('list-two', 'Text', { text: 'Valves to check', variant: 'caption' }),
        C('more', 'Modal', { trigger: 'more-open', content: 'more-text' }),
        C('more-open', 'Button', { child: 'more-open-label', action: { event: { name: 'noop' } } }),
        label('more-open-label', 'Safety notes'),
        C('more-text', 'Text', { text: 'Wear gloves near the valves.' }),
        C('actions', 'Row', { children: ['a-help', 'a-clear', 'a-save'] }),
        C('a-help', 'Button', { variant: 'borderless', child: 'a-help-label', action: { event: { name: 'noop' } } }),
        label('a-help-label', 'Help'),
        C('a-clear', 'Button', { child: 'a-clear-label', action: { event: { name: 'noop' } } }),
        label('a-clear-label', 'Clear'),
        C('a-save', 'Button', {
          variant: 'primary',
          child: 'a-save-label',
          action: {
            event: { name: 'add', context: { site: { path: '/draft/site' }, hours: { path: '/draft/hours' } } },
          },
          checks: [{ condition: { call: 'required', args: { value: { path: '/draft/site' } } }, message: 'Say where' }],
        }),
        label('a-save-label', 'Submit'),
      ],
    },
  ],
  entry: 'main',
  collection: {
    fields: [
      { name: 'site', type: 'string', required: true },
      { name: 'hours', type: 'number' },
    ],
  },
  actions: {
    add: { kind: 'insert' },
    edit: { kind: 'update' },
    remove: { kind: 'delete' },
    noop: { kind: 'navigate', screen: 'main' },
  },
  summaries: { count: { kind: 'count' }, total_hours: { kind: 'sum', field: 'hours' } },
}

// An app whose one form holds every capture field taskto.ai's catalog has: photos, a location, a
// signature and a barcode, each with its own field in the collection.
const CAPTURE_APP_NAME = 'Every capture field'
const CAPTURE_APP = {
  screens: [
    {
      name: 'main',
      root: 'main-root',
      components: [
        C('main-root', 'Column', { children: ['title', 'photos', 'place', 'signed', 'code', 'save'] }),
        C('title', 'Text', { text: 'Site visit', variant: 'h2' }),
        C('photos', 'PhotoInput', { field: 'photos', label: 'Photos of the site', value: { path: '/draft/photos' } }),
        C('place', 'LocationInput', { field: 'place', label: 'Where you are', value: { path: '/draft/place' } }),
        C('signed', 'SignatureInput', { field: 'signed', label: 'Your signature', value: { path: '/draft/signed' } }),
        C('code', 'BarcodeInput', { field: 'code', label: 'Asset tag', value: { path: '/draft/code' } }),
        C('save', 'Button', {
          variant: 'primary',
          child: 'save-label',
          action: {
            event: {
              name: 'add',
              context: {
                photos: { path: '/draft/photos' },
                place: { path: '/draft/place' },
                signed: { path: '/draft/signed' },
                code: { path: '/draft/code' },
              },
            },
          },
        }),
        label('save-label', 'Save the visit'),
      ],
    },
  ],
  entry: 'main',
  collection: {
    fields: [
      { name: 'photos', type: 'attachment', max_files: 3 },
      { name: 'place', type: 'location' },
      { name: 'signed', type: 'signature' },
      { name: 'code', type: 'barcode' },
    ],
  },
  actions: { add: { kind: 'insert' } },
}

// What a screen needs, in the order the run goes: before any account exists, with an account and
// nothing in it, with the sample data, and signed out again.
export const PHASES = ['first-run', 'empty', 'seeded', 'signed-out']

// What the script looks for on a page, in English (it runs the app in English): a heading by its
// exact name, or a text that is on the page.
const heading = (s, name) => s.page.getByRole('heading', { name, exact: true })
// A board's column: its heading is the name and the number of cards.
const column = (s, name) => s.page.getByRole('heading', { name: new RegExp(`^${name} \\d+$`) })
const text = (s, value, exact = false) => s.page.getByText(value, { exact })

// The heading each Settings tab puts at the top of its page, in the order of the side list.
export const SETTINGS_HEADINGS = {
  general: 'General',
  members: 'Members',
  spaces: 'Shared spaces',
  audit: 'Audit log',
  providers: 'Model providers',
  integrations: 'Integrations',
  'integration-approvals': 'Integration approvals',
  'integration-events': 'Integration events',
  'cli-accounts': 'CLI accounts',
  search: 'Search',
  dictation: 'Dictation',
  storage: 'Storage',
  notifications: 'Notifications',
  password: 'Password',
  sessions: 'Active sessions',
  data: 'Your data',
  tokens: 'Access tokens',
  about: 'About taskto.ai',
}
export const SETTINGS_TABS = Object.keys(SETTINGS_HEADINGS)

// Each screen: a stable name (the file's), its phase, `show(s)`, which brings the screen up, and
// `ready(s)`, the locator (or a list of them) that only this screen, with its data, puts on the
// page: the capture waits for all of them and fails when one never shows, so a stand-in page or a
// screen that didn't load is never saved as a picture. `s` has `page`, `go(path)`, `ids` (the
// seeded data), `agent` (whether the agent runtime runs) and `path(section, item)`. A screen that
// can't find what it clicks throws, so a renamed control fails the run too. `only: 'phone'` limits
// a screen to one size; `needsAgent` marks a screen that exists only where the agent runtime runs
// (a conversation can't be made without it).
export const SCREENS = [
  {
    name: 'setup',
    phase: 'first-run',
    show: (s) => s.go('/'),
    ready: (s) => [heading(s, 'Set up this server'), s.page.getByRole('button', { name: 'Create the owner' })],
  },
  {
    name: 'login',
    phase: 'signed-out',
    show: (s) => s.go('/'),
    ready: (s) => [heading(s, 'Log in'), s.page.getByRole('button', { name: 'Log in', exact: true })],
  },
  {
    name: 'empty-home',
    phase: 'empty',
    show: (s) => s.go(s.path('home')),
    ready: (s) => [heading(s, 'Home'), text(s, 'Nothing on your Home yet')],
  },
  {
    name: 'empty-conversations',
    phase: 'empty',
    show: (s) => s.go(s.path('conversations')),
    ready: (s) => [heading(s, 'Conversations'), text(s, 'No conversations yet')],
  },
  {
    name: 'empty-knowledge',
    phase: 'empty',
    show: (s) => s.go(s.path('knowledge')),
    ready: (s) => [heading(s, 'Knowledge base'), text(s, 'No documents in this space yet.')],
  },
  {
    name: 'empty-boards',
    phase: 'empty',
    show: (s) => s.go(s.path('boards')),
    ready: (s) => [heading(s, 'Personal'), column(s, 'Backlog')],
  },
  {
    name: 'empty-apps',
    phase: 'empty',
    show: (s) => s.go(s.path('apps')),
    // Every space is empty and says so: the first one's will do.
    ready: (s) => [heading(s, 'Apps'), text(s, /^No apps in .+ yet$/).first()],
  },
  {
    name: 'empty-inbox',
    phase: 'empty',
    show: (s) => s.go(s.path('inbox')),
    ready: (s) => [heading(s, 'Notifications'), text(s, 'No notifications yet.')],
  },
  {
    name: 'home',
    phase: 'seeded',
    show: (s) => s.go(s.path('home')),
    ready: (s) => [heading(s, 'Home'), text(s, `From ${s.ids.homeApp}`)],
  },
  {
    name: 'home-customise',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('home'))
      await s.page.getByRole('button', { name: 'Customize' }).first().click()
    },
    ready: (s) => [s.page.getByRole('button', { name: 'Done', exact: true }), heading(s, 'Add from your apps')],
  },
  {
    name: 'conversations',
    phase: 'seeded',
    show: (s) => s.go(s.path('conversations')),
    ready: (s) => [
      heading(s, 'Conversations'),
      s.agent ? text(s, 'Plan the launch week') : text(s, 'No conversations yet'),
    ],
  },
  {
    name: 'conversation',
    phase: 'seeded',
    needsAgent: true,
    show: (s) => s.go(s.path('conversations', s.ids.conversation)),
    ready: (s) => [heading(s, 'Plan the launch week'), text(s, 'Add a card to my board')],
  },
  {
    name: 'knowledge',
    phase: 'seeded',
    show: (s) => s.go(s.path('knowledge')),
    ready: (s) => [heading(s, 'Knowledge base'), text(s, 'welcome.md', true)],
  },
  {
    name: 'knowledge-document',
    phase: 'seeded',
    show: (s) => s.go(s.path('knowledge', `${s.ids.space}:welcome.md`)),
    ready: (s) => [heading(s, 'Welcome'), heading(s, 'Getting started')],
  },
  {
    name: 'knowledge-editor',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('knowledge', `${s.ids.space}:welcome.md`))
      await s.page.getByRole('button', { name: 'Edit', exact: true }).first().click()
    },
    ready: (s) => [
      s.page.getByRole('textbox', { name: 'Text' }),
      s.page.getByRole('button', { name: 'Save', exact: true }),
    ],
  },
  {
    name: 'knowledge-search',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('knowledge'))
      const box = s.page.getByRole('searchbox').first()
      await box.fill('launch')
      await box.press('Enter')
    },
    ready: (s) => [heading(s, 'Results for “launch”'), text(s, 'Launch plan')],
  },
  {
    name: 'boards',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards')),
    ready: (s) => [heading(s, 'Personal'), text(s, 'Write the launch post')],
  },
  {
    name: 'board',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`)),
    ready: (s) => [column(s, 'Backlog'), text(s, 'Dark mode contrast pass'), text(s, 'Audit log filters')],
  },
  {
    name: 'board-shared',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards', `${s.ids.team}:${s.ids.teamBoard}`)),
    ready: (s) => [column(s, 'Backlog'), text(s, 'Quotas: design the usage view'), heading(s, 'Launch')],
  },
  {
    name: 'card-open',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards', `${s.ids.space}:${s.ids.board}:${s.ids.card}`)),
    ready: (s) => [heading(s, 'Write the launch post'), text(s, 'Looks good. Can we add the pricing table?')],
  },
  {
    name: 'card-blocked',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`))
      await s.page.getByRole('button', { name: /^Audit log filters/ }).click()
    },
    ready: (s) => [heading(s, 'Audit log filters'), text(s, 'Blocked: waiting for the audit log API')],
  },
  {
    name: 'card-move',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`))
      await s.page.getByRole('button', { name: 'Move “Write the launch post”' }).click()
    },
    ready: (s) => [s.page.getByRole('menuitem', { name: 'To Doing' })],
  },
  {
    name: 'card-add',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`))
      await s.page.getByRole('button', { name: 'Add a card' }).first().click()
    },
    ready: (s) => [s.page.getByRole('form', { name: /^New card in/ }), text(s, 'Write the launch post')],
  },
  {
    name: 'apps',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps')),
    ready: (s) => [heading(s, 'Apps'), text(s, s.ids.appName)],
  },
  {
    name: 'apps-templates',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps', 'templates')),
    ready: (s) => [
      heading(s, 'Start from a template'),
      s.page.getByRole('button', { name: 'Use the template Expense tracker', exact: true }),
    ],
  },
  {
    name: 'app-open',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps', `${s.ids.space}-${s.ids.app}`)),
    ready: (s) => heading(s, s.ids.appName),
  },
  {
    name: 'app-components',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps', `${s.ids.space}-${s.ids.componentsApp}`)),
    ready: (s) => [heading(s, COMPONENTS_APP_NAME), text(s, 'Submit')],
  },
  {
    // The same app scrolled to its end: the choices, the check box, the slider, the tabs, the list,
    // the modal's button and the row of buttons.
    name: 'app-components-end',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('apps', `${s.ids.space}-${s.ids.componentsApp}`))
      await text(s, 'Submit', true).scrollIntoViewIfNeeded()
    },
    ready: (s) => [text(s, 'Submit', true), text(s, 'Safety notes', true)],
  },
  {
    // Its records, opened from the app's header: the explorer (a table, or rows on a phone).
    name: 'app-records',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('apps', `${s.ids.space}-${s.ids.componentsApp}`))
      await s.page.getByRole('button', { name: 'Records and history' }).click()
      await text(s, 'Riverside depot', true).first().waitFor()
    },
    ready: (s) => [text(s, 'Riverside depot', true).first(), text(s, /matching records/)],
  },
  {
    // What its button set going: documents made, and a log the later visits couldn't make.
    name: 'app-effects',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('apps', `${s.ids.space}-${s.ids.componentsApp}`))
      await s.page.getByRole('button', { name: /^Effects/ }).click()
      await s.page.getByRole('list', { name: 'The app’s effects' }).scrollIntoViewIfNeeded()
    },
    ready: (s) => [s.page.getByRole('list', { name: 'The app’s effects' })],
  },
  {
    // One record open, with its fields and its history.
    name: 'app-record',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('apps', `${s.ids.space}-${s.ids.componentsApp}`))
      await s.page.getByRole('button', { name: 'Records and history' }).click()
      await s.page
        .getByRole('button', { name: /^Open record \d+$/ })
        .first()
        .click()
    },
    ready: (s) => [heading(s, /^Record \d+$/), text(s, /^History$/)],
  },
  // The capture fields: a photo, a location, a signature and a barcode, at the form's top and end.
  {
    name: 'app-capture',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps', `${s.ids.space}-${s.ids.captureApp}`)),
    ready: (s) => [heading(s, CAPTURE_APP_NAME), text(s, 'Take a photo', true)],
  },
  {
    name: 'app-capture-end',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('apps', `${s.ids.space}-${s.ids.captureApp}`))
      await text(s, 'Save the visit', true).scrollIntoViewIfNeeded()
    },
    ready: (s) => [text(s, 'Save the visit', true), s.page.getByLabel(/Code \(type it/)],
  },
  {
    name: 'app-builder',
    phase: 'seeded',
    show: (s) => s.go(s.path('apps', `edit-${s.ids.space}-${s.ids.app}`)),
    ready: (s) => [heading(s, 'Edit with AI'), text(s, s.ids.appName)],
  },
  {
    name: 'inbox',
    phase: 'seeded',
    show: (s) => s.go(s.path('inbox')),
    ready: (s) => [heading(s, 'Notifications'), text(s, INBOX_CARD)],
  },
  ...SETTINGS_TABS.map((tab) => ({
    name: `settings-${tab}`,
    phase: 'seeded',
    show: (s) => s.go(s.path('settings', tab)),
    ready: (s) => [heading(s, SETTINGS_HEADINGS[tab])],
  })),
  {
    name: 'account-menu',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('home'))
      // On a phone the account is in the sidebar's drawer.
      const drawer = s.page.getByRole('button', { name: 'Open the menu' })
      if (await drawer.isVisible()) await drawer.click()
      await s.page.getByRole('button', { name: `Account: ${owner.name}` }).click()
    },
    ready: (s) => [heading(s, 'Home'), s.page.getByRole('menuitem', { name: 'Log out' })],
  },
  {
    name: 'drawer',
    phase: 'seeded',
    only: 'phone',
    show: async (s) => {
      await s.go(s.path('home'))
      await s.page.getByRole('button', { name: 'Open the menu' }).click()
    },
    ready: (s) => [
      heading(s, 'Home'),
      s.page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name: 'Boards' }),
    ],
  },
]

// Every capture, in the order the run takes them, as `{ id: 'desktop-light/boards', ... }`.
export function captures() {
  const combos = SIZES.flatMap((size) => THEMES.map((theme) => ({ size, theme, dir: `${size.name}-${theme}` })))
  return PHASES.flatMap((phase) =>
    SCREENS.filter((screen) => screen.phase === phase).flatMap((screen) =>
      combos
        .filter((c) => !screen.only || screen.only === c.size.name)
        .map((c) => ({ id: `${c.dir}/${screen.name}`, screen, ...c })),
    ),
  )
}

// ---------------------------------------------------------------------------------------------
// Secrets never reach the output. The setup token is in the server's log while no account exists;
// the session cookie and the sample password are known once they exist. Every text the script
// prints goes through `redact`, which also knows the values added with `hide`.

const hidden = new Set()
export const hide = (value) => {
  if (typeof value === 'string' && value.length >= 6) hidden.add(value)
}
export const redact = (value) => {
  let out = String(value).replace(/setup_token=\S+/g, 'setup_token=[hidden]')
  for (const secret of hidden) out = out.split(secret).join('[hidden]')
  return out
}

// Playwright's own debug traces (`DEBUG=pw:channel`) print every call's arguments, the session
// cookie set on a browser context among them: the script keeps the other debug settings and drops
// those, and what it keeps (a `*`, say) excludes Playwright's namespaces explicitly.
export function withoutPlaywrightDebug(debug) {
  const kept = String(debug ?? '')
    .split(/[\s,]+/)
    .filter((entry) => entry && !/^-?pw:/.test(entry))
  return kept.length > 0 ? [...kept, '-pw:*'].join(',') : undefined
}

// ---------------------------------------------------------------------------------------------
// The cleanup: what the run has to undo (the server, the browser, the data directory), run once,
// in the reverse order of what was added, by an ordinary end, a failure and a signal alike.

export function createCleanup() {
  const tasks = []
  const exits = []
  let running
  const cleanup = {
    closing: false,
    // `undo` is awaited by `run`; `now`, optional, is a synchronous last resort for when the
    // process is about to exit with the work undone.
    add(undo, now) {
      if (now) exits.push(now)
      if (cleanup.closing) {
        // Added after the cleanup began (a server spawned as the interruption arrived): undo it at once.
        Promise.resolve()
          .then(undo)
          .catch(() => {})
        return
      }
      tasks.push(undo)
    },
    run() {
      cleanup.closing = true
      running ??= (async () => {
        for (const undo of tasks.reverse())
          await Promise.resolve()
            .then(undo)
            .catch(() => {})
      })()
      return running
    },
    // Runs when the process exits whatever else happened: only what is synchronous can.
    last() {
      for (const now of exits.reverse()) {
        try {
          now()
        } catch {}
      }
    },
  }
  return cleanup
}

// An interruption goes through the same cleanup as a failure, then the process exits with the
// conventional code (128 plus the signal's number). Installed before anything is started.
export function trapSignals(cleanup, { signals = ['SIGINT', 'SIGTERM', 'SIGHUP'], target = process, exit, say } = {}) {
  const leave = exit ?? ((code) => process.exit(code))
  const log = say ?? ((msg) => console.error(msg))
  const handlers = signals.map((signal) => {
    const handler = () => {
      log(`${signal}: stopping the server and removing the temporary data`)
      cleanup.run().finally(() => leave(128 + osConstants.signals[signal]))
    }
    target.on(signal, handler)
    return [signal, handler]
  })
  return () => {
    for (const [signal, handler] of handlers) target.off(signal, handler)
  }
}

// ---------------------------------------------------------------------------------------------
// The server.

const READY_LINE = /taskto listening addr=(\S+).*web_app_built=(\w+)/
const SETUP_TOKEN = /setup_token=([A-Za-z0-9_-]{43})/

// Starts `bin args` and waits until it listens. Resolves to the server, or rejects with an error
// that carries it (`err.server.logs()`), after the child has been stopped and reaped: whatever
// the reason (it exited, it never said it listens, the web app isn't built, a timeout), nothing
// is left running. `track(server)` is called as soon as the child exists, so a cleanup can reach
// it while this is still waiting.
export function launch({ bin, args, env, timeoutMs = 60_000, killAfterMs = 10_000, track = () => {} }) {
  // Its own process group, so that a stubborn server and what it started can be killed together.
  const proc = spawn(bin, args, { env, stdio: ['ignore', 'ignore', 'pipe'], detached: true })
  let logs = ''
  const running = () => proc.pid !== undefined && proc.exitCode === null && proc.signalCode === null
  const signalGroup = (signal) => {
    try {
      process.kill(-proc.pid, signal)
    } catch {
      try {
        proc.kill(signal)
      } catch {}
    }
  }
  // Whether anything of the child's process group is still there: a wrapper script that started
  // the server without `exec` can exit before the server does.
  const groupAlive = () => {
    try {
      process.kill(-proc.pid, 0)
      return true
    } catch {
      return false
    }
  }
  let stopping
  const server = {
    proc,
    logs: () => redact(logs),
    // Stops it and what it started (SIGTERM to the group, then SIGKILL after `killAfterMs`) and
    // resolves once the child has exited and none of its group is left.
    stop() {
      stopping ??= new Promise((done) => {
        if (proc.pid === undefined || (!running() && !groupAlive())) return done()
        const hard = setTimeout(() => signalGroup('SIGKILL'), killAfterMs)
        const settle = () => {
          if (running()) return
          if (!groupAlive()) {
            clearTimeout(hard)
            return done()
          }
          setTimeout(settle, 20)
        }
        proc.once('exit', settle)
        signalGroup('SIGTERM')
        settle()
      })
      return stopping
    },
    // Synchronous and not graceful: the last resort when the process is leaving.
    kill() {
      if (running()) signalGroup('SIGKILL')
    },
  }
  proc.stderr.on('error', () => {})
  track(server)
  return new Promise((ok, fail) => {
    let settled = false
    const timer = setTimeout(() => reject(new Error('the server did not start in time')), timeoutMs)
    const reject = (err) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      server.stop().then(() => fail(Object.assign(err, { server })))
    }
    proc.stderr.on('data', (chunk) => {
      logs = (logs + chunk).slice(-100_000)
      if (settled) return
      const addr = logs.match(READY_LINE)
      const token = logs.match(SETUP_TOKEN)
      if (!addr || !token) return
      hide(token[1])
      if (addr[2] !== 'true') return reject(new Error('the web app is not built into the binary: run make build'))
      settled = true
      clearTimeout(timer)
      server.base = `http://${addr[1]}`
      server.token = token[1]
      ok(server)
    })
    proc.on('error', (e) => reject(new Error(`the server did not start: ${e.code ?? 'error'}`)))
    proc.on('exit', (code, signal) => {
      // Its last words may still be on the way: they say why (the agent runtime refused, say).
      const exited = () => reject(new Error(`the server exited (${code ?? signal})`))
      if (proc.stderr.readableEnded || proc.stderr.destroyed) return exited()
      proc.stderr.once('end', exited)
      proc.stderr.once('close', exited)
      setTimeout(exited, 2_000).unref()
    })
  })
}

// The server with the agent runtime when it accepts it. `agent` says what came of that: 'on', or
// why not.
async function startServer(dir, { unsafeAgent, cleanup }) {
  const bin = process.env.TASKTO_SCREENS_BIN || join(repo, 'bin/taskto')
  const entry = join(repo, 'agent/dist/index.js')
  // The server gets its own flags only: a TASKTO_* variable of this machine (a config file, a
  // data directory, a provider) must not change what is captured.
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('TASKTO_')))
  const start = (flags) => {
    if (cleanup.closing) throw new Error('interrupted')
    return launch({
      bin,
      env,
      args: ['-addr', '127.0.0.1:0', '-data-dir', join(dir, 'data'), '-insecure-cookies', ...flags],
      track: (server) =>
        cleanup.add(
          () => server.stop(),
          () => server.kill(),
        ),
    })
  }
  if (!existsSync(entry)) {
    const server = await start([])
    return Object.assign(server, { agent: 'off: the agent runtime is not built (make build builds it)' })
  }
  const agentFlags = ['-agent', '-agent-entry', entry, ...(unsafeAgent ? ['-unsafe-privileged-agents'] : [])]
  try {
    const server = await start(agentFlags)
    return Object.assign(server, { agent: unsafeAgent ? 'on (-unsafe-privileged-agents)' : 'on' })
  } catch (err) {
    // launch has stopped the refused server already.
    const refused = /must not be able to act as root|act as root/.test(err.server?.logs() ?? '')
    if (!refused) throw err
    rmSync(join(dir, 'data'), { recursive: true, force: true })
    const server = await start([])
    return Object.assign(server, {
      agent:
        'off: the server refuses the agent runtime for a user who can run sudo without a password (docs/agent-safety.md)',
    })
  }
}

// ---------------------------------------------------------------------------------------------
// The script's own HTTP client, for the setup and the sample data. The session cookie is kept in
// memory and sent back; a failed call is reported as its method, path and the status (or the
// error's code) and never carries a header, a body or the raw error.

export class ApiFailure extends Error {
  constructor(method, path, status, code) {
    super(`${method} ${path}: ${status ?? 'no answer'}${code ? ` (${code})` : ''}`)
    this.name = 'ApiFailure'
  }
}

const safeCode = (value) => (typeof value === 'string' && /^[A-Za-z0-9_.-]{1,60}$/.test(value) ? value : undefined)

export function createClient(base, { fetchImpl = fetch, timeoutMs = 30_000 } = {}) {
  const jar = new Map()
  const remember = (res) => {
    for (const line of res.headers.getSetCookie?.() ?? []) {
      const [pair, ...attrs] = line.split(';')
      const at = pair.indexOf('=')
      if (at < 1) continue
      const name = pair.slice(0, at).trim()
      const value = pair.slice(at + 1).trim()
      const gone = value === '' || attrs.some((a) => /^\s*max-age=0*\s*$/i.test(a))
      if (gone) jar.delete(name)
      else {
        jar.set(name, value)
        hide(value)
      }
    }
  }
  return {
    // The cookies as a browser context takes them.
    cookies: () => [...jar].map(([name, value]) => ({ name, value, url: base })),
    async call(method, path, body) {
      let res
      try {
        res = await fetchImpl(`${base}${path}`, {
          method,
          headers: {
            ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
            ...(jar.size > 0 ? { Cookie: [...jar].map(([name, value]) => `${name}=${value}`).join('; ') } : {}),
          },
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: AbortSignal.timeout(timeoutMs),
        })
      } catch (err) {
        throw new ApiFailure(method, path, undefined, safeCode(err?.cause?.code) ?? safeCode(err?.name))
      }
      remember(res)
      if (!res.ok) {
        const code = await res
          .json()
          .then((b) => safeCode(b?.code))
          .catch(() => undefined)
        throw new ApiFailure(method, path, res.status, code)
      }
      if (res.status === 204) return null
      return res.json().catch(() => {
        throw new ApiFailure(method, path, res.status, 'not_json')
      })
    },
  }
}

// ---------------------------------------------------------------------------------------------
// The output folder. The script writes `<size>-<theme>/<screen>.png` and lists in a manifest what
// it wrote, so that it knows what is its own: stale captures of an earlier run are pruned from
// that list, after a run that failed nowhere, and any other file in the folder is left alone.

export const MANIFEST = '.screens-manifest.json'
const OWN = /^[a-z]+-(?:light|dark)\/[a-z0-9-]+$/

// A symbolic link at the manifest's path is never followed, to read or to write: it is not the
// list this script wrote.
const manifestIsLink = (outDir) => lstatSync(join(outDir, MANIFEST), { throwIfNoEntry: false })?.isSymbolicLink()

export function readManifest(outDir) {
  try {
    if (manifestIsLink(outDir)) return new Set()
    const list = JSON.parse(readFileSync(join(outDir, MANIFEST), 'utf8')).captures
    return new Set(list.filter((id) => typeof id === 'string' && OWN.test(id)))
  } catch {
    return new Set()
  }
}

// `written`: the ids this run saved. `failed`: the selected ones it did not save, failed or
// skipped (an old picture of one would pass for the current screen, so it goes, in a filtered run
// too). `prune`: a full run that failed nowhere also removes the earlier run's pictures it didn't
// write.
export function recordCaptures({ outDir, written, failed = [], prune = false }) {
  if (manifestIsLink(outDir)) throw new Error(`${MANIFEST} is a symbolic link: not writing through it`)
  const before = readManifest(outDir)
  const remove = new Set(failed.filter((id) => before.has(id) && !written.has(id)))
  if (prune) for (const id of before) if (!written.has(id)) remove.add(id)
  for (const id of remove) rmSync(join(outDir, `${id}.png`), { force: true })
  const next = new Set([...before].filter((id) => !remove.has(id)))
  for (const id of written) next.add(id)
  mkdirSync(outDir, { recursive: true })
  // A temporary file beside it, renamed over it: the manifest is whole or untouched.
  const temporary = join(outDir, `${MANIFEST}.${process.pid}.tmp`)
  try {
    writeFileSync(temporary, `${JSON.stringify({ captures: [...next].sort() }, null, 2)}\n`, { flag: 'wx' })
    renameSync(temporary, join(outDir, MANIFEST))
  } catch (err) {
    rmSync(temporary, { force: true })
    throw err
  }
  return { removed: [...remove].sort() }
}

// Whether the run succeeded: nothing failed, and in `--strict` the browser reported nothing. It
// decides the exit status and, for a full run, whether the earlier run's other pictures are pruned.
export const runSucceeded = ({ failures, issues, strict }) => failures.length === 0 && !(strict && issues.size > 0)

// The run's one update of the output folder: `record(prune)` writes it the first time it is called
// and does nothing after. `failedIds` and `skipped` are read when it runs. When the folder can't be
// updated (the manifest's path is a directory, a read-only disk) the reason goes into `failures`, so
// the run exits non-zero, and nothing is thrown: the cleanup that follows must still run. The
// pictures this run made that the manifest couldn't take on (not in `owned`, the earlier manifest)
// are removed, so that the next run, once the folder is right, doesn't refuse them as foreign.
export function createRecorder({ outDir, written, owned = new Set(), failedIds, skipped, failures }) {
  let recorded = false
  return (prune) => {
    if (recorded) return
    recorded = true
    try {
      recordCaptures({ outDir, written, failed: [...failedIds, ...skipped], prune })
    } catch (err) {
      const reason = `could not update ${MANIFEST}: ${redact(String(err.message).split('\n')[0])}`
      console.error(reason)
      failures.push(reason)
      for (const id of written) if (!owned.has(id)) rmSync(join(outDir, `${id}.png`), { force: true })
    }
  }
}

// ---------------------------------------------------------------------------------------------
// One capture.

export const READY_TIMEOUT = 15_000

// Waits for every locator of the screen's ready check. A screen with no check, or one whose check
// never shows, throws.
export async function awaitReady(screen, s, timeout = READY_TIMEOUT) {
  if (typeof screen.ready !== 'function') throw new Error(`${screen.name} has no ready check`)
  const checks = [screen.ready(s)].flat().filter(Boolean)
  if (checks.length === 0) throw new Error(`${screen.name} has an empty ready check`)
  try {
    await Promise.all(checks.map((check) => check.first().waitFor({ state: 'visible', timeout })))
  } catch (err) {
    throw new Error(`${screen.name} is not what the screen shows (ready check): ${String(err.message).split('\n')[0]}`)
  }
}

// Brings the screen up, waits for its ready check and the page to settle, and saves the picture.
// A file already at the picture's path is replaced only when an earlier run wrote it (`owned`, the
// manifest's captures read before this run); another file there fails the capture and stays.
// Returns `{ ok: true }` or `{ ok: false, error }`; never throws.
export async function captureOne(c, s, { outDir, written, owned = new Set(), readyTimeout = READY_TIMEOUT }) {
  try {
    await c.screen.show(s)
    await awaitReady(c.screen, s, readyTimeout)
    // Fonts, the requests that follow the screen's first render, and two frames for a settled
    // layout, so two runs match. The requests are best effort: a stream that stays open is no fault.
    await s.page.waitForLoadState('networkidle', { timeout: 5_000 }).catch(() => {})
    await s.page.evaluate(() =>
      document.fonts.ready.then(
        () => new Promise((frame) => requestAnimationFrame(() => requestAnimationFrame(frame))),
      ),
    )
    // The screen is still the screen after settling.
    await awaitReady(c.screen, s, readyTimeout)
    const file = join(outDir, `${c.id}.png`)
    if (!owned.has(c.id) && lstatSync(file, { throwIfNoEntry: false })) {
      throw new Error(`${c.id}.png is there and this script did not write it: not replacing it`)
    }
    mkdirSync(join(outDir, c.dir), { recursive: true })
    await s.page.screenshot({ path: file, animations: 'disabled', caret: 'hide' })
    written.add(c.id)
    return { ok: true }
  } catch (err) {
    return { ok: false, error: redact(String(err.message).split('\n')[0]) }
  }
}

// ---------------------------------------------------------------------------------------------
// The sample data.

const isoDay = (offset) => {
  const d = new Date()
  d.setDate(d.getDate() + offset)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

// The sample data, through the API as the owner. Dates are relative to today, so the labels that
// depend on them (overdue, soon) read the same on every day. Returns the ids the screens use.
async function seed({ api, base, workspace, agent, say }) {
  const W = `/api/workspaces/${workspace}`
  const { spaces } = await api('GET', `${W}/spaces`)
  const mine = spaces.find((s) => s.kind === 'private') ?? spaces[0]
  const team = await api('POST', `${W}/spaces`, { name: 'Product team' })
  const P = `${W}/spaces/${mine.id}`
  const T = `${W}/spaces/${team.id}`

  const docs = {
    'welcome.md':
      '# Welcome\n\nThis is the **knowledge base**. Notes live here as Markdown.\n\n## Getting started\n\n- Write a note\n- Ask the agent to summarise it\n- Link it to a card\n\n> Everything is versioned in git.\n\n```sh\ntaskto backup --out backup.tar.gz\n```\n\n| Plan | Seats | Price |\n|---|---:|---:|\n| Solo | 1 | 0 |\n| Team | 10 | 49 |\n',
    'launch/plan.md':
      '# Launch plan\n\nShip the beta in two weeks.\n\n1. Finish onboarding\n2. Write the launch post\n3. Record the demo\n',
    'launch/post.md': '# Launch post\n\nDraft of the announcement.\n',
    'meetings/weekly-planning.md':
      '# Weekly planning\n\nAttendees: Ana, Bruno, Carla.\n\n## Decisions\n\n- Keep the launch date.\n',
    'meetings/weekly-planning-with-a-very-long-title-that-should-wrap-or-truncate-gracefully.md':
      '# Weekly\n\nNotes.\n',
    'ideas.md': '# Ideas\n\n- A quotas view\n- Email digests\n',
  }
  for (const [path, content] of Object.entries(docs))
    await api('PUT', `${P}/docs/${path}`, { content, message: `Add ${path}` })
  await api('PUT', `${T}/docs/handbook.md`, { content: '# Handbook\n\nHow the team works.\n', message: 'Add handbook' })

  const cards = [
    [
      'Write the launch post',
      'todo',
      {
        due: isoDay(-3),
        labels: ['launch', 'writing'],
        body: 'Draft, review, publish.\n\n- [ ] Outline\n- [ ] Draft\n',
      },
    ],
    ['Record the demo video', 'todo', { due: isoDay(9), labels: ['launch'] }],
    [
      'Fix the onboarding checklist copy so that it reads well on small phones and long languages',
      'doing',
      { labels: ['onboarding', 'copy', 'i18n'], due: isoDay(1) },
    ],
    ['Review the pricing page', 'doing', {}],
    ['Quotas: design the usage view', 'backlog', { labels: ['design'] }],
    ['Email digests', 'backlog', {}],
    ['Audit log filters', 'backlog', { blocked_reason: 'waiting for the audit log API' }],
    ['Dark mode contrast pass', 'review', { labels: ['design', 'a11y'] }],
    ['Set up backups', 'done', {}],
    ['Invite the team', 'done', {}],
  ]
  const board = (await api('GET', `${P}/boards`)).boards[0]
  for (const [title, column, extra] of cards)
    await api('POST', `${P}/boards/${board.id}/cards`, { title, column, ...extra })
  const card = (await api('GET', `${P}/boards/${board.id}/cards`)).cards.find((c) => c.title === cards[0][0])
  await api('POST', `${P}/boards/${board.id}/cards/${card.id}/comments`, {
    body: 'First outline is in `launch/post.md`.',
  })
  await api('POST', `${P}/boards/${board.id}/cards/${card.id}/comments`, {
    body: 'Looks good. Can we add the pricing table?',
  })

  const teamBoard = await api('POST', `${T}/boards`, { name: 'Launch' })
  await api('POST', `${T}/boards`, { name: 'Support' })
  for (const [title, column, extra] of cards.slice(0, 5))
    await api('POST', `${T}/boards/${teamBoard.id}/cards`, { title, column, ...extra })

  // A second person, so that the owner's inbox has something in it (nothing the owner does tells
  // the owner): Bruno takes up an invite, joins the team space, assigns the owner a card there and
  // mentions them in a comment on it. A pending invite stays for the Members screen.
  const invite = await api('POST', `${W}/invites`, { email: 'bruno@example.com', role: 'member' })
  hide(invite.token)
  hide(colleague.password)
  const other = createClient(base)
  const bruno = await other.call('POST', '/api/invites/accept', {
    token: invite.token,
    name: colleague.name,
    password: colleague.password,
  })
  await api('POST', `${T}/members`, { user_id: bruno.user.id, role: 'editor' })
  const ownerId = (await api('GET', '/api/me')).user.id
  const assigned = await other.call('POST', `${T}/boards/${teamBoard.id}/cards`, {
    title: INBOX_CARD,
    column: 'todo',
    assignee: { kind: 'user', id: ownerId },
  })
  await other.call('POST', `${T}/boards/${teamBoard.id}/cards/${assigned.id}/comments`, {
    body: `@[${owner.name}](user:${ownerId}) can you take a look before Friday?`,
  })
  await api('POST', `${W}/invites`, { email: 'carla@example.com', role: 'member' })
  await api('POST', `${W}/tokens`, { name: 'laptop' })

  // Apps from the templates that need no board, installed and published; their widgets and pins
  // on Home.
  const { templates } = await api('GET', `${W}/app-templates`)
  const apps = []
  for (const template of templates.filter((t) => !t.needs_board).slice(0, 3)) {
    const app = await api('POST', `${P}/app-templates/${template.id}/install`, {})
    await api('POST', `${P}/apps/${app.id}/publish`, { version: app.version, acknowledge: [] })
    apps.push(app)
  }
  // An app that uses every component of the renderer an app may (docs/design.md, "The apps
  // renderer"), with a few records through its own action, for its records and their history.
  {
    // What its button sets going: a document for each visit, and one log that only the first visit
    // can make (a document is never written over), so the others' fail, with their retry.
    const effects = [
      { kind: 'create_document', space_id: mine.id, path: 'visits/{{id}}.md', content: 'Site: {{site}}\n' },
      { kind: 'create_document', space_id: mine.id, path: 'visits/log.md', content: 'First visit: {{site}}\n' },
    ]
    const definition = {
      ...COMPONENTS_APP,
      actions: { ...COMPONENTS_APP.actions, add: { kind: 'insert', effects } },
    }
    const app = await api('POST', `${P}/apps`, { name: COMPONENTS_APP_NAME, definition })
    await api('POST', `${P}/apps/${app.id}/publish`, { version: app.version, acknowledge: [] })
    apps.componentsApp = app.id
    const visits = [
      ['North pumping station', 3.5],
      ['Riverside depot', 1.25],
      ['Old mill substation', 6],
    ]
    for (const [i, [site, hours]] of visits.entries()) {
      await api('POST', `${P}/apps/${app.id}/actions`, {
        name: 'add',
        surfaceId: `app-${app.id}`,
        sourceComponentId: 'a-save',
        timestamp: new Date().toISOString(),
        context: { site, hours },
        operation: `screens-visit-${i}`,
      })
    }
  }
  // An app whose form holds every capture field.
  {
    const app = await api('POST', `${P}/apps`, { name: CAPTURE_APP_NAME, definition: CAPTURE_APP })
    await api('POST', `${P}/apps/${app.id}/publish`, { version: app.version, acknowledge: [] })
    apps.captureApp = app.id
  }
  const available = (await api('GET', `${W}/home/available`)).apps ?? []
  let homeApp
  for (const app of available.slice(0, 3)) {
    for (const widget of (app.widgets ?? []).slice(0, 2)) {
      await api('POST', `${W}/home/items`, {
        kind: 'widget',
        space_id: app.space_id,
        app_id: app.app_id,
        widget: widget.id ?? widget,
      })
      homeApp ??= app.name
    }
    if (app.pin) await api('POST', `${W}/home/items`, { kind: 'pin', space_id: app.space_id, app_id: app.app_id })
  }

  // Conversations: one with an exchange when the scripted model is there to answer. Without the
  // agent runtime the server can't make one.
  let talk
  if (agent.startsWith('on')) {
    talk = await api('POST', `${W}/conversations`, { title: 'Plan the launch week' })
    await api('POST', `${W}/conversations`, { title: 'Summarise the weekly notes' })
    await api('POST', `${W}/conversations`, {})
    let replies = 0
    for (const content of [
      'List the documents in my knowledge base and summarise the launch plan.',
      'Add a card to my board: "Book the launch call".',
    ]) {
      await api('POST', `${W}/conversations/${talk.id}/messages`, { content })
      const deadline = Date.now() + 60_000
      for (;;) {
        const { messages } = await api('GET', `${W}/conversations/${talk.id}/messages`)
        if (messages.filter((m) => m.role === 'assistant').length > replies) break
        if (Date.now() > deadline) throw new Error('the scripted model did not answer in a minute')
        await new Promise((r) => setTimeout(r, 500))
      }
      replies++
    }
  } else {
    say('the agent runtime is off: there are no conversations to capture')
  }

  return {
    space: mine.id,
    team: team.id,
    board: board.id,
    teamBoard: teamBoard.id,
    card: card.id,
    app: apps[0].id,
    appName: apps[0].name,
    homeApp: homeApp ?? apps[0].name,
    componentsApp: apps.componentsApp,
    captureApp: apps.captureApp,
    conversation: talk?.id,
  }
}

// ---------------------------------------------------------------------------------------------
// The run.

async function main() {
  const { values } = parseArgs({
    options: {
      out: { type: 'string' },
      only: { type: 'string' },
      list: { type: 'boolean', default: false },
      strict: { type: 'boolean', default: false },
      'unsafe-agent': { type: 'boolean', default: false },
    },
  })
  const only = (values.only ?? process.env.TASKTO_SCREENS_ONLY ?? '')
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean)
  const plan = captures().filter((c) => only.length === 0 || only.some((t) => c.id.includes(t)))
  if (values.list) {
    for (const c of plan) console.log(c.id)
    return 0
  }
  if (plan.length === 0) {
    console.error(`no capture's name contains ${only.map((t) => `"${t}"`).join(' or ')}: see --list`)
    return 1
  }
  const outDir = resolve(values.out || process.env.TASKTO_SCREENS_OUT || join(repo, 'web/screens'))
  const unsafeAgent = values['unsafe-agent'] || process.env.TASKTO_SCREENS_UNSAFE_AGENT === '1'
  const started = Date.now()
  const say = (msg) => console.log(msg)

  // Before anything is started: an interruption from here on stops what exists and removes the
  // data directory, as a failure does.
  const cleanup = createCleanup()
  const untrap = trapSignals(cleanup)
  process.once('exit', () => cleanup.last())

  const debug = withoutPlaywrightDebug(process.env.DEBUG)
  if (debug === undefined) delete process.env.DEBUG
  else process.env.DEBUG = debug
  hide(owner.password)

  const dir = mkdtempSync(join(tmpdir(), 'tt-screens-'))
  const removeDir = () => rmSync(dir, { recursive: true, force: true })
  cleanup.add(removeDir, removeDir)

  // The pictures an earlier run wrote: the only files this run may replace.
  const owned = readManifest(outDir)
  const failures = []
  const issues = new Set()
  const skipped = []
  const written = new Set()
  const failedIds = []
  let server
  let done = 0

  // What a run leaves in the output folder: its own pictures in the manifest, the failed ones
  // removed, and after a run that failed nowhere the earlier run's others pruned.
  const record = createRecorder({ outDir, written, owned, failedIds, skipped, failures })
  cleanup.add(() => record(false))

  try {
    // Left out of the module's top so that listing and testing the screens needs no browser.
    const { chromium } = await import('playwright')

    server = await startServer(dir, { unsafeAgent, cleanup })
    say(`server up (agent runtime ${server.agent})`)
    // Playwright's own signal handling is off: the script's handlers stop the browser, in order.
    const browser = await chromium.launch({ handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false })
    cleanup.add(() => browser.close())

    // A capture is a page in a context of its size and theme; the contexts share the owner's
    // session once there is one.
    const contexts = new Map()
    const contextFor = async ({ size, theme, dir: name }, cookies) => {
      const key = `${name}:${cookies ? 'in' : 'out'}`
      if (!contexts.has(key)) {
        const context = await browser.newContext({
          baseURL: server.base,
          viewport: { width: size.width, height: size.height },
          colorScheme: theme,
          locale: 'en-US',
          timezoneId: 'UTC',
        })
        if (cookies) await context.addCookies(cookies)
        context.setDefaultTimeout(15_000)
        contexts.set(key, context)
      }
      return contexts.get(key)
    }

    // What the browser reported, with the capture it happened in. A signed-out screen's calls
    // for the account get 401: that is the app asking who is there, not a fault.
    const watch = (page, phase, id) => {
      const quiet = phase === 'first-run' || phase === 'signed-out'
      const note = (text) => issues.add(`${id}: ${redact(text)}`)
      page.on('console', (m) => {
        if (m.type() !== 'error') return
        if (quiet && /status of 401/.test(m.text())) return
        note(`console: ${m.text().slice(0, 300)}`)
      })
      page.on('pageerror', (e) => note(`page error: ${e.message.slice(0, 300)}`))
      page.on('requestfailed', (r) => {
        // A request cut off by the next navigation, or a stream closed with its page, is no fault.
        if (/ERR_ABORTED/.test(r.failure()?.errorText ?? '')) return
        note(`request failed: ${r.method()} ${new URL(r.url()).pathname} ${r.failure()?.errorText ?? ''}`)
      })
      page.on('response', (r) => {
        if (r.status() < 400 || (quiet && r.status() === 401)) return
        note(`HTTP ${r.status()}: ${r.request().method()} ${new URL(r.url()).pathname}`)
      })
    }

    const capture = async (c, cookies, ids, workspace) => {
      const context = await contextFor(c, cookies)
      const page = await context.newPage()
      watch(page, c.screen.phase, c.id)
      const s = {
        page,
        ids,
        agent: server.agent.startsWith('on'),
        path: (section, item) =>
          `/w/${workspace}/${section === 'inbox' ? 'notifications' : section}${item ? `/${encodeURIComponent(item)}` : ''}`,
        go: async (path) => {
          await page.goto(path, { waitUntil: 'domcontentloaded' })
        },
      }
      try {
        const result = await captureOne(c, s, { outDir, written, owned })
        if (result.ok) done++
        else {
          failures.push(`${c.id}: ${result.error}`)
          failedIds.push(c.id)
        }
      } finally {
        await page.close().catch(() => {})
      }
    }

    // Each phase's captures run side by side, one per size and theme.
    const runPhase = async (phase, cookies, ids, workspace) => {
      const todo = plan.filter((c) => c.screen.phase === phase)
      for (const c of todo) if (c.screen.needsAgent && !server.agent.startsWith('on')) skipped.push(c.id)
      const byScreen = Map.groupBy(
        todo.filter((c) => !skipped.includes(c.id)),
        (c) => c.screen.name,
      )
      for (const group of byScreen.values()) {
        await Promise.all(group.map((c) => capture(c, cookies, ids, workspace)))
      }
    }

    await runPhase('first-run', undefined)

    // The owner, through the script's own client: the same session goes to every browser context.
    const client = createClient(server.base)
    const api = client.call
    const me = await api('POST', '/api/setup', {
      email: owner.email,
      name: owner.name,
      password: owner.password,
      workspace_name: owner.workspace,
      setup_token: server.token,
    })
    const workspace = me.workspaces[0].id
    const session = client.cookies()

    await runPhase('empty', session, {}, workspace)
    const needsSeed = plan.some((c) => c.screen.phase === 'seeded')
    if (needsSeed) {
      say('seeding the sample data')
      const ids = await seed({ api, base: server.base, workspace, agent: server.agent, say })
      await runPhase('seeded', session, ids, workspace)
    }
    await runPhase('signed-out', undefined)
  } catch (err) {
    failures.push(`the run stopped: ${redact(String(err.message).split('\n')[0])}`)
    const log = (server ?? err.server)?.logs?.().trim()
    if (log) console.error(`server log (last lines):\n${log.split('\n').slice(-15).join('\n')}`)
  } finally {
    // Whatever happened, the browser and the server stop and the data directory goes.
    record(only.length === 0 && runSucceeded({ failures, issues, strict: values.strict }))
    await cleanup.run()
    untrap()
  }

  const seconds = Math.round((Date.now() - started) / 1000)
  say(`${done} of ${plan.length - skipped.length} captures written to ${outDir} in ${seconds} s`)
  if (skipped.length > 0)
    say(
      `${skipped.length} skipped, they need the agent runtime: ${[...new Set(skipped.map((id) => id.split('/')[1]))].join(', ')}`,
    )
  if (server?.agent && !server.agent.startsWith('on')) {
    say(
      `note: the agent runtime was ${server.agent}; set TASKTO_SCREENS_UNSAFE_AGENT=1 on a development machine for the conversation's replies`,
    )
  }
  if (issues.size > 0) {
    say(`\nthe browser reported ${issues.size} problem${issues.size === 1 ? '' : 's'}:`)
    for (const i of [...issues].sort()) say(`  ${i}`)
  } else {
    say('the browser reported no console error and no failed request')
  }
  if (failures.length > 0) {
    say(`\n${failures.length} failed:`)
    for (const f of failures) say(`  ${f}`)
  }
  return runSucceeded({ failures, issues, strict: values.strict }) ? 0 : 1
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main()
}
