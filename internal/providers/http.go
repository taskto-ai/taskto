package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
)

// Service serves the provider account API (docs/api.md) and keeps the agent runtime's
// providers in step with it.
type Service struct {
	store  *Store
	feed   *Feed // nil when the agent runtime is off
	egress Egress
	// The clients of account tests, under each scope's egress policy.
	workspaceClient, userClient *http.Client
	resolve                     egress.Resolver // nil: the system's
}

// New returns the API over store. feed is nil when the agent runtime is off: accounts are
// then only stored. Its egress policies are "any" until SetEgress.
func New(store *Store, feed *Feed) *Service {
	s := &Service{store: store, feed: feed}
	s.SetEgress(Egress{})
	return s
}

// SetEgress sets the egress policies saving and testing accounts check. Set it before serving.
func (s *Service) SetEgress(e Egress) {
	s.egress = e
	s.workspaceClient = checkClient(e.Workspace)
	s.userClient = checkClient(e.Personal)
}

// client is the client of o's account tests.
func (s *Service) client(o owner) *http.Client {
	if o.workspace != 0 {
		return s.workspaceClient
	}
	return s.userClient
}

// checkEgress refuses a base URL o's egress policy doesn't allow, resolving its host now; a
// host that doesn't resolve now is left to the check of every connection.
func (s *Service) checkEgress(ctx context.Context, o owner, baseURL string) error {
	u, err := checkBaseURL(baseURL)
	if err != nil {
		return nil // the validation says what's wrong with it
	}
	ctx, cancel := context.WithTimeout(ctx, egressCheckTimeout)
	defer cancel()
	if err := s.egress.of(o).Check(ctx, u.Hostname(), s.resolve); err != nil {
		if refused := egress.Refusal(err); refused != nil {
			return refused.APIError("base_url")
		}
		return err
	}
	return nil
}

// Routes mounts the provider account endpoints on mux, behind a's access checks.
func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	ws := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	admin := func(h http.HandlerFunc) http.Handler { return a.RequireAdmin(h) }
	me := func(h http.HandlerFunc) http.Handler { return a.RequireUser(h) }

	mux.Handle("GET /api/workspaces/{workspace}/providers", ws(s.handleList(workspaceOwner)))
	mux.Handle("POST /api/workspaces/{workspace}/providers", admin(s.handleCreate(workspaceOwner)))
	mux.Handle("PATCH /api/workspaces/{workspace}/providers/{account}", admin(s.handleUpdate(workspaceOwner)))
	mux.Handle("DELETE /api/workspaces/{workspace}/providers/{account}", admin(s.handleDelete(workspaceOwner)))
	mux.Handle("POST /api/workspaces/{workspace}/providers/{account}/test", admin(s.handleCheck(workspaceOwner)))

	mux.Handle("GET /api/me/providers", me(s.handleList(userOwner)))
	mux.Handle("POST /api/me/providers", me(s.handleCreate(userOwner)))
	mux.Handle("PATCH /api/me/providers/{account}", me(s.handleUpdate(userOwner)))
	mux.Handle("DELETE /api/me/providers/{account}", me(s.handleDelete(userOwner)))
	mux.Handle("POST /api/me/providers/{account}/test", me(s.handleCheck(userOwner)))

	mux.Handle("GET /api/workspaces/{workspace}/models", ws(s.handleModels))
}

// workspaceOwner and userOwner read the owner of the accounts a route acts on from what the
// auth middleware put on the request.
func workspaceOwner(r *http.Request) owner {
	m, _ := auth.MembershipFrom(r.Context())
	return owner{workspace: m.WorkspaceID}
}

func userOwner(r *http.Request) owner {
	u, _ := auth.UserFrom(r.Context())
	return owner{user: u.ID}
}

func (s *Service) handleList(ownerOf func(*http.Request) owner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recs, err := s.store.list(r.Context(), ownerOf(r))
		if err != nil {
			serverError(w, r, err)
			return
		}
		accounts := make([]Account, len(recs))
		for i, rec := range recs {
			accounts[i] = rec.Account
		}
		writeJSON(w, http.StatusOK, map[string][]Account{"providers": accounts})
	}
}

// createRequest is the body of a create. Kind defaults to openai-compatible.
type createRequest struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	Kind        string          `json:"kind"`
	BaseURL     string          `json:"base_url"`
	APIKey      *string         `json:"api_key"`
	Compat      json.RawMessage `json:"compat"`
	Models      []Model         `json:"models"`
}

func (s *Service) handleCreate(ownerOf func(*http.Request) owner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createRequest
		if !readJSON(w, r, &req) {
			return
		}
		if req.Kind == "" {
			req.Kind = KindOpenAICompatible
		}
		d := draft{
			name: req.Name, displayName: req.DisplayName, kind: req.Kind, baseURL: req.BaseURL,
			apiKey: req.APIKey, compat: req.Compat, models: req.Models,
		}
		if err := d.validate(d.apiKey != nil); err != nil {
			fail(w, r, err)
			return
		}
		if err := s.checkEgress(r.Context(), ownerOf(r), d.baseURL); err != nil {
			fail(w, r, err)
			return
		}
		u, _ := auth.UserFrom(r.Context())
		rec, err := s.store.create(r.Context(), ownerOf(r), d, u.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		slog.InfoContext(r.Context(), "provider account created", "account", rec.ID, "provider", rec.Provider, "by", u.ID)
		s.changed(r)
		writeJSON(w, http.StatusCreated, rec.Account)
	}
}

// optional is a field of a PATCH body: absent (not set), null (set, nil value) or a value.
type optional[T any] struct {
	set   bool
	value *T
}

func (o *optional[T]) UnmarshalJSON(raw []byte) error {
	o.set = true
	if string(raw) == "null" {
		return nil
	}
	o.value = new(T)
	return json.Unmarshal(raw, o.value)
}

// updateRequest is the body of a PATCH. An absent field stays as it is. api_key: a string sets
// a new key, null removes it. compat: an object replaces the settings, null removes them. The
// name and kind can't change: the name is part of the agent runtime's provider ID.
type updateRequest struct {
	Name        optional[string]          `json:"name"`
	Kind        optional[string]          `json:"kind"`
	DisplayName optional[string]          `json:"display_name"`
	BaseURL     optional[string]          `json:"base_url"`
	APIKey      optional[string]          `json:"api_key"`
	Compat      optional[json.RawMessage] `json:"compat"`
	Models      optional[[]Model]         `json:"models"`
}

func (s *Service) handleUpdate(ownerOf func(*http.Request) owner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := accountID(w, r)
		if !ok {
			return
		}
		var req updateRequest
		if !readJSON(w, r, &req) {
			return
		}
		if req.Name.set || req.Kind.set {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "name and kind can't change: add a new account instead")
			return
		}
		// Only a new base URL is checked: an account the policy came to forbid can still be
		// renamed or pointed elsewhere.
		if req.BaseURL.set {
			if err := s.checkEgress(r.Context(), ownerOf(r), deref(req.BaseURL.value)); err != nil {
				fail(w, r, err)
				return
			}
		}
		rec, err := s.store.update(r.Context(), ownerOf(r), id, func(d *draft) {
			if req.DisplayName.set {
				d.displayName = deref(req.DisplayName.value)
			}
			if req.BaseURL.set {
				d.baseURL = deref(req.BaseURL.value)
			}
			if req.APIKey.set {
				d.apiKey, d.keepKey = req.APIKey.value, false
			}
			if req.Compat.set {
				d.compat = deref(req.Compat.value)
			}
			if req.Models.set {
				d.models = deref(req.Models.value)
			}
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		u, _ := auth.UserFrom(r.Context())
		slog.InfoContext(r.Context(), "provider account changed", "account", rec.ID, "provider", rec.Provider,
			"by", u.ID, "api_key_changed", req.APIKey.set)
		s.changed(r)
		writeJSON(w, http.StatusOK, rec.Account)
	}
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func (s *Service) handleDelete(ownerOf func(*http.Request) owner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := accountID(w, r)
		if !ok {
			return
		}
		if err := s.store.remove(r.Context(), ownerOf(r), id); err != nil {
			fail(w, r, err)
			return
		}
		u, _ := auth.UserFrom(r.Context())
		slog.InfoContext(r.Context(), "provider account deleted", "account", id, "by", u.ID)
		s.changed(r)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleModels lists what the caller's conversations in the workspace may use: the
// operator's providers, the workspace's accounts and the caller's own.
func (s *Service) handleModels(w http.ResponseWriter, r *http.Request) {
	choices := []Choice{}
	if s.feed != nil {
		choices = append(choices, s.feed.Instance()...)
	}
	for _, o := range []owner{workspaceOwner(r), userOwner(r)} {
		recs, err := s.store.list(r.Context(), o)
		if err != nil {
			serverError(w, r, err)
			return
		}
		for _, rec := range recs {
			choices = append(choices, choiceOf(rec))
		}
	}
	writeJSON(w, http.StatusOK, map[string][]Choice{"providers": choices})
}

// changed brings the agent runtime up to date after a change. The change is saved either way:
// a failure here is logged, and a restarted agent runtime reads the files anew.
func (s *Service) changed(r *http.Request) {
	if s.feed == nil {
		return
	}
	// Not the request's context: a client that leaves mustn't stop the files half-updated.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := s.feed.Sync(ctx); err != nil {
		slog.ErrorContext(ctx, "providers: updating the agent runtime", "err", err)
	}
}

func accountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("account"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeProviderNotFound, errNotFound.Error())
		return 0, false
	}
	return id, true
}

// fail answers with the status for one of the package's errors, or 500.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var bad *apierr.Error
	switch {
	case errors.As(err, &bad):
		apierr.WriteError(w, bad)
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeProviderNotFound, errNotFound.Error())
	case errors.Is(err, errTaken):
		writeError(w, http.StatusConflict, apierr.CodeProviderNameTaken, errTaken.Error())
	case errors.Is(err, errTooMany):
		writeError(w, http.StatusConflict, apierr.CodeTooManyProviders,
			"at most "+strconv.Itoa(maxAccounts)+" provider accounts per workspace or user", apierr.Params{"max": maxAccounts})
	default:
		serverError(w, r, err)
	}
}

// serverError logs err and answers 500 without details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.ErrorContext(r.Context(), "providers: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

const maxBody = 64 << 10

// readJSON decodes the request body into v, strictly: unknown fields and trailing data are
// errors. It answers 400 and returns false on failure. The decoder's message isn't passed on:
// it can quote the body, which may hold a key.
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

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200 with a
// broken body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("providers: encode response", "err", err)
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
