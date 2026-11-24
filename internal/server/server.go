// Package server is the HTTP side of the TaskTo.AI binary.
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
	"taskto.ai/taskto/internal/dictation"
	"taskto.ai/taskto/internal/export"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/mcpserver"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/schedule"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/webui"
)

// Options are the server's switches.
type Options struct {
	// WebUI is the built web app to serve; nil means the one embedded in the binary.
	WebUI fs.FS
	// Auth configures accounts and sessions.
	Auth auth.Options
	// AgentState reports the agent runtime's state for /healthz; nil means it is off.
	AgentState func() string
	// DataDir holds the spaces' files, the knowledge base's repositories among them.
	// Empty leaves the knowledge base's routes out.
	DataDir string
	// Knowledge sets the knowledge base's limits.
	Knowledge knowledge.Options
	// Providers serves the provider account API; nil leaves it out.
	Providers *providers.Service
	// Search serves knowledge base search. Its knowledge base then serves the knowledge
	// routes in place of one made from DataDir and Knowledge, so the index hears of every
	// change. nil leaves search out.
	Search *search.Index
	// Conversations serves the conversation API; nil leaves it out.
	Conversations *conversations.Service
	// Tools serves the MCP endpoint (mcpserver.Path) to access tokens; nil leaves it out.
	Tools *agenttools.Service
	// CLIAccounts serves the users' CLI accounts (/api/me/cli-accounts); nil leaves it out.
	CLIAccounts *cliaccounts.Service
	// Dictation serves speech to text for the chat box; nil leaves it out.
	Dictation *dictation.Service
	// Browser serves the agent browser (/api/me/browser and its live view); nil leaves it out.
	Browser *agentbrowser.Service
	// Notifications serves the notification inbox and Web Push subscriptions; nil leaves them out.
	Notifications *notifications.Service
	// Scheduler is the general scheduler, whose reminders' handlers the command registered (boards'
	// EnableReminders): the HTTP routes of cards schedule on it too, so a card dated on the web
	// keeps its reminders like one dated by the agent. nil makes none.
	Scheduler *schedule.Service
	// Mail is the server's outgoing email (docs/install.md#email); nil means the operator set up
	// none. The admins' routes for it are there either way, and say so.
	Mail *mail.Service
	// MailTestCooldown is how long an admin waits between test emails; zero is 30 seconds and
	// a negative duration is no wait. Tests shorten it.
	MailTestCooldown time.Duration
	// AppsReady, if set, is called with the apps service the server made, which keeps attachments
	// in DataDir (none if that is empty), so its caller can run its maintenance
	// (apps.Service.Maintain) for as long as it serves.
	AppsReady func(*apps.Service)
	// Export sets the limits of a person's export of their own data (docs/api.md#export-my-data);
	// the zero value is the defaults. Tests shorten them.
	Export export.Options
	// BackupDataDir is the data directory whose scheduled backups /healthz/backup and the
	// admins' backup route report (internal/backup.Status). Empty reports none.
	BackupDataDir string
}

// New returns the root HTTP handler, backed by the database sqlDB, which must be
// migrated.
func New(version string, sqlDB *sql.DB, opts Options) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthz(version, sqlDB, opts.AgentState))
	mux.Handle("GET /healthz/backup", backupHealth(opts.BackupDataDir))
	accounts := auth.New(sqlDB, opts.Auth)
	accounts.Routes(mux)
	mux.Handle("GET /api/workspaces/{workspace}/backup", accounts.RequireAdmin(backupStatus(opts.BackupDataDir)))
	mailRoutes(mux, accounts, opts.Mail, newMailTestGate(opts.MailTestCooldown, time.Now))
	checklist.New(sqlDB, checklist.Options{
		Email: opts.Mail.Configured,
		Backups: func() (bool, error) {
			_, ok, err := readBackupStatus(opts.BackupDataDir)
			return ok, err
		},
	}).Routes(mux, accounts)
	spaces := access.New(sqlDB, accounts)
	spaces.Routes(mux)
	boardSvc := boards.New(sqlDB, spaces)
	if opts.Scheduler != nil {
		// The handlers are the command's; this service only schedules, and follows spaces that are
		// archived or brought back, and people who are added to a space or a workspace (whose
		// reminders the schema dropped when they left).
		boardSvc.WithScheduler(opts.Scheduler)
		spaces.OnArchiveChange(boardSvc.SyncSpace)
		spaces.OnMemberAdded(boardSvc.SyncSpaceMember)
		accounts.OnMemberAdded(boardSvc.SyncWorkspaceMember)
	}
	boardSvc.Routes(mux)
	// The apps' routes list and retry their effects and check the targets they name; the
	// server's own worker, which does them, is the one the command starts (apps.StartOutbox).
	appsService := apps.New(sqlDB, spaces).WithFiles(opts.DataDir).WithPlatform(apps.Platform{Boards: boardSvc})
	appsService.Routes(mux)
	appsService.HomeRoutes(mux, accounts)
	appsService.TemplateRoutes(mux, accounts)
	if opts.AppsReady != nil {
		opts.AppsReady(appsService)
	}
	if opts.Notifications != nil {
		opts.Notifications.Routes(mux, accounts)
	}
	var kb *knowledge.Store
	if opts.Search != nil {
		kb = opts.Search.Knowledge()
		kb.Routes(mux, spaces)
		opts.Search.Routes(mux, accounts, spaces)
	} else if opts.DataDir != "" {
		kb = knowledge.New(opts.DataDir, opts.Knowledge)
		kb.Routes(mux, spaces)
	}
	// A person's export of their own data reads through the same knowledge base, boards, apps and
	// conversations the rest of the server uses, and writes nothing.
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
		opts.CLIAccounts.SwitchingRoutes(mux, accounts)
	}
	if opts.Dictation != nil {
		opts.Dictation.Routes(mux, accounts, spaces)
	}
	if opts.Browser != nil {
		opts.Browser.Routes(mux, accounts)
	}
	if opts.Tools != nil {
		mux.Handle(mcpserver.Path, mcpserver.New(version, accounts, opts.Tools))
	}

	// An unknown API path is a 404, not the web app's page.
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

	// Every state-changing /api request, of any route, goes through the CSRF checks, and every
	// /api request that names the account it is for (the offline code does, on every request:
	// downloads, revalidation, the queue) is refused for any other session.
	// The audit log's events carry the client's address, found as the login limits find it.
	return accounts.ClientAddr(auth.CSRF(expectAccount(spaces, mux), append(slices.Clone(knowledge.RawBodyRoutes), apps.RawBodyRoutes...)...)), nil
}

// expectAccount applies access.Checker.ExpectUser to every /api request, so a client that names
// the account it is for (X-Taskto-Expected-User) is refused with 409 session_account_changed
// for any other session on any route, whatever the route's own checks: nothing is read or
// written as the wrong person. Requests without the header, and everything outside /api, pass
// unchanged.
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

// healthz reports the version, whether the database answers, and the agent runtime's state.
// It answers 503 when the database doesn't, so a load balancer or container runtime can
// tell. The agent runtime doesn't change the code: the server works without it, and the
// supervisor restarts it on its own.
func healthz(version string, sqlDB *sql.DB, agentState func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, database, code := "ok", "ok", http.StatusOK
		if err := sqlDB.PingContext(ctx); err != nil {
			// The error can name paths on the server: it goes to the log, not the reply.
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
