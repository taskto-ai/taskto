package cliowner

import (
	"context"
	"errors"
)

var ErrOtherInstallation = errors.New("the CLI worker's volume belongs to another taskto installation: see docs/cli-workers.md")

var ErrAuthorityLost = errors.New("the CLI worker lost its history of server boots and refuses every deletion until an operator hands it over: see docs/cli-workers.md")

var ErrCensusNeeded = errors.New("the CLI worker takes deletions only after the server's first census: it is tried again")

type key struct{}

func With(ctx context.Context, identity string) context.Context {
	if identity == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, identity)
}

func From(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(key{}).(string)
	return id, ok && id != ""
}

type Account struct {
	User         int64  `json:"user"`
	Identity     string `json:"identity"`
	Boot         string `json:"boot,omitempty"`
	Installation string `json:"installation,omitempty"`
}

func (a Account) Valid() bool { return a.User > 0 && IsIdentity(a.Identity) }

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
