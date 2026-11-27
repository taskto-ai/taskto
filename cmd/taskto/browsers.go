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

// startBrowsers returns the agent browser (docs/agent-browser.md) for cfg, the browsers a deleted
// account's purge stops and removes the files of (nil when there are none to stop), and a stop that
// ends every browser. It runs where the CLIs do and under the same rule (cli.Usable):
//
//   - in container mode, in the CLI worker, as each account's Unix user (a Remote client);
//   - in local mode, in this process, as the server's own OS user, each account's browser in its
//     CLI home (<data>/cli/h<home>/browser), with Chromium's sandbox on.
//
// The browsers know an account by its identity (cli.Account), never by its number alone. Off (the
// default), its API and tools say so. A container worker keeps its browsers whatever the server's
// setting (one may still run from when it was on, and its files are in the worker), so the purge of
// an account reaches it anyway.
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

// purgeBrowsers is the browsers as a deleted account's purge reaches them (accountdeletion.Browsers),
// by user number: the account named is the one the purge's step keeps (internal/cliowner), or else
// the account's if it is still there, or else none (a purge from before identities were kept).
type purgeBrowsers struct {
	b   browser.Browsers
	cli *cliaccounts.Service
}

// Forget implements accountdeletion.Browsers.
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

// browserUsable is the CLI workers' rule for the agent browser (cli.Usable).
func browserUsable(cli *cliaccounts.Service) func(context.Context, int64) error {
	return func(ctx context.Context, _ int64) error { return cli.Usable(ctx) }
}
