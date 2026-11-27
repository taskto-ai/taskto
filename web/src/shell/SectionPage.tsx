import { lazy, type ReactNode, Suspense } from 'react'
import type { Me, Workspace } from '../api'
import { WorkspaceAdmin } from '../apps/admin'
import { BoardsPage } from '../boards/BoardsPage'
import { ConversationsPage } from '../conversations/ConversationsPage'
import { useT } from '../i18n'
import { KnowledgePage } from '../knowledge/KnowledgePage'
import { NotificationsPage } from '../notifications/NotificationsPage'
import type { Route } from '../router'
import { SettingsPage } from '../settings/SettingsPage'
import { Page as PageFrame } from '../ui/Page'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'

const AppsPage = lazy(() => import('../apps/AppsPage').then((m) => ({ default: m.AppsPage })))
const HomePage = lazy(() => import('../home/HomePage').then((m) => ({ default: m.HomePage })))

export function SectionPage({ route, me, workspace }: { route: Route; me: Me; workspace: Workspace }): ReactNode {
  switch (route.section) {
    case 'home':
      return (
        <Suspense fallback={<Loading />}>
          <HomePage workspace={route.workspace} admin={workspace.role === 'admin'} />
        </Suspense>
      )
    case 'conversations':
      return <ConversationsPage workspace={route.workspace} open={route.item} />
    case 'knowledge':
      return (
        <KnowledgePage
          key={route.item ?? ''}
          workspace={route.workspace}
          admin={workspace.role === 'admin'}
          open={route.item}
        />
      )
    case 'boards':
      return <BoardsPage key={route.item ?? ''} workspace={route.workspace} open={route.item} />
    case 'apps':
      return (
        <WorkspaceAdmin value={workspace.role === 'admin'}>
          <Suspense fallback={<Loading />}>
            <AppsPage workspace={route.workspace} open={route.item} />
          </Suspense>
        </WorkspaceAdmin>
      )
    case 'settings':
      return <SettingsPage me={me} workspace={workspace} tab={route.item} />
    case 'notifications':
      return <NotificationsPage workspace={route.workspace} />
  }
}

function Loading() {
  const t = useT()
  return (
    <PageFrame aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      <Skeleton width="40%" />
      <Skeleton shape="block" height="160px" />
      <Skeleton width="70%" />
    </PageFrame>
  )
}
