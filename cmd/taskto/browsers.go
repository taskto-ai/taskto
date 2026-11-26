package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"taskto.ai/taskto/internal/agentbrowser"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/config"
)

// startBrowsers returns the agent browser (docs/agent-browser.md) for cfg, the browsers a deleted
// account's purge stops and removes the files of (nil when there are none to stop), and a stop that
// ends every browser. It runs where the CLIs do and under the same rule (cli.Usable):
//
//   - in container mode, in the CLI worker, as each user's Unix user (a Remote client);
//   - in local mode, in this process, as the server's own OS user, each user's browser in
//     their CLI directory (<data>/cli/u<id>/browser), with Chromium's sandbox on.
//
// Off (the default), its API and tools say so. A container worker keeps its browsers whatever the
// server's setting (one may still run from when it was on, and its files are in the worker), so the
// purge of an account reaches it anyway.
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
		return agentbrowser.New(remote, browserUsable(cli)), remote, func() {}
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
	return agentbrowser.New(m, browserUsable(cli)), m, m.Close
}

// browserUsable is the CLI workers' rule for the agent browser, and, since a user's profile is in
// their CLI directory, that the worker has been told who exists since this server started and who
// the user is (cliaccounts.Service.ReadyUser): until then a directory a restore left there may be
// someone else's. Its refusals carry their own status and code, for the web app.
func browserUsable(cli *cliaccounts.Service) func(context.Context, int64) error {
	return func(ctx context.Context, user int64) error {
		if err := cli.Usable(ctx); err != nil {
			return err
		}
		switch err := cli.ReadyUser(ctx, user); {
		case err == nil:
			return nil
		case errors.Is(err, cliworker.ErrForgotten):
			return apierr.New(http.StatusGone, apierr.CodeCLIAccountForgotten, cliworker.ErrForgotten.Error())
		case errors.Is(err, cliworker.ErrBusy):
			return apierr.New(http.StatusConflict, apierr.CodeCLIAccountBusy, cliworker.ErrBusy.Error())
		default:
			return apierr.New(http.StatusServiceUnavailable, apierr.CodeCLIWorkerNotReady, cliaccounts.ErrNotReady.Error())
		}
	}
}
