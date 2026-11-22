package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"taskto.ai/taskto/internal/agentbrowser"
	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/config"
)

// startBrowsers returns the agent browser (docs/agent-browser.md) for cfg, and a stop that
// ends every browser. It runs where the CLIs do and under the same rule (cli.Usable):
//
//   - in container mode, in the CLI worker, as each user's Unix user (a Remote client);
//   - in local mode, in this process, as the server's own OS user, each user's browser in
//     their CLI directory (<data>/cli/u<id>/browser), with Chromium's sandbox on.
//
// Off (the default), its API and tools say so.
func startBrowsers(cfg config.Config, cli *cliaccounts.Service) (*agentbrowser.Service, func()) {
	if !cfg.Browser {
		return agentbrowser.New(nil, nil), func() {}
	}
	if cli.Mode() == string(cliaccounts.Container) {
		slog.Info("agent browser on, in the CLI worker")
		return agentbrowser.New(browser.NewRemote(cfg.CLIWorkerSocket), cli.Usable), func() {}
	}
	bins := browser.FindBins(cfg.BrowserChromium)
	m := browser.NewManager(browser.Config{Host: &browser.LocalHost{
		Root: filepath.Join(cfg.DataDir, "cli"),
		Bins: bins,
		Path: os.Getenv("PATH"),
	}})
	slog.Info("agent browser on, in this process", "personal", m.Available(browser.Personal), "testing", m.Available(browser.Testing))
	if !m.Available(browser.Testing) {
		slog.Warn("agent browser: no Chromium found (chromium on the PATH, or -browser-chromium)")
	}
	return agentbrowser.New(m, cli.Usable), m.Close
}
