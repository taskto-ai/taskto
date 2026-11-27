package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"taskto.ai/taskto/internal/agentbrowser"
	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/config"
)

func startBrowsers(cfg config.Config, cli *cliaccounts.Service) (*agentbrowser.Service, purgeBrowsers, func()) {
	container := cli.Mode() == string(cliaccounts.Container)
	if !cfg.Browser {
		if container {
			return agentbrowser.New(nil, nil, cli.Account), purgeBrowsers{browser.NewRemote(cfg.CLIWorkerSocket), cli}, func() {}
		}
		return agentbrowser.New(nil, nil, cli.Account), purgeBrowsers{}, func() {}
	}
	if container {
		slog.Info("agent browser on, in the CLI worker")
		remote := browser.NewRemote(cfg.CLIWorkerSocket)
		return agentbrowser.New(remote, browserUsable(cli), cli.Account), purgeBrowsers{remote, cli}, func() {}
	}
	bins := browser.FindBins(cfg.BrowserChromium)
	m := browser.NewManager(browser.Config{Host: &browser.LocalHost{
		Root:  filepath.Join(cfg.DataDir, "cli"),
		Home:  cli.LocalHome,
		Found: cli.LocalHomeFound,
		Bins:  bins,
		Path:  os.Getenv("PATH"),
	}})
	slog.Info("agent browser on, in this process", "personal", m.Available(browser.Personal), "testing", m.Available(browser.Testing))
	if !m.Available(browser.Testing) {
		slog.Warn("agent browser: no Chromium found (chromium on the PATH, or -browser-chromium)")
	}
	return agentbrowser.New(m, browserUsable(cli), cli.Account), purgeBrowsers{m, cli}, m.Close
}

type purgeBrowsers struct {
	b   browser.Browsers
	cli *cliaccounts.Service
}

func (p purgeBrowsers) Forget(ctx context.Context, user int64) error {
	if p.b == nil {
		return nil
	}
	inst, err := p.cli.Installation(ctx)
	if err != nil {
		return err
	}
	a := cliowner.Account{User: user, Boot: p.cli.Boot(), Installation: inst}
	if id, ok := cliowner.From(ctx); ok {
		a.Identity = id
	} else if found, err := p.cli.Account(ctx, user); err == nil {
		a = found
	} else if !errors.Is(err, cliworker.ErrForgotten) {
		return err
	}
	return p.b.Forget(ctx, a)
}

func browserUsable(cli *cliaccounts.Service) func(context.Context, int64) error {
	return func(ctx context.Context, _ int64) error { return cli.Usable(ctx) }
}
