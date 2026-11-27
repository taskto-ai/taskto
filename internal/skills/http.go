package skills

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

const maxRequestBody = 160 * 1024

type ownerFunc func(w http.ResponseWriter, r *http.Request) (owner Owner, manages bool, ok bool)

type createRequest struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     *bool  `json:"enabled"`
}

type patchRequest struct {
	Name        *string `json:"name"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Body        *string `json:"body"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Store) Routes(mux *http.ServeMux, accounts *auth.Service, spaces *access.Checker) {
	const (
		mine      = "/api/me/workspaces/{workspace}/skills"
		ofSpace   = "/api/workspaces/{workspace}/spaces/{space}/skills"
		workspace = "/api/workspaces/{workspace}/skills"
	)
	s.group(mux, mine, accounts.RequireMember, accounts.RequireMember, personalOwner)
	s.group(mux, ofSpace,
		func(next http.Handler) http.Handler { return spaces.RequireSpace(access.Read, next) },
		func(next http.Handler) http.Handler { return spaces.RequireSpace(access.Write, next) },
		spaceOwner)
	s.group(mux, workspace, accounts.RequireMember, accounts.RequireAdmin, workspaceOwner)
}

func (s *Store) group(mux *http.ServeMux, base string, reads, writes func(http.Handler) http.Handler, owner ownerFunc) {
	mux.Handle("GET "+base, reads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, manages, ok := owner(w, r)
		if !ok {
			return
		}
		list, err := s.List(r.Context(), o, manages)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SkillList{Skills: list})
	})))
	mux.Handle("POST "+base, writes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, _, ok := owner(w, r)
		if !ok {
			return
		}
		var req createRequest
		if !readJSON(w, r, &req) {
			return
		}
		created, err := s.Create(r.Context(), o, caller(r), Input{
			Name: req.Name, Title: req.Title, Description: req.Description, Body: req.Body, Enabled: req.Enabled,
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	})))
	mux.Handle("PATCH "+base+"/{skill}", writes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, _, ok := owner(w, r)
		if !ok {
			return
		}
		id := skillID(r)
		var req patchRequest
		if !readJSON(w, r, &req) {
			return
		}
		updated, err := s.Update(r.Context(), o, caller(r), id, Patch(req))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})))
	mux.Handle("DELETE "+base+"/{skill}", writes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, _, ok := owner(w, r)
		if !ok {
			return
		}
		id := skillID(r)
		if err := s.Delete(r.Context(), o, caller(r), id); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}

func caller(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

func personalOwner(w http.ResponseWriter, r *http.Request) (Owner, bool, bool) {
	m, _ := auth.MembershipFrom(r.Context())
	return Owner{Scope: ScopeUser, WorkspaceID: m.WorkspaceID, UserID: caller(r)}, true, true
}

func workspaceOwner(w http.ResponseWriter, r *http.Request) (Owner, bool, bool) {
	m, _ := auth.MembershipFrom(r.Context())
	return Owner{Scope: ScopeWorkspace, WorkspaceID: m.WorkspaceID}, m.Role == auth.RoleAdmin, true
}

func spaceOwner(w http.ResponseWriter, r *http.Request) (Owner, bool, bool) {
	sp, ok := access.SpaceFrom(r.Context())
	if !ok || sp.Kind != access.Shared {
		apierr.Write(w, http.StatusNotFound, apierr.CodeSpaceNotFound, access.ErrNotFound.Error())
		return Owner{}, false, false
	}
	return Owner{Scope: ScopeSpace, WorkspaceID: sp.WorkspaceID, SpaceID: sp.ID, SpaceGeneration: sp.Generation}, sp.Access.Allows(access.Write), true
}

func skillID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("skill"), 10, 64)
	return id
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("skills: encode response", "err", err)
		apierr.Internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var api *apierr.Error
	switch {
	case errors.As(err, &api):
		apierr.WriteError(w, api)
	case errors.Is(err, access.ErrNotFound):
		apierr.Write(w, http.StatusNotFound, apierr.CodeSpaceNotFound, access.ErrNotFound.Error())
	default:
		if r.Context().Err() != nil {
			return
		}
		slog.Error("skills: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apierr.Internal(w)
	}
}
