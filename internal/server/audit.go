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

func auditRoutes(mux *http.ServeMux, accounts *auth.Service, sqlDB *sql.DB) {
	mux.Handle("GET /api/workspaces/{workspace}/audit", accounts.RequireAdmin(auditList(sqlDB)))
}

func auditList(sqlDB *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, ok := auth.MembershipFrom(r.Context())
		if !ok {
			apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
			return
		}
		q, code, msg := auditQuery(r.URL.Query())
		if msg != "" {
			apierr.Write(w, http.StatusBadRequest, code, msg)
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

func auditQuery(v url.Values) (q audit.Query, code apierr.Code, msg string) {
	code = apierr.CodeBadRequest
	q.Action, q.TargetType, q.Cursor = v.Get("action"), v.Get("target_type"), v.Get("cursor")
	if q.Action != "" && !audit.KnownAction(q.Action) {
		return q, code, "action isn't an action of the log"
	}
	if q.TargetType != "" && !audit.KnownTarget(q.TargetType) {
		return q, code, "target_type isn't a kind of object the log records"
	}
	if s := v.Get("actor"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return q, code, "actor isn't a user id"
		}
		q.Actor = n
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > audit.MaxPage {
			return q, code, "limit is a number from 1 to " + strconv.Itoa(audit.MaxPage)
		}
		q.Limit = n
	}
	var err error
	if q.From, err = optionalTime(v.Get("from")); errors.Is(err, errTimeRange) {
		return q, apierr.CodeAuditTimeInvalid, "from is outside the years 1000 to 9999"
	} else if err != nil {
		return q, code, "from isn't a time in RFC 3339"
	}
	if q.To, err = optionalTime(v.Get("to")); errors.Is(err, errTimeRange) {
		return q, apierr.CodeAuditTimeInvalid, "to is outside the years 1000 to 9999"
	} else if err != nil {
		return q, code, "to isn't a time in RFC 3339"
	}
	return q, code, ""
}

var errTimeRange = errors.New("audit: a time bound outside the years 1000 to 9999")

func optionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	if y := t.UTC().Year(); y < 1000 || y > 9999 {
		return time.Time{}, errTimeRange
	}
	return t, nil
}
