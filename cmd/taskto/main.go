package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "time/tzdata"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/accountdeletion"
	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/dictation"
	"taskto.ai/taskto/internal/drain"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/live"
	"taskto.ai/taskto/internal/lockfile"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/privcheck"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/quota"
	"taskto.ai/taskto/internal/schedule"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/server"
	"taskto.ai/taskto/internal/serverkey"
	"taskto.ai/taskto/internal/webui"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == cliworker.HelperArg {
		os.Exit(cliworker.RunHelper())
	}
	if len(os.Args) > 1 && os.Args[1] == cliworker.ApproveArg {
		os.Exit(cliworker.RunApprove(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "cli-worker" {
		if err := runCLIWorker(os.Args[2:], os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "taskto cli-worker:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "backup" || os.Args[1] == "backup-schedule" || os.Args[1] == "restore" || os.Args[1] == "conversations" || os.Args[1] == "agent-compact") {
		os.Exit(subcommand(os.Args[1], os.Args[2:]))
	}

	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "taskto:", err)
		os.Exit(2)
	}
	if cfg.ShowVersion {
		fmt.Println(version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, nil); err != nil {
		stop()
		slog.Error("taskto stopped", "err", err)
		os.Exit(1)
	}
}

func subcommand(name string, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch name {
	case "backup":
		err = runBackup(ctx, args, os.Getenv, os.Stdout, os.Stderr)
	case "backup-schedule":
		err = runBackupSchedule(ctx, args, os.Getenv, os.Stderr)
	case "restore":
		err = runRestore(ctx, args, os.Getenv, os.Stdin, os.Stderr)
	case "conversations":
		err = runConversations(ctx, args, os.Getenv, os.Stdout, os.Stderr)
	case "agent-compact":
		err = runAgentCompact(ctx, args, os.Getenv, os.Stderr)
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintf(os.Stderr, "taskto %s: %v\n", name, err)
		return 1
	}
	return 0
}

type agentRuntime struct {
	state func() string
	ready func(context.Context) (*agentrt.Client, error)
	feed  *providers.Feed
	stop  func() error
}

func run(ctx context.Context, cfg config.Config, listening func(net.Addr)) (result error) {
	stuck := &shutdownReport{}
	defer func() { result = errors.Join(result, stuck.err()) }()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if cfg.Agent || cfg.CLIWorkers == "local" || cfg.CLIWorkers == "local-unsafe" {
		what := "this server, whose user the agent sidecar and local CLI workers run as,"
		if err := privcheck.Enforce(what, privcheck.Process{}.Problems(ctx), cfg.UnsafePrivilegedAgents); err != nil {
			return err
		}
	}
	ssoProviders, ssoSecretEnv, err := loadSSOProviders(cfg.SSOProviders, os.Getenv, os.Unsetenv)
	if err != nil {
		return err
	}
	mailer, err := loadMail(cfg, os.Unsetenv)
	if err != nil {
		return err
	}
	if mailer.Configured() {
		info := mailer.Info()
		slog.Info("outgoing email is on", "host", info.Host, "port", info.Port, "security", info.Security,
			"sign_in", info.SignsIn, "egress", cfg.SMTPEgressPolicy().String())
		if cfg.PublicURL == "" {
			slog.Warn("reset and invite links aren't emailed: set public_url, the server's external address, which the links are built on")
		}
	}
	lock, err := lockfile.DataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if stuck.busy {
			keepLock(lock)
			slog.Error("shutdown: keeping the data directory's lock until the process exits")
			return
		}
		_ = lock.Close()
	}()
	dbPath := filepath.Join(cfg.DataDir, db.FileName)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if stuck.busy {
			slog.Error("shutdown: not closing the database, work that uses it is still running; the process is exiting")
			return
		}
		_ = sqlDB.Close()
	}()
	applied, err := db.Migrate(ctx, sqlDB)
	if err != nil {
		return err
	}
	for _, name := range applied {
		slog.Info("applied migration", "name", name)
	}

	key, err := serverkey.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	store := providers.NewStore(sqlDB, key)

	auditCtx, stopAudit := context.WithCancel(ctx)
	auditDone := make(chan struct{})
	go func() {
		defer close(auditDone)
		audit.Retain(auditCtx, sqlDB, cfg.AuditRetention(), time.Hour)
	}()
	defer func() {
		stopAudit()
		<-auditDone
	}()

	kb := knowledge.New(cfg.DataDir, knowledge.Options{
		MaxFileSize: cfg.KBMaxFileSize, LargeFileSize: cfg.KBLargeFileSize,
		MaxRepoSize: cfg.KBMaxRepoSize, MaxUnversionedSize: cfg.KBMaxUnversionedSize,
	})
	quotas := quota.New(sqlDB, quota.Limits{Workspace: cfg.StorageQuotaWorkspace, User: cfg.StorageQuotaUser}, kb)
	kb.SetQuota(quotas.KnowledgeCheck())
	kb.SetSpaceCheck(access.SpaceRowCheck(sqlDB))
	if err := kb.Recover(ctx); err != nil {
		return err
	}
	index := search.New(sqlDB, kb, store, search.Options{
		HTTPClient:  egressOf(cfg).Workspace.Client(search.EmbedTimeout),
		VectorCache: cfg.SearchVectorCache,
	})
	stopSearch := index.Start(ctx)
	defer stopSearch()

	cliAccounts, stopCLI, err := startCLIWorkers(ctx, cfg, sqlDB, key)
	if err != nil {
		return err
	}
	defer func() { stuck.note("the CLI workers' proxy", stopCLI()) }()
	browsers, purgeBrowsers, stopBrowsers := startBrowsers(cfg, cliAccounts)
	defer stopBrowsers()

	spaces := access.New(sqlDB, nil)
	boardSvc := boards.New(sqlDB, spaces)
	scheduler := schedule.New(sqlDB, spaces, schedule.Options{})
	if err := boardSvc.EnableReminders(scheduler); err != nil {
		return err
	}
	if err := boardSvc.ReconcileReminders(ctx); err != nil {
		return err
	}
	integrationStore := integrations.NewStore(sqlDB, key)
	if err := integrationStore.BackfillFingerprints(ctx); err != nil {
		return fmt.Errorf("fingerprinting the integrations' webhook secrets: %w", err)
	}
	integrationSvc := integrations.New(integrationStore, integrations.Options{PublicURL: cfg.PublicURL, Egress: integrationEgress(cfg)})
	appSvc := apps.New(sqlDB, spaces).WithPlatform(apps.Platform{Boards: boardSvc, Docs: kb, Payments: integrationSvc}).WithDeletedRetention(cfg.AppDeletedRetention())
	integrationSvc.UseRecords(appSvc)
	stopEffects := appSvc.StartOutbox(ctx)
	defer stopEffects()
	notifier, err := notifications.New(ctx, sqlDB, spaces, notifications.Options{Key: key, Contact: cfg.PushContact, Mail: mailer, PublicURL: cfg.PublicURL})
	if err != nil {
		return err
	}
	if err := notifier.AttachScheduler(scheduler); err != nil {
		return err
	}
	stopPush := notifier.Start(ctx)
	defer stopPush()
	deletions := accountdeletion.New(accountdeletion.Options{
		DB: sqlDB, DataDir: cfg.DataDir, Scheduler: scheduler, Knowledge: kb, CLI: cliAccounts, Browsers: purgeBrowsers,
	})
	if err := deletions.Attach(); err != nil {
		return err
	}
	stopSchedule := scheduler.Start(ctx)
	defer func() {
		if !stopSchedule() {
			stuck.note("the scheduler", fmt.Errorf("%w: a job's handler did not return", drain.ErrBusy))
		}
	}()
	tools := agenttools.New(agenttools.Options{
		DB: sqlDB, Key: key, Access: spaces, Boards: boardSvc, Apps: appSvc,
		Knowledge: kb, Search: index, Browser: browsers,
		Integrations: integrations.NewStore(sqlDB, key),
	})
	enableCLITools(cfg, cliAccounts, tools)

	var agent agentRuntime
	var convAgent conversations.Agent
	if cfg.Agent {
		agent, err = startAgent(ctx, cfg, store, toolsSocket(tools, cliAccounts), cliAccounts.Providers(), ssoSecretEnv)
		if err != nil {
			return err
		}
		defer func() { stuck.note("the agent runtime's tools socket", agent.stop()) }()
		deletions.SetProviders(agent.feed.Sync)
		deletions.SetAgent(func(ctx context.Context) (conversations.Purger, error) {
			c, err := agent.ready(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		})
		cliAccounts.SetModelSetter(func(ctx context.Context, conversation int64, cli cliworker.CLI) error {
			c, err := agent.ready(ctx)
			if err != nil {
				return err
			}
			return c.SetModel(ctx, conversation, agentrt.ModelRef{Provider: "cli-" + string(cli), ModelID: "default"})
		})
		convAgent = func(ctx context.Context) (conversations.Runtime, error) {
			c, err := agent.ready(ctx)
			if err != nil {
				return nil, err
			}
			return conversations.Client(c), nil
		}
	}
	convs := conversations.New(sqlDB, convAgent)
	liveHub := live.New(live.Config{})
	convs.SetCapabilities(tools.Capability)
	if cfg.Agent {
		index.SetConversations(convs)
	}
	convs.SetCLIQuestions(cliAccounts)

	if cfg.InsecureCookies {
		slog.Warn("the session cookie is sent over plain http (insecure_cookies): use this only on localhost")
	}
	authOpts := auth.Options{InsecureCookies: cfg.InsecureCookies, TrustedProxies: cfg.TrustedProxyPrefixes(), Key: key,
		PublicURL: cfg.PublicURL, DisablePasswordLogin: cfg.DisablePasswordLogin,
		WorkspaceCreation: cfg.WorkspaceCreationMode(), AccountDeletionGrace: time.Duration(cfg.AccountDeletionDays) * 24 * time.Hour}
	authOpts.SSO = ssoProviders
	if len(ssoProviders) > 0 {
		slog.Info("single sign-on is on", "providers", len(ssoProviders), "public_url", cfg.PublicURL,
			"password_login", !cfg.DisablePasswordLogin)
	}
	for _, p := range authOpts.TrustedProxies {
		if (p.Addr().Is4() && p.Bits() < 24) || (!p.Addr().Is4() && p.Bits() < 64) {
			slog.Warn("trusted_proxies includes a broad range: every client inside it can set its own address for the login limits; list only your reverse proxy's address",
				"range", p.String())
		}
	}
	if e := egressOf(cfg); !e.Workspace.IsAny() || !e.Personal.IsAny() {
		slog.Info("provider accounts connect only where their egress policy allows",
			"workspace_accounts", e.Workspace.String(), "personal_accounts", e.Personal.String())
	}
	needed, err := auth.SetupNeeded(ctx, sqlDB)
	if err != nil {
		return err
	}
	if needed {
		authOpts.SetupToken = auth.NewSetupToken()
		slog.Info("first-run setup: no account exists yet; set up the owner with this setup token (POST /api/setup, see docs/install.md); it works until setup is done or the server restarts",
			"setup_token", authOpts.SetupToken)
	}
	maintainCtx, stopMaintain := context.WithCancel(ctx)
	var maintaining sync.WaitGroup
	defer func() {
		stopMaintain()
		maintaining.Wait()
	}()
	stopIntegrationEvents := integrationSvc.StartEvents(ctx)
	defer stopIntegrationEvents()
	stopIntegrationPolling := integrationSvc.StartPolling(ctx)
	defer stopIntegrationPolling()
	var accounts *auth.Service
	handler, err := server.New(version, sqlDB, server.Options{
		AccountsReady: func(svc *auth.Service) {
			accounts = svc
			svc.SetDeletionScheduler(deletions)
		},
		Notifications:    notifier,
		Scheduler:        scheduler,
		Auth:             authOpts,
		DataDir:          cfg.DataDir,
		AgentState:       agent.state,
		Providers:        providerAPI(store, agent.feed, egressOf(cfg)),
		Search:           index,
		Conversations:    convs,
		Live:             liveHub,
		Tools:            tools,
		CLIAccounts:      cliAccounts,
		Browser:          browsers,
		Dictation:        dictationService(cfg, sqlDB, key, kb),
		Integrations:     integrationSvc,
		BackupDataDir:    cfg.DataDir,
		UpdateStatusDir:  cfg.UpdateStatusDir,
		UpdateRequestDir: cfg.UpdateRequestDir,
		Quota:            quotas,
		Mail:             mailer,
		AppsReady: func(svc *apps.Service) {
			deletions.SetApps(svc)
			maintaining.Add(1)
			go func() {
				defer maintaining.Done()
				svc.Maintain(maintainCtx, 10*time.Minute)
			}()
		},
	})
	if err != nil {
		return err
	}
	if err := deletions.Reconcile(ctx); err != nil {
		return err
	}
	maintaining.Add(1)
	go func() {
		defer maintaining.Done()
		deletions.Maintain(maintainCtx, 10*time.Minute)
	}()

	inflight := drain.New()
	srv := &http.Server{
		Handler:           inflight.Handler(wrapHandler(handler, sqlDB)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	srv.RegisterOnShutdown(convs.EndStreams)
	srv.RegisterOnShutdown(liveHub.EndStreams)
	ln, err := listen(ctx, cfg.Addr)
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	_, webBuilt := webui.FS()
	slog.Info("taskto listening", "addr", ln.Addr().String(), "version", version,
		"database", dbPath, "web_app_built", webBuilt, "agent", cfg.Agent, "cli_workers", cliAccounts.Mode(),
		"trusted_proxies", cfg.TrustedProxies)
	if listening != nil {
		listening(ln.Addr())
	}

	var listenErr error
	select {
	case listenErr = <-errc:
		slog.Error("the listener failed; shutting down", "err", listenErr)
	case <-ctx.Done():
	}
	err = inflight.Shutdown(srv, shutdownGrace, handlerWait)
	if listenErr == nil {
		if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) {
			listenErr = serveErr
		}
	}
	stuck.note("the server's requests", err)
	idleCtx, stopIdle := context.WithTimeout(context.Background(), emailWait)
	defer stopIdle()
	if err := accounts.Shutdown(idleCtx); err != nil {
		slog.Warn("shutting down with emails still being made")
		stuck.note("the emails being made", fmt.Errorf("%w: %w", drain.ErrBusy, err))
	}
	return listenErr
}

var (
	heldLocksMu sync.Mutex
	heldLocks   []*os.File
)

func keepLock(f *os.File) {
	heldLocksMu.Lock()
	defer heldLocksMu.Unlock()
	heldLocks = append(heldLocks, f)
}

var (
	shutdownGrace = defaultShutdownGrace
	wrapHandler   = func(h http.Handler, _ *sql.DB) http.Handler { return h }
	listen        = func(ctx context.Context, addr string) (net.Listener, error) {
		return new(net.ListenConfig).Listen(ctx, "tcp", addr)
	}
)

func egressOf(cfg config.Config) providers.Egress {
	ws, personal := cfg.Egress()
	return providers.Egress{Workspace: ws, Personal: personal}
}

func providerAPI(store *providers.Store, feed *providers.Feed, e providers.Egress) *providers.Service {
	s := providers.New(store, feed)
	s.SetEgress(e)
	return s
}

func dictationService(cfg config.Config, sqlDB *sql.DB, key *serverkey.Key, kb *knowledge.Store) *dictation.Service {
	c := dictation.Config{
		DB: sqlDB, Key: key, Docs: kb, Egress: egressOf(cfg).Workspace,
		Addons: dictation.DetectAddons(cfg.AddonsDirectory()), Scratch: cfg.DictationCacheDir(),
	}
	if model := cfg.LocalDictation(); model != "off" {
		local, err := dictation.NewLocal(model, cfg.DictationCacheDir())
		if err != nil {
			slog.Error("dictation: no local speech model", "err", err)
		} else {
			c.Local = local
			slog.Info("dictation: local speech model", "model", model, "state", local.State().State, "dir", cfg.DictationCacheDir())
		}
	}
	for _, a := range c.Addons {
		slog.Info("dictation: add-on installed", "addon", a.ID, "path", a.Path)
	}
	return dictation.New(c)
}

func integrationEgress(cfg config.Config) *egress.Policy {
	p := cfg.IntegrationEgressPolicy()
	return &p
}
