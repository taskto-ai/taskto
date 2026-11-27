package cliaccounts

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"taskto.ai/taskto/internal/auth"
)

// WorkerStatus is GET /api/workspaces/{workspace}/cli-worker, for the server's operators
// (Settings → About): whether the CLI worker lost its authority, that is its history of server
// boots, so that it refuses every census and deletion until an operator hands it over to this
// server (docs/cli-workers.md). The worker is the whole server's, so only operators see it
// (auth.Service.RequireOperator).
type WorkerStatus struct {
	// Mode is the -cli-workers mode: off, local or container.
	Mode string `json:"mode"`
	// Reachable is false when the worker couldn't be asked.
	Reachable bool `json:"reachable"`
	// AuthorityLost: the worker refuses every census and deletion until an operator hands it over.
	AuthorityLost bool `json:"authority_lost"`
	// Boot is this server's boot when the worker lost its authority: what an operator hands it over
	// to (taskto cli-worker trust-server -boot).
	Boot string `json:"boot,omitempty"`
}

// OperatorRoutes registers the operators' route of the CLI worker's state.
func (s *Service) OperatorRoutes(mux *http.ServeMux, a *auth.Service) {
	mux.Handle("GET /api/workspaces/{workspace}/cli-worker", a.RequireOperator(http.HandlerFunc(s.handleWorkerStatus)))
}

func (s *Service) handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	out := WorkerStatus{Mode: string(s.mode)}
	if s.mode == Off || s.worker == nil {
		out.Mode = "off"
	} else if lost, err := s.worker.AuthorityLost(r.Context()); err != nil {
		slog.WarnContext(r.Context(), "cliaccounts: asking the CLI worker for its authority", "err", err)
	} else {
		out.Reachable = true
		if lost != "" {
			out.AuthorityLost, out.Boot = true, s.boot
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
