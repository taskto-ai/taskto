//go:build !unix

package main

import (
	"context"
	"errors"
	"io"
	"net/http"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/providers"
)

func startAgent(context.Context, config.Config, *providers.Store, http.Handler, []providers.CLIProvider, []string) (agentRuntime, error) {
	return agentRuntime{}, errors.New("agent runtime: needs Linux, macOS or another Unix system")
}

func runConversations(context.Context, []string, func(string) string, io.Writer, io.Writer) error {
	return errors.New("needs the agent runtime, which needs Linux, macOS or another Unix system")
}
