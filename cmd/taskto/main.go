// Command taskto is the single taskto.ai binary.
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

	// The time zone database, in the binary: a person's time zone (internal/auth, timezone.go) is
	// found whatever the host or the image has. The standard library's, not a dependency.
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

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// The CLI workers' helper: starts one CLI for one user (internal/cliworker).
	if len(os.Args) > 1 && os.Args[1] == cliworker.HelperArg {
		os.Exit(cliworker.RunHelper())
	}
	// The CLIs' permission hook: asks the worker whether a tool may run (docs/agent-safety.md).
	if len(os.Args) > 1 && os.Args[1] == cliworker.ApproveArg {
		os.Exit(cliworker.RunApprove(os.Args[2:]))
	}
	// The CLI worker of container mode, in a container of its own (docs/cli-workers.md).
	if len(os.Args) > 1 && os.Args[1] == "cli-worker" {
		if err := runCLIWorker(os.Args[2:], os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "taskto cli-worker:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "backup" || os.Args[1] == "backup-schedule" || os.Args[1] == "restore" || os.Args[1] == "conversations") {
		os.Exit(subcommand(os.Args[1], os.Args[2:]))
	}

	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		// The flag package has already printed the usage for a bad flag.
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
		// Exits at once: a data directory lock that run kept for work still running goes with
		// the process, and nothing else is waited for here.
		os.Exit(1)
	}
}

// subcommand runs `taskto backup`, `taskto backup-schedule`, `taskto restore` or `taskto
// conversations` and returns the exit code.
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

// agentRuntime is a running agent runtime. The zero value is the agent runtime off.
type agentRuntime struct {
	state func() string                                  // for /healthz
	ready func(context.Context) (*agentrt.Client, error) // waits until the sidecar answers
	feed  *providers.Feed                                // keeps its providers in step with the accounts
	stop  func() error                                   // stops the runtime; the error says a tools handler was still running
}

// run serves until ctx is done. If listening isn't nil, it is called with the
// address once the server accepts connections.
func run(ctx context.Context, cfg config.Config, listening func(net.Addr)) (result error) {
	// What could not be stopped cleanly is told to stuck by the deferred stops below, and
	// returned once they are all done (this defer runs last), after the database's close.
	stuck := &shutdownReport{}
	defer func() { result = errors.Join(result, stuck.err()) }()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	// The agent sidecar and local CLI workers run as this server's own user, so it must not
	// be able to act as root (docs/agent-safety.md): checked before anything starts.
	if cfg.Agent || cfg.CLIWorkers == "local" || cfg.CLIWorkers == "local-unsafe" {
		what := "this server, whose user the agent sidecar and local CLI workers run as,"
		if err := privcheck.Enforce(what, privcheck.Process{}.Problems(ctx), cfg.UnsafePrivilegedAgents); err != nil {
			return err
		}
	}
	// The single sign-on providers' client secrets are read, and their environment variables
	// removed, before anything starts a child process: the agent sidecar, the CLI workers and
	// the browsers must not inherit them. A mistake in the file stops the server here; the
	// providers themselves are asked on first use, so one that is down doesn't.
	ssoProviders, ssoSecretEnv, err := loadSSOProviders(cfg.SSOProviders, os.Getenv, os.Unsetenv)
	if err != nil {
		return err
	}
	// The outgoing email's password is read the same way, and the variables that name the SMTP
	// account are removed from the environment before any child starts.
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
	// One server per data directory, enforced before anything in it is opened. Deferred
	// first, so the lock goes last, after the sidecar has stopped and the database closed.
	lock, err := lockfile.DataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if stuck.busy {
			// Work that uses the data directory is still running, and the database is left open for
			// it: whoever takes the lock now (a second server, `taskto restore`) would write beside
			// it. The lock goes with the process, which is exiting.
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

	// Provider API keys are sealed with the server key; it is created on first run.
	key, err := serverkey.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	store := providers.NewStore(sqlDB, key)

	// The audit log's old events are removed in the background, for as long as the server runs;
	// it stops before the database closes.
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

	// The search index catches up with the knowledge base in the background; it stops
	// before the database closes.
	kb := knowledge.New(cfg.DataDir, knowledge.Options{
		MaxFileSize: cfg.KBMaxFileSize, LargeFileSize: cfg.KBLargeFileSize,
		MaxRepoSize: cfg.KBMaxRepoSize, MaxUnversionedSize: cfg.KBMaxUnversionedSize,
	})
	// The storage quotas: writes that would take a space or its workspace over one are refused.
	// With none set (the default) the check does nothing.
	quotas := quota.New(sqlDB, quota.Limits{Workspace: cfg.StorageQuotaWorkspace, User: cfg.StorageQuotaUser}, kb)
	kb.SetQuota(quotas.KnowledgeCheck())
	// A history rewrite a stopped process left half done is finished before anyone reads.
	if err := kb.Recover(ctx); err != nil {
		return err
	}
	// Embeddings are made with workspaces' accounts, under their egress policy.
	index := search.New(sqlDB, kb, store, search.Options{
		HTTPClient:  egressOf(cfg).Workspace.Client(search.EmbedTimeout),
		VectorCache: cfg.SearchVectorCache,
	})
	stopSearch := index.Start(ctx)
	defer stopSearch()

	// CLI workers: the users' own Claude Code, Codex and Grok Build logins. Deferred before
	// the agent runtime, so their turns stop after it.
	cliAccounts, stopCLI, err := startCLIWorkers(ctx, cfg, sqlDB, key)
	if err != nil {
		return err
	}
	defer func() { stuck.note("the CLI workers' proxy", stopCLI()) }()
	// The agent browser, in the CLI worker (or with it, in local mode).
	browsers, stopBrowsers := startBrowsers(cfg, cliAccounts)
	defer stopBrowsers()

	// The knowledge-base, board, app and browser tools, for the sidecar and the MCP endpoint:
	// they act for each conversation's owner, or each access token's user, with that user's
	// access.
	spaces := access.New(sqlDB, nil)
	boardSvc := boards.New(sqlDB, spaces)
	// The scheduler: timed work that runs for people, later (package schedule). The features that
	// use it register their handlers here, before anything that changes cards runs and before it
	// starts; it stops before the database closes. Cards' due-date reminders and overdue notices
	// are scheduled by every path that changes a card: the tools, the apps' effects (below) and the
	// HTTP routes (server.Options.Scheduler); the cards dated before there were reminders get theirs.
	scheduler := schedule.New(sqlDB, spaces, schedule.Options{})
	if err := boardSvc.EnableReminders(scheduler); err != nil {
		return err
	}
	if err := boardSvc.ReconcileReminders(ctx); err != nil {
		return err
	}
	appSvc := apps.New(sqlDB, spaces).WithPlatform(apps.Platform{Boards: boardSvc, Docs: kb}).WithDeletedRetention(cfg.AppDeletedRetention())
	// The apps' effects (cards, documents) are done in the background, as the users who acted,
	// so they go on when nobody has the app open. Stopped before the database closes.
	stopEffects := appSvc.StartOutbox(ctx)
	defer stopEffects()
	// The notification inbox, and Web Push for the people who turn it on: the VAPID key pair is made
	// on first start, and the worker that sends pushes runs until the server stops.
	notifier, err := notifications.New(ctx, sqlDB, spaces, notifications.Options{Key: key, Contact: cfg.PushContact, Mail: mailer, PublicURL: cfg.PublicURL})
	if err != nil {
		return err
	}
	// The daily email summary is a scheduled job: its handler is registered before the scheduler starts.
	if err := notifier.AttachScheduler(scheduler); err != nil {
		return err
	}
	stopPush := notifier.Start(ctx)
	defer stopPush()
	// The purge of a deleted account, after its grace period: a job of the scheduler too, so its
	// handler is registered before the scheduler starts.
	deletions := accountdeletion.New(accountdeletion.Options{
		DB: sqlDB, DataDir: cfg.DataDir, Scheduler: scheduler, Knowledge: kb, CLI: cliAccounts,
	})
	if err := deletions.Attach(); err != nil {
		return err
	}
	stopSchedule := scheduler.Start(ctx)
	defer func() {
		if !stopSchedule() {
			// Its handler has to be told apart from a handler of a listener, which has its own error.
			stuck.note("the scheduler", fmt.Errorf("%w: a job's handler did not return", drain.ErrBusy))
		}
	}()
	tools := agenttools.New(agenttools.Options{
		DB: sqlDB, Key: key, Access: spaces, Boards: boardSvc, Apps: appSvc,
		Knowledge: kb, Search: index, Browser: browsers,
	})
	// The same tools for the CLIs of conversations' turns, on the per-turn proxy's listener.
	enableCLITools(cfg, cliAccounts, tools)

	// Deferred after the database's Close, so the sidecar stops first.
	var agent agentRuntime
	var convAgent conversations.Agent // nil: the conversation API answers 503
	if cfg.Agent {
		agent, err = startAgent(ctx, cfg, store, toolsSocket(tools, cliAccounts), cliAccounts.Providers(), ssoSecretEnv)
		if err != nil {
			return err
		}
		defer func() { stuck.note("the agent runtime's tools socket", agent.stop()) }()
		// A purged account's conversations are retired and purged in the agent runtime too, and its
		// providers file and keys are rewritten.
		deletions.SetProviders(agent.feed.Sync)
		deletions.SetAgent(func(ctx context.Context) (conversations.Purger, error) {
			c, err := agent.ready(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		})
		// A turn that switches to another CLI shows it in the conversation's model.
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
	convs.SetCapabilities(tools.Capability)
	// The search index reads each conversation's text from the agent runtime, through them: with
	// no agent runtime there is none to read, and no conversation is searched.
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
		// Anyone inside a trusted range can claim any address in X-Forwarded-For, and so
		// dodge the per-address login limits: trust the proxy, not a whole network.
		if (p.Addr().Is4() && p.Bits() < 24) || (!p.Addr().Is4() && p.Bits() < 64) {
			slog.Warn("trusted_proxies includes a broad range: every client inside it can set its own address for the login limits; list only your reverse proxy's address",
				"range", p.String())
		}
	}
	if e := egressOf(cfg); !e.Workspace.IsAny() || !e.Personal.IsAny() {
		slog.Info("provider accounts connect only where their egress policy allows",
			"workspace_accounts", e.Workspace.String(), "personal_accounts", e.Personal.String())
	}
	// Until someone sets the server up, setup needs a token only its operator can read,
	// here in the log. Each start makes a new one, and it stops working once setup is done.
	needed, err := auth.SetupNeeded(ctx, sqlDB)
	if err != nil {
		return err
	}
	if needed {
		authOpts.SetupToken = auth.NewSetupToken()
		slog.Info("first-run setup: no account exists yet; set up the owner with this setup token (POST /api/setup, see docs/install.md); it works until setup is done or the server restarts",
			"setup_token", authOpts.SetupToken)
	}
	// The apps' attachment sweep runs in the background, for as long as the server runs. It is
	// waited for before the database closes: left running, its query's connection closes after
	// run returned, and that last close checkpoints the database while its file is still locked.
	maintainCtx, stopMaintain := context.WithCancel(ctx)
	var maintaining sync.WaitGroup
	defer func() {
		stopMaintain()
		maintaining.Wait()
	}()
	var accounts *auth.Service
	handler, err := server.New(version, sqlDB, server.Options{
		AccountsReady: func(svc *auth.Service) {
			accounts = svc
			svc.SetDeletionScheduler(deletions)
		},
		Notifications: notifier,
		Scheduler:     scheduler,
		Auth:          authOpts,
		DataDir:       cfg.DataDir,
		AgentState:    agent.state,
		Providers:     providerAPI(store, agent.feed, egressOf(cfg)),
		Search:        index,
		Conversations: convs,
		Tools:         tools,
		CLIAccounts:   cliAccounts,
		Browser:       browsers,
		Dictation:     dictationService(cfg, sqlDB, key, kb),
		Integrations:  integrations.New(integrations.NewStore(sqlDB, key), integrations.Options{PublicURL: cfg.PublicURL, Egress: integrationEgress(cfg)}),
		BackupDataDir: cfg.DataDir,
		// The host updater's status, read-only (docs/install.md#automatic-updates).
		UpdateStatusDir: cfg.UpdateStatusDir,
		// Update now's request, for the same updater to take; the one place of its the server writes.
		UpdateRequestDir: cfg.UpdateRequestDir,
		Quota:            quotas,
		Mail:             mailer,
		// Photos that were never saved in a record are removed after a day, and an app moved to
		// another space has its files follow it (internal/apps/attachments.go).
		AppsReady: func(svc *apps.Service) {
			// A purged account's photos in a moved app are finished, and its uploads removed.
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
	// Every deleted account has its purge scheduled: one made while no scheduler was set, or whose
	// job was lost, gets it now.
	if err := deletions.Reconcile(ctx); err != nil {
		return err
	}
	// The steps of purges that couldn't be done then (the agent runtime or the CLI workers were
	// off) are done in the background, at once and then every ten minutes, as is any purge that ran
	// out of tries: the server's start never waits for the agent runtime.
	maintaining.Add(1)
	go func() {
		defer maintaining.Done()
		deletions.Maintain(maintainCtx, 10*time.Minute)
	}()

	// No server-wide ReadTimeout or WriteTimeout: agent replies stream for minutes and
	// knowledge-base uploads can be large, so those limits belong on individual handlers.
	inflight := drain.New()
	srv := &http.Server{
		Handler:           inflight.Handler(wrapHandler(handler, sqlDB)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Event streams last until their client leaves: end them, so they don't hold up a shutdown.
	srv.RegisterOnShutdown(convs.EndStreams)
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

	// Whether a signal ends the server or its listener fails, the same shutdown follows: every
	// handler is done, or given up on and reported, before the deferred stops run and the
	// database closes. A request still running then would find it closed.
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
	// The emails the server accepted are made after its answer: end their sends and wait for
	// them, with a deadline of its own so a slow shutdown above doesn't leave none, and
	// whether or not that went well. The database closes (the deferred Close) only after this,
	// and not at all if they are still running, as with a request.
	idleCtx, stopIdle := context.WithTimeout(context.Background(), emailWait)
	defer stopIdle()
	if err := accounts.Shutdown(idleCtx); err != nil {
		slog.Warn("shutting down with emails still being made")
		stuck.note("the emails being made", fmt.Errorf("%w: %w", drain.ErrBusy, err))
	}
	// The listener's own error is the result, ahead of whatever the shutdown adds to it.
	return listenErr
}

// heldLocks are the data-directory locks run kept open when it returned with work still running.
// They are referenced from here so the garbage collector never closes one (an *os.File closes
// itself when it is collected); the process's exit releases them.
var (
	heldLocksMu sync.Mutex
	heldLocks   []*os.File
)

func keepLock(f *os.File) {
	heldLocksMu.Lock()
	defer heldLocksMu.Unlock()
	heldLocks = append(heldLocks, f)
}

// shutdownGrace is how long a shutdown lets requests finish before it ends their contexts, and
// wrapHandler lets a test put a handler of its own beside the server's. Both are for tests.
var (
	shutdownGrace = defaultShutdownGrace
	wrapHandler   = func(h http.Handler, _ *sql.DB) http.Handler { return h }
	listen        = func(ctx context.Context, addr string) (net.Listener, error) {
		return new(net.ListenConfig).Listen(ctx, "tcp", addr)
	}
)

// egressOf is the provider accounts' egress policies of cfg.
func egressOf(cfg config.Config) providers.Egress {
	ws, personal := cfg.Egress()
	return providers.Egress{Workspace: ws, Personal: personal}
}

// providerAPI is the provider account API, checking saves and tests against e.
func providerAPI(store *providers.Store, feed *providers.Feed, e providers.Egress) *providers.Service {
	s := providers.New(store, feed)
	s.SetEgress(e)
	return s
}

// dictationService is dictation with the operator's local model and add-ons. The local
// model's files aren't fetched here: the first dictation that needs them starts that.
func dictationService(cfg config.Config, sqlDB *sql.DB, key *serverkey.Key, kb *knowledge.Store) *dictation.Service {
	c := dictation.Config{
		DB: sqlDB, Key: key, Docs: kb, Egress: egressOf(cfg).Workspace,
		Addons: dictation.DetectAddons(cfg.AddonsDirectory()), Scratch: cfg.DictationCacheDir(),
	}
	if model := cfg.LocalDictation(); model != "off" {
		local, err := dictation.NewLocal(model, cfg.DictationCacheDir())
		if err != nil {
			slog.Error("dictation: no local speech model", "err", err) // config.Load checks the name
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

// integrationEgress is the policy of calls to integrations' providers: what -integration-egress
// says, which is "public" when it isn't set.
func integrationEgress(cfg config.Config) *egress.Policy {
	p := cfg.IntegrationEgressPolicy()
	return &p
}
