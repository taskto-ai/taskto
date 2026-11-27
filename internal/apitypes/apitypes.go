package apitypes

//go:generate go run ./generate

import (
	"reflect"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/updatestatus"
)

type Root struct {
	Name string
	Type reflect.Type
}

func root[T any](name string) Root { return Root{Name: name, Type: reflect.TypeFor[T]()} }

const Output = "../../web/src/api.gen.ts"

func Roots() []Root {
	return []Root{
		root[auth.Me]("Me"),
		root[auth.Membership]("Workspace"),
		root[auth.Member]("Member"),
		root[auth.Invite]("Invite"),
		root[auth.Token]("Token"),
		root[auth.InviteCreated]("InviteCreated"),
		root[auth.TokenCreated]("TokenCreated"),
		root[auth.DeletionStatus]("DeletionStatus"),
		root[auth.DeletionReply]("DeletionReply"),
		root[auth.PendingDeletion]("PendingDeletion"),

		root[access.Space]("Space"),
		root[access.AppAccess]("AppAccess"),
		root[knowledge.Entry]("DocEntry"),
		root[knowledge.DocReply]("Doc"),

		root[boards.Board]("Board"),
		root[boards.Card]("Card"),
		root[boards.Comment]("Comment"),
		root[boards.Thread]("CardThread"),

		root[conversations.Conversation]("Conversation"),
		root[notifications.Item]("NotificationItem"),
		root[notifications.List]("NotificationList"),

		root[apps.App]("App"),
		root[apps.Release]("AppRelease"),
		root[apps.Record]("AppRecord"),
		root[apps.Version]("RecordVersion"),
		root[apps.EffectView]("AppEffect"),
		root[apps.EffectList]("AppEffectList"),
		root[apps.EffectTarget]("EffectTarget"),
		root[apps.OfflineApp]("OfflineApp"),
		root[apps.IntegrationStatus]("AppIntegrationStatus"),
		root[apps.IntegrationApproval]("AppIntegrationApproval"),
		root[apps.ApprovalList]("AppIntegrationApprovalList"),
		root[apps.TemplateInfo]("AppTemplate"),
		root[apps.Widget]("Widget"),
		root[apps.Pin]("Pin"),
		root[apps.HomeItem]("HomeItem"),
		root[apps.HomeView]("HomeView"),
		root[apps.AvailableApp]("AvailableApp"),

		root[integrations.Integration]("Integration"),
		root[integrations.SpaceIntegration]("SpaceIntegration"),
		root[integrations.Registry]("IntegrationRegistry"),
		root[integrations.CheckResult]("IntegrationCheck"),

		root[updatestatus.Report]("UpdateReport"),
		root[updatestatus.Requested]("UpdateRequested"),
		root[cliaccounts.WorkerStatus]("CLIWorkerStatus"),
	}
}
