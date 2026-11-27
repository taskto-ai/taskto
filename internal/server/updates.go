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
		defer prepared.Discard()
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
