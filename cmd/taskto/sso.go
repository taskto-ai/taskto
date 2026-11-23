package main

import (
	"taskto.ai/taskto/internal/auth"
)

// loadSSOProviders reads the providers file (none when path is empty) and then removes every
// environment variable a client secret was read from, so that no child process the server
// starts afterwards, the agent sidecar above all, inherits one. File-backed secrets are preferred (docs/sso.md); this is
// for the ones that are not. It also returns the variables' names, for the supervisor to drop
// from its child's environment too.
func loadSSOProviders(path string, getenv func(string) string, unsetenv func(string) error) ([]auth.SSOProvider, []string, error) {
	if path == "" {
		return nil, nil, nil
	}
	var names []string
	providers, err := auth.LoadSSOProviders(path, func(name string) string {
		names = append(names, name)
		return getenv(name)
	})
	for _, name := range names {
		_ = unsetenv(name)
	}
	return providers, names, err
}
