package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/updatestatus"
)

// updateStatus is GET /api/workspaces/{workspace}/updates, for the server's operators
// (Settings → About; routed through auth.Service.RequireOperator, like the backups: the updates
// are the whole server's, not a workspace's): the version this server runs, and what the host's
// updater last wrote (docs/install.md#automatic-updates). The updater is the server's host's,
// not the server's: this only reads the status file it leaves in a directory mounted
// read-only, which holds codes and versions only (internal/updatestatus).
//
// `configured` is whether the server was told where the status is, and `status` is set once the
// updater has written one; `stale` says it wasn't written for days; `expects_status` is set when
// the server is a release (or a commit's build) and the status is still missing, which only an
// updater of the host that predates it explains; `request` says whether "Update now" can be
// pressed. The status is never cached.
func updateStatus(version, statusDir, requestDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, err := updatestatus.BuildReport(version, statusDir, requestDir, time.Now())
		if err != nil {
			slog.Error("the update status is unreadable", "err", err)
			apierr.Internal(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(reply)
	})
}

// updateRequest is POST /api/workspaces/{workspace}/updates/request, "Update now", for the
// server's operators. It carries nothing: the host installs what the nightly run would take, on
// its configured channel, under every gate of that run. The server writes one file into the
// request directory (updatestatus.Request) and never runs anything; the host's path unit and
// updater do the rest (docs/install.md#update-now). One request waits at a time, none while a
// run is changing the install, and each is an audit event, committed before the file is made
// visible to the host, so a refused request records none and an update has always a record.
func updateRequest(version, statusDir, requestDir string, sqlDB *sql.DB, now func() time.Time) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		ws, err := strconv.ParseInt(r.PathValue("workspace"), 10, 64)
		if err != nil {
			apierr.Write(w, http.StatusNotFound, apierr.CodeWorkspaceNotFound, "workspace not found")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		t := now()
		rep, err := updatestatus.BuildReport(version, statusDir, requestDir, t)
		if err != nil {
			slog.Error("the update status is unreadable", "err", err)
			apierr.Internal(w)
			return
		}
		if !rep.Request.Available {
			switch rep.Request.Reason {
			case updatestatus.ReasonOff:
				apierr.Write(w, http.StatusConflict, apierr.CodeUpdateChannelOff, "the updater's channel is off")
			case updatestatus.ReasonPending:
				apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRequestPending, "an update is already requested")
			case updatestatus.ReasonRunning:
				apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRunning, "an update is running")
			default:
				apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRequestUnsupported, "this host can't take an update request")
			}
			return
		}
		// The request is written first but kept out of sight; the event is committed; only then
		// is the request made visible. A host that took a request always has its record, and a
		// request that couldn't be published leaves an event of one that came to nothing.
		prepared, err := updatestatus.Prepare(requestDir, t)
		switch {
		case errors.Is(err, updatestatus.ErrPending):
			apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRequestPending, "an update is already requested")
			return
		case errors.Is(err, updatestatus.ErrNoDir):
			apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRequestUnsupported, "this host can't take an update request")
			return
		case err != nil:
			slog.Error("update request: writing the request", "err", err)
			apierr.Internal(w)
			return
		}
		committed := false
		defer func() {
			if !committed {
				prepared.Discard()
			}
		}()
		tx, err := sqlDB.BeginTx(r.Context(), nil)
		if err != nil {
			slog.Error("update request: begin", "err", err)
			apierr.Internal(w)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if err := audit.Record(r.Context(), tx, audit.Event{Workspace: ws, Actor: u.ID, Action: audit.ServerUpdateRequested,
			TargetType: audit.TargetWorkspace, Target: ws, Details: audit.Details{"channel": rep.Status.Channel}}); err != nil {
			slog.Error("update request: audit", "err", err)
			apierr.Internal(w)
			return
		}
		if err := tx.Commit(); err != nil {
			slog.Error("update request: commit", "err", err)
			apierr.Internal(w)
			return
		}
		committed = true
		switch err := prepared.Publish(); {
		case errors.Is(err, updatestatus.ErrPending):
			apierr.Write(w, http.StatusConflict, apierr.CodeUpdateRequestPending, "an update is already requested")
			return
		case err != nil:
			slog.Error("update request: publishing the request", "err", err)
			apierr.Internal(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(updatestatus.Requested{RequestedAt: t.UTC()})
	})
}
