package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agentbrowser"
	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/checklist"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/desk"
	"taskto.ai/taskto/internal/dictation"
	"taskto.ai/taskto/internal/export"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/live"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/mcpserver"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/quota"
	"taskto.ai/taskto/internal/schedule"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/webui"
)

type Options struct {
	WebUI            fs.FS
	Auth             auth.Options
	AgentState       func() string
	DataDir          string
	Knowledge        knowledge.Options
	Providers        *providers.Service
	Search           *search.Index
	Conversations    *conversations.Service
	Live             *live.Hub
	Tools            *agenttools.Service
	CLIAccounts      *cliaccounts.Service
	Dictation        *dictation.Service
	Integrations     *integrations.Service
	Browser          *agentbrowser.Service
	Notifications    *notifications.Service
	Quota            *quota.Service
	Scheduler        *schedule.Service
	Mail             *mail.Service
	MailTestCooldown time.Duration
	AccountsReady    func(*auth.Service)
	AppsReady        func(*apps.Service)
	Export           export.Options
	BackupDataDir    string
	UpdateStatusDir  string
	UpdateRequestDir string
}

func New(version string, sqlDB *sql.DB, opts Options) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthz(version, sqlDB, opts.AgentState))
	mux.Handle("GET /healthz/backup", backupHealth(opts.BackupDataDir))
	opts.Auth.Mail = opts.Mail
	accounts := auth.New(sqlDB, opts.Auth)
	if opts.AccountsReady != nil {
		opts.AccountsReady(accounts)
	}
	accounts.Routes(mux)
	mux.Handle("GET /api/workspaces/{workspace}/backup", accounts.RequireOperator(backupStatus(opts.BackupDataDir)))
	mux.Handle("GET /api/workspaces/{workspace}/updates", accounts.RequireOperator(updateStatus(version, opts.UpdateStatusDir, opts.UpdateRequestDir)))
	mux.Handle("POST /api/workspaces/{workspace}/updates/request", accounts.RequireOperator(updateRequest(version, opts.UpdateStatusDir, opts.UpdateRequestDir, sqlDB, time.Now)))
	auditRoutes(mux, accounts, sqlDB)
	mailRoutes(mux, accounts, opts.Mail, newMailTestGate(opts.MailTestCooldown, time.Now))
	checklist.New(sqlDB, checklist.Options{
		Email: opts.Mail.Configured,
		Backups: func() (bool, error) {
			_, ok, err := readBackupStatus(opts.BackupDataDir)
			return ok, err
		},
	}).Routes(mux, accounts)
	desk.New(sqlDB).Routes(mux, accounts)
	spaces := access.New(sqlDB, accounts)
	spaces.Routes(mux)
	if opts.Live != nil {
		opts.Live.UseSessions(accounts.WatchSession)
		spaces.OnMemberRemoved(opts.Live.EndUserIn)
	}
	boardSvc := boards.New(sqlDB, spaces)
	if opts.Scheduler != nil {
		boardSvc.WithScheduler(opts.Scheduler)
		spaces.OnArchiveChange(boardSvc.SyncSpace)
		spaces.OnMemberAdded(boardSvc.SyncSpaceMember)
		accounts.OnMemberAdded(boardSvc.SyncWorkspaceMember)
		accounts.OnTimeZoneChanged(boardSvc.SyncTimeZone)
	}
	if opts.Notifications != nil {
		accounts.OnTimeZoneChanged(opts.Notifications.SyncTimeZone)
	}
	boardSvc.Routes(mux)
	var kb *knowledge.Store
	if opts.Search != nil {
		kb = opts.Search.Knowledge()
		kb.Routes(mux, spaces)
		opts.Search.Routes(mux, accounts, spaces)
	} else if opts.DataDir != "" {
		kb = knowledge.New(opts.DataDir, opts.Knowledge)
		kb.Routes(mux, spaces)
	}
	platform := apps.Platform{Boards: boardSvc, ProviderKinds: providerKinds}
	if opts.Integrations != nil {
		platform.Payments = opts.Integrations
	}
	appsService := apps.New(sqlDB, spaces).WithFiles(opts.DataDir).WithPlatform(platform)
	if opts.Scheduler != nil {
		appsService.WithScheduler(opts.Scheduler)
	}
	if kb != nil {
		appsService.WithSpaceHolder(kb)
	}
	if opts.Quota != nil {
		appsService.WithQuota(opts.Quota)
		opts.Quota.Routes(mux, accounts)
	}
	appsService.Routes(mux)
	appsService.ApprovalRoutes(mux, accounts)
	appsService.HomeRoutes(mux, accounts)
	appsService.TemplateRoutes(mux, accounts)
	if opts.AppsReady != nil {
		opts.AppsReady(appsService)
	}
	if opts.Notifications != nil {
		opts.Notifications.Routes(mux, accounts)
	}
	export.New(sqlDB, spaces, kb, boardSvc, appsService, opts.Conversations, opts.Export).Routes(mux, accounts)
	if opts.Providers != nil {
		opts.Providers.Routes(mux, accounts)
	}
	if opts.Conversations != nil {
		opts.Conversations.Routes(mux, accounts)
	}
	if opts.CLIAccounts != nil {
		opts.CLIAccounts.Routes(mux, accounts)
		opts.CLIAccounts.SharedRoutes(mux, accounts)
		opts.CLIAccounts.OperatorRoutes(mux, accounts)
		opts.CLIAccounts.SwitchingRoutes(mux, accounts)
	}
	if opts.Dictation != nil {
		opts.Dictation.Routes(mux, accounts, spaces)
	}
	if opts.Integrations != nil {
		opts.Integrations.Routes(mux, accounts, spaces)
		opts.Integrations.HookRoutes(mux)
	}
	if opts.Browser != nil {
		opts.Browser.Routes(mux, accounts)
	}
	if opts.Tools != nil {
		mux.Handle(mcpserver.Path, mcpserver.New(version, accounts, opts.Tools))
	}

	mux.Handle("/api/", http.NotFoundHandler())
	mux.Handle("/api", http.NotFoundHandler())

	webFS := opts.WebUI
	if webFS == nil {
		webFS, _ = webui.FS()
	}
	web, err := webui.Handler(webFS)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", web)

	return accounts.ClientAddr(auth.CSRF(expectAccount(spaces, mux), append(slices.Clone(knowledge.RawBodyRoutes), apps.RawBodyRoutes...)...)), nil
}

func expectAccount(spaces *access.Checker, next http.Handler) http.Handler {
	expect := spaces.ExpectUser(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) && r.Header.Get(access.ExpectedUserHeader) != "" {
			expect.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func healthz(version string, sqlDB *sql.DB, agentState func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, database, code := "ok", "ok", http.StatusOK
		if err := sqlDB.PingContext(ctx); err != nil {
			slog.Warn("healthz: database unreachable", "err", err)
			status, database, code = "unavailable", "unreachable", http.StatusServiceUnavailable
		}
		agent := "off"
		if agentState != nil {
			agent = agentState()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "version": version, "database": database, "agent": agent})
	})
}

func providerKinds(provider string) []string {
	if p := integrations.Lookup(provider); p != nil {
		return p.Kinds()
	}
	return nil
}
