package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

// auditRoutes mounts the admins' view of the audit log (docs/api.md#audit-log).
func auditRoutes(mux *http.ServeMux, accounts *auth.Service, sqlDB *sql.DB) {
	mux.Handle("GET /api/workspaces/{workspace}/audit", accounts.RequireAdmin(auditList(sqlDB)))
}

// auditList is GET /api/workspaces/{workspace}/audit: the events of the workspace the caller
// administers, newest first. The workspace comes from the membership RequireAdmin checked, never
// from the path or the query, and audit.ListWorkspace refuses to run without one: no filter can
// reach another workspace's events or those of no workspace.
func auditList(sqlDB *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, ok := auth.MembershipFrom(r.Context())
		if !ok {
			apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
			return
		}
		q, msg := auditQuery(r.URL.Query())
		if msg != "" {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, msg)
			return
		}
		page, err := audit.ListWorkspace(r.Context(), sqlDB, m.WorkspaceID, q)
		if errors.Is(err, audit.ErrBadCursor) {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "cursor isn't one this log gave out")
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				slog.ErrorContext(r.Context(), "audit: reading the log", "err", err)
			}
			apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(page)
	})
}

// auditQuery reads the filters of the list; msg says what is wrong with them, if anything.
func auditQuery(v url.Values) (q audit.Query, msg string) {
	q.Action, q.TargetType, q.Cursor = v.Get("action"), v.Get("target_type"), v.Get("cursor")
	if q.Action != "" && !audit.KnownAction(q.Action) {
		return q, "action isn't an action of the log"
	}
	if q.TargetType != "" && !audit.KnownTarget(q.TargetType) {
		return q, "target_type isn't a kind of object the log records"
	}
	if s := v.Get("actor"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return q, "actor isn't a user id"
		}
		q.Actor = n
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > audit.MaxPage {
			return q, "limit is a number from 1 to " + strconv.Itoa(audit.MaxPage)
		}
		q.Limit = n
	}
	var err error
	if q.From, err = optionalTime(v.Get("from")); err != nil {
		return q, "from isn't a time in RFC 3339"
	}
	if q.To, err = optionalTime(v.Get("to")); err != nil {
		return q, "to isn't a time in RFC 3339"
	}
	return q, ""
}

func optionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
