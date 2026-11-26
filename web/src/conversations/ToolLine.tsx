// A tool the agent used, as one compact line: a check (or a spinner while it runs), what it did,
// and the tool's name in the mono face. A CLI's turn has its output as plain text under the line,
// open while the CLI works and folded once it is done, unless the person opened, closed or
// scrolled it; an app the agent saved is launched inline or linked to.

import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import { CLI_PRODUCTS } from '../api'
import { appPath } from '../apps/route'
import { type T, useT } from '../i18n'
import { Link } from '../router'
import { Icon } from '../ui/Icon'
import { Skeleton } from '../ui/Skeleton'
import { Spinner } from '../ui/Spinner'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { type AppRef, CLI_TOOL, HELP_TOOL, type ToolUse } from './transcript'

// The A2UI renderer is most of the app's code: loaded when a conversation shows an app, its chunk
// fetched together with the preview's (the preview draws it lazily), not one after the other.
const AppPreview = lazy(() => {
  void import('../apps/AppView').catch(() => {})
  return import('../apps/AppPreview').then((m) => ({ default: m.AppPreview }))
})

export function ToolLine({
  tool,
  workspace,
  launched,
}: {
  tool: ToolUse
  workspace: number
  // Whether its app shows launched here; otherwise the line links to it.
  launched: boolean
}) {
  const t = useT()
  const line = (
    <>
      {tool.running ? <Spinner size="sm" /> : <Icon name="check" size={14} className="ui-chat-tool-check" />}
      <span className="ui-chat-tool-label">{toolLabel(t, tool)}</span>
    </>
  )
  return (
    <div className="ui-chat-tool">
      {/* A CLI's work is plain text lines, shown as such: never Markdown, never HTML. */}
      {tool.output ? (
        <CliOutput running={Boolean(tool.running)} output={tool.output} line={line} />
      ) : (
        <p className="ui-chat-tool-line">
          {line}
          {tool.app && !launched && (
            <>
              <span aria-hidden="true">{'·'}</span>
              <Link className="ui-chat-tool-link" href={appPath(workspace, tool.app.space, tool.app.app)}>
                {t('conversations.openApp')}
              </Link>
            </>
          )}
        </p>
      )}
      {tool.app && launched && <InlineApp workspace={workspace} app={tool.app} />}
    </div>
  )
}

// A CLI's output under its line: open while it works, folded once it is done, but only if the
// person never touched it: what they opened, closed or scrolled stays as they left it.
function CliOutput({ running, output, line }: { running: boolean; output: string; line: ReactNode }) {
  const [open, setOpen] = useState(running)
  const touched = useRef(false)
  useEffect(() => {
    if (!touched.current) setOpen(running)
  }, [running])
  return (
    <details
      className="ui-chat-tool-output"
      open={open}
      onToggle={(e) => {
        // The browser says so when it follows `open` too: only a change the page didn't ask for is
        // the person's.
        const now = e.currentTarget.open
        if (now !== open) {
          touched.current = true
          setOpen(now)
        }
      }}
    >
      <summary className="ui-chat-tool-line">
        {line}
        <Icon name="chevron-down" size={14} className="ui-chat-tool-chevron" />
      </summary>
      <pre
        className="ui-chat-output"
        onScroll={() => {
          touched.current = true
        }}
        onPointerDown={() => {
          touched.current = true
        }}
      >
        {output}
      </pre>
    </details>
  )
}

// An app the agent saved, launched in the conversation, in its own frame.
function InlineApp({ workspace, app }: { workspace: number; app: AppRef }) {
  const t = useT()
  return (
    <Suspense
      fallback={
        <div className="ui-chat-app-loading" aria-busy="true">
          <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
          <Skeleton shape="block" height="8rem" />
        </div>
      }
    >
      <AppPreview workspace={workspace} space={app.space} app={app.app} />
    </Suspense>
  )
}

// What a tool call shows as: a CLI's turn as that CLI's work, any other as the tool it used, its
// name in the mono face.
function toolLabel(t: T, tool: ToolUse): ReactNode {
  if (tool.name === HELP_TOOL) return t('conversations.tool.help')
  if (tool.name !== CLI_TOOL)
    return t.rich('conversations.tool.used', {
      name: (
        <code key="name" className="ui-chat-tool-name">
          {tool.name}
        </code>
      ),
    })
  if (!tool.cli) return tool.running ? t('conversations.tool.someCliWorking') : t('conversations.tool.someCliWorked')
  const cli = CLI_PRODUCTS[tool.cli]
  return tool.running ? t('conversations.tool.cliWorking', { cli }) : t('conversations.tool.cliWorked', { cli })
}
