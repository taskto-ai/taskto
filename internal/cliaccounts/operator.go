package cliaccounts

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

type WorkerStatus struct {
	Mode              string `json:"mode"`
	Reachable         bool   `json:"reachable"`
	AuthorityLost     bool   `json:"authority_lost"`
	OtherInstallation bool   `json:"other_installation"`
	Boot              string `json:"boot,omitempty"`
	Installation      string `json:"installation,omitempty"`
}

func (s *Service) OperatorRoutes(mux *http.ServeMux, a *auth.Service) {
	mux.Handle("GET /api/workspaces/{workspace}/cli-worker", a.RequireOperator(http.HandlerFunc(s.handleWorkerStatus)))
}

func (s *Service) handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	out := WorkerStatus{Mode: string(s.mode)}
	if s.mode == Off || s.worker == nil {
		out.Mode = "off"
	} else if a, err := s.worker.Authority(r.Context()); err != nil {
		slog.WarnContext(r.Context(), "cliaccounts: asking the CLI worker for its authority", "err", err)
	} else {
		out.Reachable = true
		inst, err := s.Installation(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "cliaccounts: reading this server's installation", "err", err)
			apierr.Internal(w)
			return
		}
		out.AuthorityLost = a.Lost != ""
		out.OtherInstallation = a.Installation != "" && a.Installation != inst
		if out.AuthorityLost || out.OtherInstallation {
			out.Boot, out.Installation = s.boot, inst
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
