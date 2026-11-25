package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/apierr"
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
// updater of the host that predates it explains. The status is never cached.
func updateStatus(version, dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, ok, err := updatestatus.Read(dir)
		if err != nil {
			slog.Error("the update status is unreadable", "err", err)
			apierr.Internal(w)
			return
		}
		reply := struct {
			Configured     bool                 `json:"configured"`
			RunningVersion string               `json:"running_version"`
			ExpectsStatus  bool                 `json:"expects_status,omitempty"`
			Stale          bool                 `json:"stale,omitempty"`
			Status         *updatestatus.Status `json:"status"`
		}{Configured: dir != "", RunningVersion: version}
		if ok {
			reply.Status, reply.Stale = &st, st.Stale(time.Now())
		} else {
			reply.ExpectsStatus = reply.Configured && updatestatus.IsVersion(version)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(reply)
	})
}
