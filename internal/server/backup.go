package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/backup"
)

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
