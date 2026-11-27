package main

import (
	"taskto.ai/taskto/internal/auth"
)

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
