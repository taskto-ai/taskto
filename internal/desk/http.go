package desk

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

type DeskChanged struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Current Desk   `json:"current"`
}

func (s *Service) Routes(mux *http.ServeMux, accounts *auth.Service) {
	const base = "/api/workspaces/{workspace}/desks"
	member := func(h http.HandlerFunc) http.Handler { return accounts.RequireMember(h) }
	mux.Handle("GET "+base, member(s.handleList))
	mux.Handle("GET "+base+"/{desk}", member(s.handleGet))
	mux.Handle("PUT "+base+"/{desk}/layout", member(s.handleSave))
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	user, workspace := deskCaller(r)
	list, err := s.List(r.Context(), workspace, user)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseDesk(w, r)
	if !ok {
		return
	}
	user, workspace := deskCaller(r)
	d, err := s.Get(r.Context(), workspace, user, id)
	if errors.Is(err, ErrNotFound) {
		apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "not found")
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Service) handleSave(w http.ResponseWriter, r *http.Request) {
	id, ok := parseDesk(w, r)
	if !ok {
		return
	}
	var body LayoutWrite
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLayoutBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeRequestTooLarge, "request is too large")
			return
		}
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return
	}
	user, workspace := deskCaller(r)
	d, err := s.SaveLayout(r.Context(), workspace, user, id, body.Version, body.Layout)
	var bad *layoutInvalid
	switch {
	case errors.As(err, &bad):
		apierr.Write(w, http.StatusBadRequest, apierr.CodeDeskInvalid, "invalid desk layout", apierr.Params{"reason": bad.reason})
	case errors.Is(err, ErrChanged):
		writeJSON(w, http.StatusConflict, DeskChanged{Error: "desk changed", Code: string(apierr.CodeDeskChanged), Current: d})
	case errors.Is(err, ErrNotFound):
		apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "not found")
	case err != nil:
		fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

func parseDesk(w http.ResponseWriter, r *http.Request) (int64, bool) {
	value := r.PathValue("desk")
	if value == "main" {
		return 0, true
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "not found")
		return 0, false
	}
	return id, true
}

func deskCaller(r *http.Request) (user, workspace int64) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	return u.ID, m.WorkspaceID
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		slog.Error("desk: encode response", "err", err)
		apierr.Internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		slog.Error("desk: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	apierr.Internal(w)
}
