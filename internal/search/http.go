package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/providers"
)

// Routes mounts the search endpoints on mux: a space's search behind c.RequireSpace
// (Read), the workspace's behind a.RequireMember over the caller's Readable spaces, the
// embeddings setting, which only admins change, and a space's consent to it, which those
// who can read the space see, and its owner or editors change. docs/api.md describes them.
func (x *Index) Routes(mux *http.ServeMux, a *auth.Service, c *access.Checker) {
	// Searching takes access tokens; the settings and the consent take a session.
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/search", auth.AcceptTokens(c.RequireSpace(access.Read, http.HandlerFunc(x.handleSpace))))
	mux.Handle("GET /api/workspaces/{workspace}/search", auth.AcceptTokens(a.RequireMember(x.handleWorkspace(c))))
	mux.Handle("GET /api/workspaces/{workspace}/search/settings", a.RequireMember(http.HandlerFunc(x.handleSettings)))
	mux.Handle("PUT /api/workspaces/{workspace}/search/settings", a.RequireAdmin(http.HandlerFunc(x.handleSetSettings)))
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/search/embedding", c.RequireSpace(access.Read, http.HandlerFunc(x.handleConsent)))
	mux.Handle("PUT /api/workspaces/{workspace}/spaces/{space}/search/embedding", c.RequireSpace(access.Read, http.HandlerFunc(x.handleSetConsent)))
}

func (x *Index) handleSpace(w http.ResponseWriter, r *http.Request) {
	sp, _ := access.SpaceFrom(r.Context())
	x.search(w, r, sp.WorkspaceID, []access.Space{sp})
}

func (x *Index) handleWorkspace(c *access.Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		m, _ := auth.MembershipFrom(r.Context())
		spaces, err := c.Readable(r.Context(), u.ID, m.WorkspaceID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		x.search(w, r, m.WorkspaceID, spaces)
	}
}

func (x *Index) search(w http.ResponseWriter, r *http.Request, workspaceID int64, spaces []access.Space) {
	q := Query{Text: r.URL.Query().Get("q")}
	var ok bool
	if q.Limit, ok = queryInt(w, r, "limit"); !ok {
		return
	}
	if q.Offset, ok = queryInt(w, r, "offset"); !ok {
		return
	}
	res, err := x.Search(r.Context(), workspaceID, spaces, q)
	if errors.Is(err, ErrInvalid) {
		code, params := apierr.CodeBadRequest, apierr.Params(nil)
		if e := (*invalidError)(nil); errors.As(err, &e) {
			code, params = e.code, e.params
		}
		apierr.Write(w, http.StatusBadRequest, code, err.Error(), params)
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func queryInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, name+" must be a number, 0 or more")
		return 0, false
	}
	return n, true
}

// settingsReply is a workspace's search settings, and how far its embeddings are.
type settingsReply struct {
	// Embeddings is the embeddings model, or null for full-text search only.
	Embeddings *Embeddings `json:"embeddings"`
	// Documents are the workspace's indexed files; Pending, its Markdown documents still
	// waiting for their vectors (0 without an embeddings model).
	Documents int `json:"documents"`
	Pending   int `json:"pending"`
}

func (x *Index) settings(ctx context.Context, workspaceID int64) (settingsReply, error) {
	var reply settingsReply
	e, _, err := x.embeddings(ctx, workspaceID)
	switch {
	case err == nil:
		reply.Embeddings = &e
	case !errors.Is(err, errNoEmbeddings):
		return settingsReply{}, err
	}
	err = x.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM search_docs d JOIN spaces sp ON sp.id = d.space_id WHERE sp.workspace_id = ?),
			(SELECT count(*) `+pendingSQL+` AND sp.workspace_id = ?)`, workspaceID, workspaceID).
		Scan(&reply.Documents, &reply.Pending)
	return reply, err
}

func (x *Index) handleSettings(w http.ResponseWriter, r *http.Request) {
	m, _ := auth.MembershipFrom(r.Context())
	reply, err := x.settings(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// probeTimeout bounds the call that checks a new embeddings model answers.
const probeTimeout = 30 * time.Second

func (x *Index) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	var req struct {
		Embeddings *struct {
			Account int64  `json:"account"`
			Model   string `json:"model"`
		} `json:"embeddings"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var e *Embeddings
	if req.Embeddings != nil {
		model := strings.TrimSpace(req.Embeddings.Model)
		if model == "" || len(model) > maxModelLen || strings.ContainsFunc(model, unicode.IsControl) {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeEmbeddingsModelInvalid,
				fmt.Sprintf("embeddings.model must be 1 to %d printable characters", maxModelLen), apierr.Params{"max": maxModelLen})
			return
		}
		ep, err := x.providers.WorkspaceEndpoint(r.Context(), m.WorkspaceID, req.Embeddings.Account)
		if errors.Is(err, providers.ErrAccountNotFound) {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeEmbeddingsAccountInvalid, "embeddings.account must be one of this workspace's provider accounts")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		// A model that doesn't answer now would leave every document failing: say so here.
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		_, err = x.embed(ctx, ep, model, []string{"TaskTo.AI"})
		cancel()
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			slog.Warn("search: an embeddings model was refused", "provider", ep, "err", err)
			if refused := egress.Refusal(err); refused != nil {
				apierr.WriteError(w, refused.APIError(""))
				return
			}
			apierr.Write(w, http.StatusBadRequest, apierr.CodeEmbeddingsFailed, "the account's endpoint didn't return embeddings for this model")
			return
		}
		e = &Embeddings{Account: req.Embeddings.Account, Provider: ep.Provider, Model: model}
	}
	if err := x.setEmbeddings(r.Context(), m.WorkspaceID, u.ID, e); err != nil {
		serverError(w, r, err)
		return
	}
	reply, err := x.settings(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (x *Index) handleConsent(w http.ResponseWriter, r *http.Request) {
	sp, _ := access.SpaceFrom(r.Context())
	c, err := x.consent(r.Context(), sp.WorkspaceID, sp.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (x *Index) handleSetConsent(w http.ResponseWriter, r *http.Request) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	// The space's owner or one of its editors, archived or not: consent is about where its
	// text goes, not its content. Viewers, and admins who aren't editors, can't.
	if sp.Role != access.RoleOwner && sp.Role != access.RoleEditor {
		apierr.Write(w, http.StatusForbidden, apierr.CodeConsentForbidden, "only the space's owner or one of its editors can change this")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
		// The endpoint the person was shown, as GET answered it: consent is then to that
		// one only, not to whatever an admin set in between.
		Endpoint *ConsentEndpoint `json:"endpoint"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "enabled is required")
		return
	}
	err := x.setConsent(r.Context(), sp.WorkspaceID, sp.ID, u.ID, *req.Enabled, req.Endpoint)
	if errors.Is(err, errNoConsentEndpoint) {
		apierr.Write(w, http.StatusConflict, apierr.CodeConsentNoModel, "this workspace has no embeddings model to consent to")
		return
	}
	if errors.Is(err, errConsentEndpointChanged) {
		apierr.Write(w, http.StatusConflict, apierr.CodeConsentEndpointChanged, "the workspace's embeddings endpoint has changed: review it, then consent again")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	c, err := x.consent(r.Context(), sp.WorkspaceID, sp.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// maxRequest is the largest request body.
const maxRequest = 64 << 10

// readJSON decodes the request body into v strictly, answering 400 or 413 and returning
// false on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, err = dec.Token(); errors.Is(err, io.EOF) {
			return true
		}
		err = errors.New("one object expected")
	}
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeRequestTooLarge,
			fmt.Sprintf("the request body is over %d bytes", maxRequest), apierr.Params{"max": maxRequest})
		return false
	}
	apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
	return false
}

// serverError logs err and answers 500 without details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("search: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200
// with a broken body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("search: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
