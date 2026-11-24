// The first-run checklist an admin sees on Home: the steps that make a new workspace useful to a
// team, each linking to the page where it is done. A step is done when the server says so (the
// team has a member or an invite, a model provider exists, a shared space, an app), never by a
// tick; the admin can skip one that doesn't apply (a one-person workspace has no team to
// invite), and put the whole list away. It is the workspace's: another admin sees the same.
//
// It is an extra on Home: while it loads, or if it can't be loaded, there is nothing to show and
// the page is as it was without it. It is shown until every step that isn't optional is done or
// skipped, or it is dismissed; the Customize panel brings a dismissed one back. When it goes away
// under the keyboard (dismissed, or its last step skipped) focus moves to the page's heading, and
// finishing it is announced; when it comes back, focus moves to its own heading.

import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { api, type Checklist, type ChecklistStep, errorText, ws } from '../api'
import { templatesPath } from '../apps/route'
import { type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'

// Where each step is done.
function stepPath(workspace: number, step: ChecklistStep): string {
  switch (step) {
    case 'invite':
      return routePath({ workspace, section: 'settings', item: 'members' })
    case 'provider':
      return routePath({ workspace, section: 'settings', item: 'providers' })
    case 'space':
      return routePath({ workspace, section: 'settings', item: 'spaces' })
    case 'app':
      return templatesPath(workspace)
    case 'email':
    case 'backups':
      // Set by whoever runs the server; the page shows how they are doing.
      return routePath({ workspace, section: 'settings', item: 'about' })
  }
}

const checklistPath = (workspace: number) => `${ws(workspace)}/checklist`

// The list and the control that brings it back are two components that load it on their own: a
// change made in one tells the other, as a reload of what is shown. It isn't Home's own version
// (store.ts), whose change empties what is loaded and would take away the button being used, or the
// error being read, while the answer is on its way.
let changes = 0
const listeners = new Set<() => void>()

function checklistChanged() {
  changes++
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// Set by the control that brings the list back, taken by the list when it is shown again, so the
// keyboard follows the button that is gone.
let focusWhenShown = false

function useChecklist(workspace: number) {
  const load = useLoad(() => api<Checklist>(checklistPath(workspace)), [workspace])
  const seen = useSyncExternalStore(subscribe, () => changes)
  const loaded = useRef(seen)
  const { reload } = load
  useEffect(() => {
    if (loaded.current === seen) return
    loaded.current = seen
    reload()
  }, [seen, reload])
  return load
}

export function SetupChecklist({
  workspace,
  onDismissed,
  onCompleted,
}: {
  workspace: number
  // Called once the checklist was put away, and once its last step was handled: the shown list is
  // gone, and with it the keyboard's place.
  onDismissed?: () => void
  onCompleted?: () => void
}) {
  const t = useT()
  const f = useFormat()
  const list = useChecklist(workspace)
  const [error, setError] = useState<string | null>(null)
  const [finished, setFinished] = useState(false)
  const title = useRef<HTMLHeadingElement>(null)
  const data = list.data
  const shown = !!data && !data.dismissed && !data.complete

  // A list that was shown and now has nothing left to do was finished by what the person did here.
  const wasShown = useRef(false)
  const completed = useRef(onCompleted)
  completed.current = onCompleted
  const complete = !!data && !data.dismissed && data.complete
  useEffect(() => {
    if (complete && wasShown.current) {
      setFinished(true)
      completed.current?.()
    }
    if (shown) setFinished(false)
    wasShown.current = shown
  }, [complete, shown])

  // Brought back from Customize: the button that had the keyboard is gone.
  useEffect(() => {
    if (shown && focusWhenShown) {
      focusWhenShown = false
      title.current?.focus()
    }
  }, [shown])

  // Always there, so that what it says when the list is finished is announced.
  const status = (
    <p role="status" className="tt-sr-only">
      {finished ? t('checklist.completed') : ''}
    </p>
  )
  if (!data || !shown) return status

  // Whether the change was made; the list is loaded again either way.
  const run = async (change: () => Promise<unknown>) => {
    let ok = false
    try {
      await change()
      setError(null)
      ok = true
    } catch (e) {
      setError(errorText(e))
    }
    checklistChanged()
    return ok
  }
  const dismiss = async () => {
    if (await run(() => api(`${checklistPath(workspace)}/dismissed`, { method: 'PUT' }))) onDismissed?.()
  }
  const toggleSkip = (step: ChecklistStep, skipped: boolean) =>
    run(() => api(`${checklistPath(workspace)}/skipped/${step}`, { method: skipped ? 'DELETE' : 'PUT' }))

  const required = data.items.filter((i) => !i.optional)
  const handled = required.filter((i) => i.state !== 'todo').length
  const first = data.items.find((i) => i.state === 'todo' && !i.optional)?.id
  const problem = error ?? list.error
  return (
    <>
      <section className="tt-checklist" aria-labelledby="tt-checklist-title">
        <div className="tt-checklist-head">
          <div>
            <h2 id="tt-checklist-title" ref={title} tabIndex={-1}>
              {t('checklist.title')}
            </h2>
            <p className="tt-muted tt-small">{t('checklist.intro')}</p>
          </div>
          <button type="button" className="tt-button" onClick={dismiss}>
            {t('checklist.dismiss')}
          </button>
        </div>
        <p role="status" className="tt-small">
          {t('checklist.progress', { done: f.number(handled), total: f.number(required.length) })}
        </p>
        {problem && (
          <p role="alert" className="tt-error">
            {problem}
          </p>
        )}
        <ol className="tt-checklist-steps">
          {data.items.map((item) => {
            const name = t(`checklist.steps.${item.id}.title` as Key)
            return (
              <li key={item.id} className="tt-checklist-step" data-state={item.state}>
                <span className="tt-checklist-mark" data-state={item.state} aria-hidden="true" />
                <div className="tt-checklist-text">
                  <h3>
                    {name}
                    <span className="tt-checklist-state" data-state={item.state}>
                      {t(`checklist.state.${item.state}` as Key)}
                    </span>
                    {item.optional && <span className="tt-checklist-state">{t('checklist.optional')}</span>}
                  </h3>
                  <p className="tt-muted tt-small">{t(`checklist.steps.${item.id}.text` as Key)}</p>
                </div>
                {item.state !== 'done' && (
                  <div className="tt-checklist-actions">
                    <Link
                      className={item.id === first ? 'tt-button tt-primary' : 'tt-button'}
                      href={stepPath(workspace, item.id)}
                    >
                      {t(`checklist.steps.${item.id}.action` as Key)}
                    </Link>
                    <button
                      type="button"
                      className="tt-button"
                      aria-label={t(item.state === 'skipped' ? 'checklist.unskipNamed' : 'checklist.skipNamed', {
                        name,
                      })}
                      onClick={() => toggleSkip(item.id, item.state === 'skipped')}
                    >
                      {t(item.state === 'skipped' ? 'checklist.unskip' : 'checklist.skip')}
                    </button>
                  </div>
                )}
              </li>
            )
          })}
        </ol>
      </section>
      {status}
    </>
  )
}

// In Home's Customize panel, for admins: the way back to a checklist that was put away.
export function ChecklistRestore({ workspace }: { workspace: number }) {
  const t = useT()
  const list = useChecklist(workspace)
  const [error, setError] = useState<string | null>(null)
  // Nothing to bring back once there is nothing left to do.
  if (!list.data?.dismissed || list.data.complete) return null
  const show = async () => {
    try {
      await api(`${checklistPath(workspace)}/dismissed`, { method: 'DELETE' })
      setError(null)
      focusWhenShown = true
    } catch (e) {
      setError(errorText(e))
    }
    checklistChanged()
  }
  return (
    <section aria-labelledby="tt-home-checklist">
      <h2 id="tt-home-checklist">{t('checklist.title')}</h2>
      <p className="tt-muted tt-small">{t('checklist.dismissedNote')}</p>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <button type="button" className="tt-button" onClick={show}>
        {t('checklist.show')}
      </button>
    </section>
  )
}
