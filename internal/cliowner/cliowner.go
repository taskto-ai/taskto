// Package cliowner carries, with a request's context, the identity of the account a destructive
// call to the CLI worker is meant for (users.cli_identity): a deleted account's purge says whose
// files it removes, and the worker acts only while the number's directory is still that account's.
// A restore of the server's data gives numbers again, and a call that comes late must not reach the
// account given the number since (docs/cli-workers.md).
package cliowner

import "context"

type key struct{}

// With returns ctx carrying identity; an empty identity carries none.
func With(ctx context.Context, identity string) context.Context {
	if identity == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, identity)
}

// From is the identity ctx carries, if any.
func From(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(key{}).(string)
	return id, ok && id != ""
}
