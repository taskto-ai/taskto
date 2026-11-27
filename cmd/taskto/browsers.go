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

func startBrowsers(cfg config.Config, cli *cliaccounts.Service) (*agentbrowser.Service, browser.Browsers, func()) {
	container := cli.Mode() == string(cliaccounts.Container)
	if !cfg.Browser {
		if container {
			return agentbrowser.New(nil, nil), browser.NewRemote(cfg.CLIWorkerSocket), func() {}
		}
		return agentbrowser.New(nil, nil), nil, func() {}
	}
	if container {
		slog.Info("agent browser on, in the CLI worker")
		remote := browser.NewRemote(cfg.CLIWorkerSocket)
		return agentbrowser.New(remote, cli.Usable), remote, func() {}
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
	return agentbrowser.New(m, cli.Usable), m, m.Close
}
