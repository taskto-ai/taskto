// Package apitypes writes the TypeScript shapes of the server's JSON replies from the Go
// structs that make them, so the web app can't drift from the server without a build failing.
//
// Roots lists the structs the web app reads. `go generate ./internal/apitypes` writes
// web/src/api.gen.ts from them, and TestGeneratedIsCurrent fails when the committed file is
// not what the structs say (so renaming a field in Go fails `go test` until the file is
// regenerated). web/src/api.contract.ts then checks each type the web app declares in
// web/src/api.ts against its generated shape, so the regenerated file fails `pnpm typecheck`
// wherever the web app still reads the old field. Nothing here is linked into the server's
// binary, and no Go code reads the web app's files except that test.
//
// To cover another reply: add its struct to Roots, run `go generate ./internal/apitypes`, and
// add a line to web/src/api.contract.ts (docs/api.md#api-types).
package apitypes

//go:generate go run ./generate

import (
	"reflect"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/notifications"
)

// Root is a reply's struct and the name its TypeScript type has in api.gen.ts.
type Root struct {
	Name string
	Type reflect.Type
}

func root[T any](name string) Root { return Root{Name: name, Type: reflect.TypeFor[T]()} }

// Output is where the generated file is, relative to this package's directory.
const Output = "../../web/src/api.gen.ts"

// Roots are the replies the web app reads, each under the name its generated type has. The
// structs they hold are written too, named for their package (AccessAppAccess).
func Roots() []Root {
	return []Root{
		// Accounts and workspaces.
		root[auth.Me]("Me"),
		root[auth.Membership]("Workspace"),
		root[auth.Member]("Member"),
		root[auth.Invite]("Invite"),
		root[auth.Token]("Token"),

		// Spaces and the knowledge base.
		root[access.Space]("Space"),
		root[access.AppAccess]("AppAccess"),
		root[knowledge.Entry]("DocEntry"),
		root[knowledge.DocReply]("Doc"),

		// Boards.
		root[boards.Board]("Board"),
		root[boards.Card]("Card"),
		root[boards.Comment]("Comment"),

		// Conversations and notifications.
		root[conversations.Conversation]("Conversation"),
		root[notifications.Item]("NotificationItem"),
		root[notifications.List]("NotificationList"),

		// Apps.
		root[apps.App]("App"),
		root[apps.Release]("AppRelease"),
		root[apps.Record]("AppRecord"),
		root[apps.Version]("RecordVersion"),
		root[apps.EffectView]("AppEffect"),
		root[apps.EffectList]("AppEffectList"),
		root[apps.EffectTarget]("EffectTarget"),
		root[apps.OfflineApp]("OfflineApp"),
		root[apps.TemplateInfo]("AppTemplate"),
		root[apps.Widget]("Widget"),
		root[apps.Pin]("Pin"),
		root[apps.HomeItem]("HomeItem"),
		root[apps.HomeView]("HomeView"),
		root[apps.AvailableApp]("AvailableApp"),
	}
}
