// A tool the agent used, as one line: a CLI's turn with its output as plain text, and an app the
// agent saved, launched inline or linked to.

import { lazy, Suspense } from 'react'
import { CLI_PRODUCTS } from '../api'
import { appPath } from '../apps/route'
import { type T, useT } from '../i18n'
import { Link } from '../router'
import { type AppRef, CLI_TOOL, HELP_TOOL, type ToolUse } from './transcript'

// The A2UI renderer is most of the app's code: loaded when a conversation shows an app.
const AppPreview = lazy(() => import('../apps/AppPreview').then((m) => ({ default: m.AppPreview })))

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
  return (
    <div className="tt-tool-use">
      <p className="tt-tool">
        {toolLabel(t, tool)}
        {tool.app && !launched && (
          <>
            {' · '}
            <Link href={appPath(workspace, tool.app.space, tool.app.app)}>{t('conversations.openApp')}</Link>
          </>
        )}
      </p>
      {/* A CLI's work is plain text lines, shown as such: never Markdown, never HTML. */}
      {tool.output && <pre className="tt-cli-output">{tool.output}</pre>}
      {tool.app && launched && <InlineApp workspace={workspace} app={tool.app} />}
    </div>
  )
}

// An app the agent saved, launched in the conversation.
function InlineApp({ workspace, app }: { workspace: number; app: AppRef }) {
  const t = useT()
  return (
    <Suspense fallback={<p className="tt-muted tt-small">{t('app.loading')}</p>}>
      <AppPreview workspace={workspace} space={app.space} app={app.app} />
    </Suspense>
  )
}

// What a tool call shows as: a CLI's turn as that CLI's work, any other as the tool it used.
function toolLabel(t: T, tool: ToolUse): string {
  if (tool.name === HELP_TOOL) return t('conversations.tool.help')
  if (tool.name !== CLI_TOOL) return t('conversations.tool.used', { name: tool.name })
  if (!tool.cli) return tool.running ? t('conversations.tool.someCliWorking') : t('conversations.tool.someCliWorked')
  const cli = CLI_PRODUCTS[tool.cli]
  return tool.running ? t('conversations.tool.cliWorking', { cli }) : t('conversations.tool.cliWorked', { cli })
}
