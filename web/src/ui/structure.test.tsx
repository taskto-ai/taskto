// @vitest-environment jsdom

// The page-structure components: Page, PageHeader, Heading, Toolbar, Panel, Rows, Table, EmptyState and
// Notice.

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { createRef } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Button } from './Button'
import { EmptyState } from './EmptyState'
import { Heading } from './Heading'
import { Notice, type NoticeTone } from './Notice'
import { Page } from './Page'
import { PageHeader } from './PageHeader'
import { Panel } from './Panel'
import { Row, Rows } from './Rows'
import { Table } from './Table'
import { Toolbar, ToolbarSpacer } from './Toolbar'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Page and PageHeader', () => {
  test('the width rule is the page’s kind, a list by default', () => {
    const { container, rerender } = render(<Page>x</Page>)
    const page = container.firstElementChild as HTMLElement
    expect(page.dataset.width).toBe('list')
    rerender(<Page width="reading">x</Page>)
    expect(page.dataset.width).toBe('reading')
    rerender(<Page width="full">x</Page>)
    expect(page.dataset.width).toBe('full')
  })

  test('a header: the path, the title as the page’s heading, the help and the actions', () => {
    render(
      <PageHeader
        path={<span>Private</span>}
        title="Personal"
        help="Your own cards."
        actions={<Button variant="primary">New card</Button>}
      />,
    )
    const h1 = screen.getByRole('heading', { level: 1, name: 'Personal' })
    const header = h1.closest('header') as HTMLElement
    expect(header.textContent).toBe('PrivatePersonalYour own cards.New card')
    expect(screen.getByRole('button', { name: 'New card' })).toBeTruthy()
  })

  test('a header given a title ref lets a script, not Tab, move the keyboard to its title', () => {
    const ref = createRef<HTMLHeadingElement>()
    const { rerender } = render(<PageHeader title="Home" titleRef={ref} />)
    const h1 = screen.getByRole('heading', { level: 1, name: 'Home' })
    expect(ref.current).toBe(h1)
    ref.current?.focus()
    expect(document.activeElement).toBe(h1)
    expect(h1.tabIndex).toBe(-1)
    // Without one, the title is not a place for the keyboard at all.
    rerender(<PageHeader title="Home" />)
    expect(screen.getByRole('heading', { level: 1 }).hasAttribute('tabindex')).toBe(false)
  })

  test('a Heading takes a ref, so a script can move the keyboard to it', () => {
    const ref = createRef<HTMLHeadingElement>()
    render(
      <Heading level={2} ref={ref} tabIndex={-1}>
        Set up your workspace
      </Heading>,
    )
    const h2 = screen.getByRole('heading', { level: 2, name: 'Set up your workspace' })
    expect(ref.current).toBe(h2)
    ref.current?.focus()
    expect(document.activeElement).toBe(h2)
  })

  test('a header inside another page is a level 2 heading', () => {
    render(<PageHeader title="Members" level={2} />)
    expect(screen.getByRole('heading', { level: 2, name: 'Members' })).toBeTruthy()
  })
})

describe('Toolbar', () => {
  test('a named group when it has a label, a plain row when not', () => {
    const { rerender } = render(
      <Toolbar label="Filters">
        <Button>Mine</Button>
        <ToolbarSpacer />
      </Toolbar>,
    )
    expect(screen.getByRole('group', { name: 'Filters' })).toBeTruthy()
    rerender(
      <Toolbar>
        <Button>Mine</Button>
      </Toolbar>,
    )
    expect(screen.queryByRole('group')).toBeNull()
  })
})

describe('Panel', () => {
  test('a panel in a panel draws no edge, and says so in development', () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => {})
    const { container } = render(
      <Panel>
        <Panel padded>inner</Panel>
      </Panel>,
    )
    const [outer, inner] = container.querySelectorAll<HTMLElement>('.ui-panel')
    expect(outer.dataset.nested).toBeUndefined()
    expect(inner.dataset.nested).toBe('true')
    expect(inner.dataset.padded).toBe('true')
    expect(error).toHaveBeenCalledOnce()
  })
})

describe('Rows', () => {
  test('a list of rows: title, detail, status and actions; a row that is a link', () => {
    const open = vi.fn()
    render(
      <Rows label="Apps">
        <Row
          title="Equipment"
          detail="Edited 2 hours ago"
          status={<span>Live</span>}
          actions={<Button onClick={open}>Open</Button>}
          href="/w/1/apps/3"
        />
        <Row title="Expenses" current />
      </Rows>,
    )
    const list = screen.getByRole('list', { name: 'Apps' })
    expect(list.querySelectorAll('li')).toHaveLength(2)
    const link = screen.getByRole('link', { name: 'Equipment' })
    expect(link.getAttribute('href')).toBe('/w/1/apps/3')
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    expect(open).toHaveBeenCalledOnce()
    expect(screen.getByText('Edited 2 hours ago')).toBeTruthy()
    expect(screen.getByText('Expenses').closest('li')?.dataset.current).toBe('true')
  })

  test('a row that is a link says it was opened before the app goes there', () => {
    const opened = vi.fn()
    render(
      <Rows>
        <Row title="A card was assigned to you" href="/w/1/boards/3" onOpen={opened} />
      </Rows>,
    )
    fireEvent.click(screen.getByRole('link', { name: 'A card was assigned to you' }))
    expect(opened).toHaveBeenCalledOnce()
    expect(window.location.pathname).toBe('/w/1/boards/3')
  })
})

describe('Table', () => {
  const rows = [
    { plan: 'Solo', seats: 1 },
    { plan: 'Team', seats: 10 },
  ]
  const columns = [
    { key: 'plan', header: 'Plan', cell: (r: (typeof rows)[number]) => r.plan },
    { key: 'seats', header: 'Seats', numeric: true, cell: (r: (typeof rows)[number]) => r.seats },
    { key: 'actions', header: 'Actions', hiddenHeader: true, cell: () => <Button size="sm">Edit</Button> },
  ]

  test('a captioned table with column headers, numbers marked to align at the end', () => {
    render(<Table caption="Plans" columns={columns} rows={rows} rowKey={(r) => r.plan} />)
    const table = screen.getByRole('table', { name: 'Plans' })
    expect(screen.getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Plan', 'Seats', 'Actions'])
    expect(screen.getByRole('columnheader', { name: 'Seats' }).dataset.numeric).toBe('true')
    expect(screen.getByRole('cell', { name: '10' }).dataset.numeric).toBe('true')
    expect(screen.getByRole('cell', { name: 'Team' }).dataset.numeric).toBeUndefined()
    // It doesn't overflow here (jsdom lays nothing out): no tab stop of its own.
    expect(table.parentElement?.getAttribute('tabindex')).toBeNull()
    expect(table.parentElement?.getAttribute('role')).toBeNull()
  })
})

describe('EmptyState', () => {
  test('says what the place is for and offers the action', () => {
    render(
      <EmptyState title="No apps in Product team yet" actions={<Button>Build an app</Button>}>
        Build one with the agent, or move one of yours here to share it.
      </EmptyState>,
    )
    expect(screen.getByText('No apps in Product team yet')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Build an app' })).toBeTruthy()
  })
})

describe('Notice', () => {
  test('its role follows its tone: an alert for danger, a status for the rest, nothing for neutral', () => {
    const roles: Record<NoticeTone, string | null> = {
      danger: 'alert',
      warning: 'status',
      success: 'status',
      info: 'status',
      neutral: null,
    }
    for (const [tone, role] of Object.entries(roles)) {
      const { container, unmount } = render(<Notice tone={tone as NoticeTone}>Words</Notice>)
      const el = container.firstElementChild as HTMLElement
      expect(el.getAttribute('role')).toBe(role)
      expect(el.dataset.tone).toBe(tone)
      // An icon that a screen reader passes over: the words say it.
      expect(el.querySelector('svg')?.closest('[aria-hidden="true"]')).not.toBeNull()
      unmount()
    }
  })

  test('a title, the words and an action', () => {
    render(
      <Notice tone="warning" title="Offline" action={<Button size="sm">Send now</Button>}>
        2 records are waiting to be sent from this device.
      </Notice>,
    )
    const status = screen.getByRole('status')
    expect(status.textContent).toContain('Offline')
    expect(status.textContent).toContain('2 records are waiting')
    expect(screen.getByRole('button', { name: 'Send now' })).toBeTruthy()
  })
})

describe('Heading', () => {
  test('its level is the outline and its size the look, a section by default', () => {
    render(
      <>
        <Heading level={2}>Invites</Heading>
        <Heading level={3} size="title">
          Plans
        </Heading>
      </>,
    )
    const section = screen.getByRole('heading', { level: 2, name: 'Invites' })
    expect(section.className).toBe('ui-heading')
    expect(section.dataset.size).toBe('section')
    expect(screen.getByRole('heading', { level: 3, name: 'Plans' }).dataset.size).toBe('title')
  })
})
