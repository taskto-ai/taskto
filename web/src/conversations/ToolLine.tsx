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

const AppPreview = lazy(() => {
  void import('../apps/AppView').catch(() => {})
  return import('../apps/AppPreview').then((m) => ({ default: m.AppPreview }))
})

export function ToolLine({ tool, workspace, launched }: { tool: ToolUse; workspace: number; launched: boolean }) {
  const t = useT()
  const line = (
    <>
      {tool.running ? <Spinner size="sm" /> : <Icon name="check" size={14} className="ui-chat-tool-check" />}
      <span className="ui-chat-tool-label">{toolLabel(t, tool)}</span>
    </>
  )
  return (
    <div className="ui-chat-tool">
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
