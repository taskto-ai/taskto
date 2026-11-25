package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// Routes mounts the space endpoints on mux. docs/api.md describes them.
func (c *Checker) Routes(mux *http.ServeMux) {
	member := func(h http.HandlerFunc) http.Handler { return c.auth.RequireMember(h) }
	admin := func(h http.HandlerFunc) http.Handler { return c.auth.RequireAdmin(h) }

	// Access tokens may read the spaces; managing them takes a session.
	mux.Handle("GET /api/workspaces/{workspace}/spaces", auth.AcceptTokens(member(c.handleList)))
	mux.Handle("POST /api/workspaces/{workspace}/spaces", admin(c.handleCreate))
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}", auth.AcceptTokens(member(c.handleGet)))
	mux.Handle("PATCH /api/workspaces/{workspace}/spaces/{space}", admin(c.handleUpdate))
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/members", auth.AcceptTokens(member(c.handleMembers)))
	mux.Handle("POST /api/workspaces/{workspace}/spaces/{space}/members", admin(c.handleAddMember))
	mux.Handle("PATCH /api/workspaces/{workspace}/spaces/{space}/members/{user}", admin(c.handleSetMemberRole))
	mux.Handle("DELETE /api/workspaces/{workspace}/spaces/{space}/members/{user}", admin(c.handleRemoveMember))
}

const (
	maxBody    = 64 << 10
	maxNameLen = 100
)

// visibleSpace is spaceInPath, answering 404 unless the caller may read the space, or
// manage it: a shared space, for a workspace admin.
func (c *Checker) visibleSpace(w http.ResponseWriter, r *http.Request) (Space, bool) {
	s, ok := c.spaceInPath(w, r)
	if !ok {
		return Space{}, false
	}
	m, _ := auth.MembershipFrom(r.Context())
	if !s.Access.Allows(Read) && (s.Kind != Shared || m.Role != auth.RoleAdmin) {
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, ErrNotFound.Error())
		return Space{}, false
	}
	return s, true
}

// sharedSpace is visibleSpace for routes about shared spaces only: it answers 404 for
// private ones too.
func (c *Checker) sharedSpace(w http.ResponseWriter, r *http.Request) (Space, bool) {
	s, ok := c.visibleSpace(w, r)
	if !ok {
		return Space{}, false
	}
	if s.Kind != Shared {
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, "shared space not found")
		return Space{}, false
	}
	return s, true
}

func (c *Checker) handleList(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	all := false
	switch v := r.URL.Query().Get("all"); v {
	case "", "false":
	case "true":
		if m.Role != auth.RoleAdmin {
			writeError(w, http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can list every shared space")
			return
		}
		all = true
	default:
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `all must be "true" or "false"`)
		return
	}
	spaces, err := c.list(r.Context(), u.ID, m.WorkspaceID, all)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Space{"spaces": spaces})
}

func (c *Checker) handleCreate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, ok := spaceName(w, req.Name)
	if !ok {
		return
	}
	id, err := c.createShared(r.Context(), m.WorkspaceID, u.ID, name)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s, err := c.lookup(r.Context(), u.ID, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (c *Checker) handleGet(w http.ResponseWriter, r *http.Request) {
	if s, ok := c.visibleSpace(w, r); ok {
		writeJSON(w, http.StatusOK, s)
	}
}

func (c *Checker) handleUpdate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	s, ok := c.sharedSpace(w, r)
	if !ok {
		return
	}
	var req struct {
		Name             *string `json:"name"`
		Archived         *bool   `json:"archived"`
		OfflineDownloads *bool   `json:"offline_downloads"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == nil && req.Archived == nil && req.OfflineDownloads == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "name, archived or offline_downloads is required")
		return
	}
	if req.Name != nil {
		name, ok := spaceName(w, *req.Name)
		if !ok {
			return
		}
		req.Name = &name
	}
	if err := c.updateShared(r.Context(), s.WorkspaceID, s.ID, req.Name, req.Archived, req.OfflineDownloads); err != nil {
		c.fail(w, r, err)
		return
	}
	s, err := c.lookup(r.Context(), u.ID, s.ID)
	if err != nil {
		c.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (c *Checker) handleMembers(w http.ResponseWriter, r *http.Request) {
	s, ok := c.sharedSpace(w, r)
	if !ok {
		return
	}
	members, err := c.members(r.Context(), s.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Member{"members": members})
}

func (c *Checker) handleAddMember(w http.ResponseWriter, r *http.Request) {
	s, ok := c.sharedSpace(w, r)
	if !ok {
		return
	}
	var req struct {
		UserID int64 `json:"user_id"`
		Role   Role  `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validRole(w, req.Role) {
		return
	}
	if err := c.addMember(r.Context(), s.WorkspaceID, s.ID, req.UserID, req.Role); err != nil {
		c.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Checker) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	s, ok := c.sharedSpace(w, r)
	if !ok {
		return
	}
	userID, ok := pathUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Role Role `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validRole(w, req.Role) {
		return
	}
	if err := c.setMemberRole(r.Context(), s.WorkspaceID, s.ID, userID, req.Role); err != nil {
		c.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Checker) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	s, ok := c.sharedSpace(w, r)
	if !ok {
		return
	}
	userID, ok := pathUser(w, r)
	if !ok {
		return
	}
	if err := c.removeMember(r.Context(), s.WorkspaceID, s.ID, userID); err != nil {
		c.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail answers with the status for one of the package's errors, or 500.
func (c *Checker) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, ErrNotFound.Error())
	case errors.Is(err, errNotWorkspaceMember):
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotMember, err.Error())
	case errors.Is(err, errMemberNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeSpaceMemberNotFound, err.Error())
	case errors.Is(err, errAlreadyMember):
		writeError(w, http.StatusConflict, apierr.CodeSpaceAlreadyMember, err.Error())
	default:
		serverError(w, r, err)
	}
}

func pathUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeSpaceMemberNotFound, errMemberNotFound.Error())
		return 0, false
	}
	return id, true
}

func validRole(w http.ResponseWriter, role Role) bool {
	if !role.validMember() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `role must be "editor" or "viewer"`)
		return false
	}
	return true
}

// spaceName trims a space name and checks its length, answering 400 if it is wrong.
func spaceName(w http.ResponseWriter, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxNameLen {
		writeError(w, http.StatusBadRequest, apierr.CodeSpaceNameLength,
			fmt.Sprintf("a space name of 1 to %d characters is required", maxNameLen), apierr.Params{"max": maxNameLen})
		return "", false
	}
	return s, true
}

// readJSON decodes the request body into v, strictly: unknown fields and trailing data
// are errors. It answers 400 and returns false on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200
// with a broken body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("access: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string, params ...apierr.Params) {
	apierr.Write(w, status, code, msg, params...)
}
