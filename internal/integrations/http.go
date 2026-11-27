package integrations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/ratelimit"
)

type Options struct {
	PublicURL     string
	Egress        *egress.Policy
	ClientOptions []ClientOption
	Now           func() time.Time
}

type Service struct {
	store           *Store
	spaces          *access.Checker
	publicURL       string
	egress          egress.Policy
	clientOpts      []ClientOption
	beforeBeginRef  func()
	afterWriteAhead func() error
	afterCall       func() error

	now            func() time.Time
	hookIP, hookID *ratelimit.Limiter
	decoy          WebhookSecrets
	afterLookup    func()
	applier        atomic.Pointer[Applier]
	wake           chan struct{}
}

func New(store *Store, opts Options) *Service {
	policy := egress.MustParse("public")
	if opts.Egress != nil {
		policy = *opts.Egress
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	} else {
		store.now = now
	}
	s := &Service{store: store, publicURL: opts.PublicURL, egress: policy, clientOpts: opts.ClientOptions, now: now, wake: make(chan struct{}, 1), decoy: newDecoy()}
	s.hookIP, s.hookID = newHookLimits(now)
	return s
}

func (s *Service) Client(p Provider, settings map[string]string, opts ...ClientOption) *Client {
	return NewClient(p, settings, s.egress, opts...)
}

func (s *Service) Routes(mux *http.ServeMux, a *auth.Service, spaces *access.Checker) {
	s.spaces = spaces
	admin := func(h http.HandlerFunc) http.Handler { return a.RequireAdmin(h) }
	const base = "/api/workspaces/{workspace}/integrations"
	mux.Handle("GET /api/workspaces/{workspace}/integration-providers", admin(s.handleProviders))
	mux.Handle("GET "+base, admin(s.handleList))
	mux.Handle("POST "+base, admin(s.handleCreate))
	mux.Handle("GET "+base+"/{integration}", admin(s.handleGet))
	mux.Handle("PATCH "+base+"/{integration}", admin(s.handleUpdate))
	mux.Handle("DELETE "+base+"/{integration}", admin(s.handleDelete))
	mux.Handle("PUT "+base+"/{integration}/spaces", admin(s.handleSpaces))
	mux.Handle("POST "+base+"/{integration}/check", admin(s.handleCheck))
	mux.Handle("GET "+base+"/{integration}/events", admin(s.handleEvents))
	mux.Handle("POST "+base+"/{integration}/events/{event}/retry", admin(s.handleRetryEvent))
	mux.Handle("GET "+base+"/{integration}/payment-links/unresolved", admin(s.handleUnresolved))
	mux.Handle("POST "+base+"/{integration}/payment-links/{effect}/confirm-no-link", admin(s.handleConfirmNoLink))
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/integrations",
		s.spaces.RequireSpace(access.Read, http.HandlerFunc(s.handleForSpace)))
}

func (s *Service) view(rec record, visible map[int64]bool) Integration {
	out := rec.Integration
	out.Spaces = []int64{}
	out.ApprovalSpaces = []int64{}
	out.OtherSpaces = 0
	for _, sp := range rec.Spaces {
		if visible[sp] {
			out.Spaces = append(out.Spaces, sp)
			if slices.Contains(rec.ApprovalSpaces, sp) {
				out.ApprovalSpaces = append(out.ApprovalSpaces, sp)
			}
		} else {
			out.OtherSpaces++
		}
	}
	if u := hookURL(s.publicURL, rec.publicID); u != "" {
		out.WebhookURL = &u
	}
	return out
}

func (s *Service) manageable(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	spaces, err := s.spaces.Manageable(r.Context(), actorOf(r), workspaceOf(r))
	if err != nil {
		serverError(w, r, err)
		return nil, false
	}
	ids := make([]int64, len(spaces))
	for i, sp := range spaces {
		ids[i] = sp.ID
	}
	return ids, true
}

func visibleTo(ids []int64) map[int64]bool {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

func (s *Service) respondOne(w http.ResponseWriter, status int, rec record, visible map[int64]bool) {
	writeJSON(w, status, s.view(rec, visible))
}

func workspaceOf(r *http.Request) int64 {
	m, _ := auth.MembershipFrom(r.Context())
	return m.WorkspaceID
}

func actorOf(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

type ProviderInfo struct {
	ID       string        `json:"id"`
	Kinds    []string      `json:"kinds" apitypes:"nonnull"`
	Secrets  []SecretSpec  `json:"secrets" apitypes:"nonnull"`
	Settings []SettingSpec `json:"settings" apitypes:"nonnull"`
}

type Registry struct {
	Providers         []ProviderInfo `json:"providers" apitypes:"nonnull"`
	Currencies        []string       `json:"currencies" apitypes:"nonnull"`
	Exponents         map[string]int `json:"exponents" apitypes:"nonnull"`
	MaxAmountMinor    int64          `json:"max_amount_minor"`
	WebhooksAvailable bool           `json:"webhooks_available"`
}

func (s *Service) handleProviders(w http.ResponseWriter, _ *http.Request) {
	reg := Registry{Providers: []ProviderInfo{}, Currencies: Currencies(), Exponents: Exponents(), MaxAmountMinor: MaxAmountMinor, WebhooksAvailable: s.publicURL != ""}
	for _, p := range Providers() {
		reg.Providers = append(reg.Providers, ProviderInfo{ID: p.ID(), Kinds: p.Kinds(), Secrets: p.Secrets(), Settings: p.Settings()})
	}
	writeJSON(w, http.StatusOK, reg)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	recs, err := s.store.list(r.Context(), workspaceOf(r))
	if err != nil {
		serverError(w, r, err)
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	visible := visibleTo(ids)
	out := make([]Integration, len(recs))
	for i, rec := range recs {
		out[i] = s.view(rec, visible)
	}
	writeJSON(w, http.StatusOK, map[string][]Integration{"integrations": out})
}

func integrationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("integration"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeIntegrationNotFound, errNotFound.Error())
		return 0, false
	}
	return id, true
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	rec, err := s.store.getSnapshot(r.Context(), workspaceOf(r), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	s.respondOne(w, http.StatusOK, rec, visibleTo(ids))
}

type createRequest struct {
	Provider       string            `json:"provider"`
	Name           string            `json:"name"`
	DisplayName    string            `json:"display_name"`
	Settings       map[string]string `json:"settings"`
	Secrets        map[string]string `json:"secrets"`
	MaxAmountMinor int64             `json:"max_amount_minor"`
	Currencies     []string          `json:"currencies"`
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !readJSON(w, r, &req) {
		return
	}
	d, err := newDraft(req)
	if err != nil {
		fail(w, r, err)
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	rec, err := s.store.create(r.Context(), workspaceOf(r), actorOf(r), d)
	if err != nil {
		fail(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "integration created", "integration", rec.ID, "provider", rec.Provider, "by", actorOf(r))
	s.respondOne(w, http.StatusCreated, rec, visibleTo(ids))
}

type updateRequest struct {
	DisplayName    *string           `json:"display_name"`
	Settings       map[string]string `json:"settings"`
	Secrets        map[string]string `json:"secrets"`
	MaxAmountMinor *int64            `json:"max_amount_minor"`
	Currencies     []string          `json:"currencies"`
	Disabled       *bool             `json:"disabled"`
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	var req updateRequest
	if !readJSON(w, r, &req) {
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	rec, err := s.store.update(r.Context(), workspaceOf(r), actorOf(r), id, change{
		displayName: req.DisplayName, settings: req.Settings, secrets: req.Secrets,
		maxAmount: req.MaxAmountMinor, currencies: req.Currencies, disabled: req.Disabled,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "integration changed", "integration", rec.ID, "provider", rec.Provider, "by", actorOf(r))
	s.respondOne(w, http.StatusOK, rec, visibleTo(ids))
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	if err := s.store.remove(r.Context(), workspaceOf(r), id); err != nil {
		fail(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "integration removed", "integration", id, "by", actorOf(r))
	w.WriteHeader(http.StatusNoContent)
}

type spacesRequest struct {
	Spaces []int64 `json:"spaces"`
	Keep   []int64 `json:"keep"`
}

func (s *Service) handleSpaces(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	var req spacesRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Spaces == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "spaces must be a list of space ids")
		return
	}
	manageable, ok := s.manageable(w, r)
	if !ok {
		return
	}
	want := slices.Clone(req.Spaces)
	slices.Sort(want)
	want = slices.Compact(want)
	for _, sp := range want {
		if !slices.Contains(manageable, sp) {
			writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, "space not found")
			return
		}
	}
	keep := slices.Clone(req.Keep)
	slices.Sort(keep)
	keep = slices.Compact(keep)
	for _, sp := range keep {
		if !slices.Contains(want, sp) {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "keep names only spaces in spaces")
			return
		}
	}
	rec, err := s.store.setSpaces(r.Context(), workspaceOf(r), actorOf(r), id, want, keep, manageable)
	if err != nil {
		fail(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "integration spaces changed", "integration", id, "spaces", len(rec.Spaces), "by", actorOf(r))
	s.respondOne(w, http.StatusOK, rec, visibleTo(manageable))
}

type CheckResult struct {
	OK     bool          `json:"ok"`
	Remote bool          `json:"remote"`
	Code   apierr.Code   `json:"code,omitempty"`
	Params apierr.Params `json:"params,omitempty"`
}

func (s *Service) handleCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	ws := workspaceOf(r)
	var res CheckResult
	err := s.store.read(r.Context(), func(tx *sql.Tx) error {
		rec, err := s.store.get(r.Context(), tx, ws, id)
		if err != nil {
			return err
		}
		res, err = s.check(r.Context(), tx, ws, rec)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func settingsCheck(p Provider, settings map[string]string) (CheckResult, bool) {
	err := checkSettings(p, settings)
	if err == nil {
		return CheckResult{}, false
	}
	return CheckResult{Code: err.Code, Params: err.Params}, true
}

func (s *Service) check(ctx context.Context, tx *sql.Tx, workspace int64, rec record) (CheckResult, error) {
	p := Lookup(rec.Provider)
	if p == nil {
		return CheckResult{Code: apierr.CodeIntegrationProviderUnknown}, nil
	}
	if res, bad := settingsCheck(p, rec.Settings); bad {
		return res, nil
	}
	for _, spec := range p.Secrets() {
		if spec.Required && rec.Secrets[spec.Name] != secretSet {
			return CheckResult{Code: apierr.CodeIntegrationSecretRequired, Params: apierr.Params{"secret": spec.Name}}, nil
		}
	}
	creds, err := s.store.openCredentials(ctx, tx, workspace, rec.ID)
	if err != nil {
		var bad unreadableError
		if errors.As(err, &bad) {
			return CheckResult{Code: apierr.CodeIntegrationSecretUnreadable, Params: apierr.Params{"secret": bad.secret}}, nil
		}
		return CheckResult{}, err
	}
	current, previous, err := s.store.sharedWebhookSecrets(ctx, tx, rec.ID, creds)
	if err != nil {
		return CheckResult{}, err
	}
	if current || previous {
		name := SecretWebhookSecret
		if !current {
			name = SecretWebhookSecretPrevious
		}
		return CheckResult{Code: apierr.CodeIntegrationWebhookSecretShared, Params: apierr.Params{"secret": name}}, nil
	}
	return CheckResult{OK: true}, nil
}

func (s *Service) handleForSpace(w http.ResponseWriter, r *http.Request) {
	sp, _ := access.SpaceFrom(r.Context())
	list, err := s.store.forSpace(r.Context(), workspaceOf(r), sp.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]SpaceIntegration{"integrations": list})
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var bad *apierr.Error
	switch {
	case errors.As(err, &bad):
		apierr.WriteError(w, bad)
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w)
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeIntegrationNotFound, errNotFound.Error())
	case errors.Is(err, errTaken):
		writeError(w, http.StatusConflict, apierr.CodeIntegrationNameTaken, errTaken.Error())
	case errors.Is(err, errTooMany):
		writeError(w, http.StatusConflict, apierr.CodeTooManyIntegrations,
			"at most "+strconv.Itoa(maxIntegrations)+" integrations per workspace", apierr.Params{"max": maxIntegrations})
	case errors.Is(err, errInUse):
		writeError(w, http.StatusConflict, apierr.CodeIntegrationInUse, errInUse.Error())
	default:
		serverError(w, r, err)
	}
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.ErrorContext(r.Context(), "integrations: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

const maxBody = 64 << 10

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBody)
	var raw json.RawMessage
	dec := json.NewDecoder(body)
	if err := dec.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: an object expected")
		return false
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("integrations: encode response", "err", err)
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
