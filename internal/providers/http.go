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

type Service struct {
	store                       *Store
	feed                        *Feed
	egress                      Egress
	workspaceClient, userClient *http.Client
	resolve                     egress.Resolver
}

func New(store *Store, feed *Feed) *Service {
	s := &Service{store: store, feed: feed}
	s.SetEgress(Egress{})
	return s
}

func (s *Service) SetEgress(e Egress) {
	s.egress = e
	s.workspaceClient = checkClient(e.Workspace)
	s.userClient = checkClient(e.Personal)
}

func (s *Service) client(o owner) *http.Client {
	if o.workspace != 0 {
		return s.workspaceClient
	}
	return s.userClient
}

func (s *Service) checkEgress(ctx context.Context, o owner, baseURL string) error {
	u, err := checkBaseURL(baseURL)
	if err != nil {
		return nil
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

func (s *Service) changed(r *http.Request) {
	if s.feed == nil {
		return
	}
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

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.ErrorContext(r.Context(), "providers: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

const maxBody = 64 << 10

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
