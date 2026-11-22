package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/backup"
)

// backupHealth is /healthz/backup, for outside monitoring: whether this server's
// scheduled backups are on time (docs/install.md#backups). It answers 200 while they are
// (ok; pending, before the first; failing, while a failed night's retries still have
// time) and 503 when a night was missed (late) or no schedule has run (none). It says
// when the last backup succeeded and when the next is due, never why one failed: the
// error can name the backup host, and this route needs no login.
func backupHealth(dataDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := struct {
			Status      backup.Health `json:"status"`
			LastSuccess time.Time     `json:"last_success,omitzero"`
			DueBy       time.Time     `json:"due_by,omitzero"`
		}{Status: backup.HealthNone}
		code := http.StatusServiceUnavailable
		st, ok, err := readBackupStatus(dataDir)
		switch {
		case err != nil:
			slog.Warn("healthz: the backup status is unreadable", "err", err)
			reply.Status = "unknown"
		case ok:
			reply.Status, reply.LastSuccess, reply.DueBy = st.Health(time.Now()), st.LastSuccess, st.DueBy
			if reply.Status != backup.HealthLate {
				code = http.StatusOK
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(reply)
	})
}

// backupStatus is GET /api/workspaces/{workspace}/backup, for the workspace's admins
// (Settings → About): the schedule's whole status, with its health. The server's backups
// are the whole server's, so any workspace's admins see them.
func backupStatus(dataDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, ok, err := readBackupStatus(dataDir)
		if err != nil {
			slog.Error("the backup status is unreadable", "err", err)
			apierr.Internal(w)
			return
		}
		reply := struct {
			Health backup.Health `json:"health"`
			*backup.Status
		}{Health: backup.HealthNone}
		if ok {
			reply.Health, reply.Status = st.Health(time.Now()), &st
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(reply)
	})
}

func readBackupStatus(dataDir string) (backup.Status, bool, error) {
	if dataDir == "" {
		return backup.Status{}, false, nil
	}
	return backup.ReadStatus(dataDir)
}
