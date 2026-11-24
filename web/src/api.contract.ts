// The types api.ts declares for the server's replies, each checked against the shape the Go
// struct that makes the reply has (api.gen.ts, written by `go generate ./internal/apitypes`).
// If a field is renamed, retyped or made optional in Go and api.gen.ts is regenerated, the line
// of its type stops compiling, and the error says which field. Nothing here exists at run time.
// To cover another reply, list its struct in internal/apitypes and add a line here (docs/api.md,
// "API types").

import type * as Api from './api'
import type * as Gen from './api.gen'
import type { Check } from './contract'
import type * as Notifications from './notifications/api'

type Is<T extends true> = T

export type Contract = [
  // Accounts and workspaces.
  Is<Check<Gen.Me, Api.Me>>,
  Is<Check<Gen.Workspace, Api.Workspace>>,
  Is<Check<Gen.Member, Api.Member>>,
  Is<Check<Gen.Invite, Api.Invite>>,
  Is<Check<Gen.Token, Api.Token>>,
  Is<Check<Gen.InviteCreated, Api.InviteCreated>>,
  Is<Check<Gen.TokenCreated, Api.TokenCreated>>,
  // Spaces and the knowledge base.
  Is<Check<Gen.Space, Api.Space>>,
  Is<Check<Gen.Doc, Api.Doc>>,
  // Boards.
  Is<Check<Gen.Board, Api.Board>>,
  // An assignee is told apart by its kind: a user's has an ID, the agent's has none.
  Is<Check<Gen.Card, Api.Card, 'assignee.id'>>,
  Is<Check<Gen.Comment, Api.Comment>>,
  Is<Check<Gen.CardThread, Api.CardWithThread, 'assignee.id'>>,
  Is<Check<Gen.BoardsAuthor, NonNullable<Api.Author>>>,
  // Conversations and notifications.
  Is<Check<Gen.Conversation, Api.Conversation>>,
  Is<Check<Gen.NotificationItem, Notifications.NotificationItem>>,
  Is<Check<Gen.NotificationList, Notifications.NotificationList>>,
  // Apps.
  Is<Check<Gen.App, Api.App>>,
  Is<Check<Gen.AppAccess, Api.AppAccess>>,
  Is<Check<Gen.AppRelease, Api.AppRelease>>,
  Is<Check<Gen.AppsField, Api.AppField>>,
  Is<Check<Gen.AppRecord, Api.AppRecord>>,
  Is<Check<Gen.RecordVersion, Api.RecordVersion>>,
  Is<Check<Gen.AppEffect, Api.AppEffect>>,
  Is<Check<Gen.AppEffectList, Api.AppEffectList>>,
  Is<Check<Gen.EffectTarget, Api.EffectTarget>>,
  Is<Check<Gen.OfflineApp, Api.OfflineApp>>,
  Is<Check<Gen.AppTemplate, Api.AppTemplate>>,
  Is<Check<Gen.Widget, Api.Widget>>,
  Is<Check<Gen.Pin, Api.Pin>>,
  Is<Check<Gen.HomeItem, Api.HomeItem>>,
  Is<Check<Gen.HomeView, Api.HomeView>>,
  Is<Check<Gen.AvailableApp, Api.AvailableApp>>,
]
