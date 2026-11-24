package quota

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// Routes mounts the usage endpoints on mux (docs/api.md):
//
//	GET /api/workspaces/{workspace}/usage     admins: every space of the workspace
//	GET /api/workspaces/{workspace}/usage/me  members: their own private space
//
// Both take a session, not an access token. A member never sees another's usage, and an
// admin sees only their workspace's.
func (s *Service) Routes(mux *http.ServeMux, accounts *auth.Service) {
	mux.Handle("GET /api/workspaces/{workspace}/usage", accounts.RequireAdmin(http.HandlerFunc(s.handleUsage)))
	mux.Handle("GET /api/workspaces/{workspace}/usage/me", accounts.RequireMember(http.HandlerFunc(s.handleMine)))
}

func (s *Service) handleUsage(w http.ResponseWriter, r *http.Request) {
	m, _ := auth.MembershipFrom(r.Context())
	rep, err := s.Usage(r.Context(), m.WorkspaceID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, rep)
}

func (s *Service) handleMine(w http.ResponseWriter, r *http.Request) {
	m, _ := auth.MembershipFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	space, ok, err := s.Mine(r.Context(), m.WorkspaceID, u.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	reply := struct {
		Quota Limits      `json:"quota"`
		Space *SpaceUsage `json:"space"`
	}{Quota: s.limits}
	if ok {
		reply.Space = &space
	}
	writeJSON(w, reply)
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("quota: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("quota: encode response", "err", err)
		apierr.Internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(body, '\n'))
}
