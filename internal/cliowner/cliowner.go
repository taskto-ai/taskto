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

// Account names an account to the CLI worker: its number on this server, which a restore of the
// server's data may give to another account, its identity, which is its own for good and never
// another's (users.cli_identity), and the boot of the server that sends the request (a random value
// each server process takes when it starts). The worker keeps everything of an account under its
// identity, never under its number.
type Account struct {
	User     int64  `json:"user"`
	Identity string `json:"identity"`
	Boot     string `json:"boot,omitempty"`
}

// Valid reports whether a names an account: a number and a well-formed identity.
func (a Account) Valid() bool { return a.User > 0 && IsIdentity(a.Identity) }

// IsIdentity reports whether s is a well-formed identity, or boot: 32 lowercase hex digits.
func IsIdentity(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
