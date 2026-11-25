// Package integrations is a workspace's connections to outside services, starting with
// payments: the closed registry of providers (Stripe, Square), each connection's settings,
// limits and sealed secrets, and the spaces whose apps may use it (docs/integrations.md).
//
// The registry is code. An integration holds only what an admin typed for one provider account:
// its credentials, sealed with the server key (internal/serverkey) and write-only, so the API
// says "set" or "not set" and nothing, not a reply, the log, the audit log, an error or the
// agent, ever holds one in clear. Nothing here calls a provider: that, the approval of an app's
// use of an integration, and the webhook are the cards that follow; this package gives them the
// store (Credentials, Usable) and the address the webhook will have.
//
// Admins manage a workspace's integrations: Routes wraps them in auth.Service.RequireAdmin,
// which takes sessions only (an access token, and with it MCP, is refused), and every query is
// scoped to the workspace of the request, so another workspace's id is "not found".
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

// Options configure the Service.
type Options struct {
	// PublicURL is the server's external origin (-public-url); empty when it has none. The
	// webhook address shown to admins is made from it.
	PublicURL string
	// Egress is where calls to providers may go (-integration-egress). Nil is "public": every
	// connection's address is checked, so a call never reaches a private or loopback address.
	Egress *egress.Policy
	// Now is the clock of the webhook route's signature tolerance, the limits and the event
	// worker; nil is the real one. It is for tests, which move time without sleeping.
	Now func() time.Time
}

// Service serves the integrations API (docs/api.md#integrations).
type Service struct {
	store     *Store
	spaces    *access.Checker
	publicURL string
	egress    egress.Policy

	now func() time.Time
	// hookIP and hookID limit the webhook route (webhook.go); applier applies stored events and
	// wake tells the worker (events.go).
	hookIP, hookID *ratelimit.Limiter
	applier        atomic.Pointer[Applier]
	wake           chan struct{}
}

// New returns the API over store.
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
	s := &Service{store: store, publicURL: opts.PublicURL, egress: policy, now: now, wake: make(chan struct{}, 1)}
	s.hookIP, s.hookID = newHookLimits(now)
	return s
}

// Client returns the client for calls to provider p of an integration with these settings, under
// the server's integration egress policy (Credentials has what to send).
func (s *Service) Client(p Provider, settings map[string]string, opts ...ClientOption) *Client {
	return NewClient(p, settings, s.egress, opts...)
}

// Routes mounts the integrations endpoints on mux, behind a's access checks; spaces tells which
// spaces an admin may allow and who reads a space's list.
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
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/integrations",
		s.spaces.RequireSpace(access.Read, http.HandlerFunc(s.handleForSpace)))
}

// view is an integration as the API shows the caller: the webhook address the server's public
// URL gives it, and the spaces it is allowed in that the caller manages (visible), which never
// include another person's private space; those are only counted.
func (s *Service) view(rec record, visible map[int64]bool) Integration {
	out := rec.Integration
	out.Spaces = []int64{}
	out.OtherSpaces = 0
	for _, sp := range rec.Spaces {
		if visible[sp] {
			out.Spaces = append(out.Spaces, sp)
		} else {
			out.OtherSpaces++
		}
	}
	if u := hookURL(s.publicURL, rec.publicID); u != "" {
		out.WebhookURL = &u
	}
	return out
}

// manageable lists the spaces the caller, an admin, may manage: the ids view and setSpaces scope
// everything to. On an error it has answered.
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

// visibleTo is the set of ids view takes.
func visibleTo(ids []int64) map[int64]bool {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// respondOne writes rec as the caller sees it, with the spaces (visible) the caller managed when
// the request began. A mutation reads them before it commits, so a failed lookup can't answer an
// error for a change that was made.
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

// ProviderInfo is a registered provider as the API shows it, for the form that connects one.
type ProviderInfo struct {
	ID       string        `json:"id"`
	Kinds    []string      `json:"kinds" apitypes:"nonnull"`
	Secrets  []SecretSpec  `json:"secrets" apitypes:"nonnull"`
	Settings []SettingSpec `json:"settings" apitypes:"nonnull"`
}

// Registry is what a form needs to connect an integration.
type Registry struct {
	Providers  []ProviderInfo `json:"providers" apitypes:"nonnull"`
	Currencies []string       `json:"currencies" apitypes:"nonnull"`
	// Exponents is each currency's number of decimals, the server's own table: the web app
	// writes an amount in the unit the server checks it in, whatever its formatter says.
	Exponents map[string]int `json:"exponents" apitypes:"nonnull"`
	// MaxAmountMinor is the largest amount ceiling the server accepts.
	MaxAmountMinor int64 `json:"max_amount_minor"`
	// WebhooksAvailable is whether the server has a public URL, so providers can reach it.
	WebhooksAvailable bool `json:"webhooks_available"`
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

// createRequest is the body of a create. Secrets are the clear values, which only live here,
// between the request and sealing.
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

// updateRequest is the body of a PATCH. An absent or null field stays as it is. settings and
// currencies replace the whole value; a secret in secrets replaces that secret, and none can be
// removed. The name and the provider can't change.
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

// spacesRequest is the body of a PUT of the allowed spaces.
type spacesRequest struct {
	Spaces []int64 `json:"spaces"`
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
	// Only spaces the admin can see and manage: their own private space and the workspace's shared
	// ones. Anyone else's private space is "not found", as it is everywhere else.
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
	// Only the caller's manageable spaces change; the integration's others stay as they are.
	rec, err := s.store.setSpaces(r.Context(), workspaceOf(r), actorOf(r), id, want, manageable)
	if err != nil {
		fail(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "integration spaces changed", "integration", id, "spaces", len(rec.Spaces), "by", actorOf(r))
	s.respondOne(w, http.StatusOK, rec, visibleTo(manageable))
}

// CheckResult is the answer of a check. Remote says whether the provider was asked: this version
// only checks what the server holds, so it is false (docs/integrations.md#checking-a-connection).
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
	// One snapshot: the integration, which secrets are set and their opening are of one state, so
	// a removal that commits meanwhile is "not found" or a finished check, never an "internal".
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

// check looks at what the server holds of an integration: every required secret is set and opens
// with the server's key, and the settings are the provider's. A secret that doesn't open is what
// restoring a backup on another machine leaves, so it is the one thing worth saying aloud. An
// error is a failure to read, not an answer.
func (s *Service) check(ctx context.Context, tx *sql.Tx, workspace int64, rec record) (CheckResult, error) {
	p := Lookup(rec.Provider)
	if p == nil {
		return CheckResult{Code: apierr.CodeIntegrationProviderUnknown}, nil
	}
	if err := checkSettings(p, rec.Settings); err != nil {
		return CheckResult{Code: err.Code, Params: err.Params}, nil //nolint:nilerr // a bad setting is the check's answer, not a failure
	}
	for _, spec := range p.Secrets() {
		if spec.Required && rec.Secrets[spec.Name] != secretSet {
			return CheckResult{Code: apierr.CodeIntegrationSecretRequired, Params: apierr.Params{"secret": spec.Name}}, nil
		}
	}
	if _, err := s.store.openCredentials(ctx, tx, workspace, rec.ID); err != nil {
		var bad unreadableError
		if errors.As(err, &bad) {
			return CheckResult{Code: apierr.CodeIntegrationSecretUnreadable, Params: apierr.Params{"secret": bad.secret}}, nil
		}
		return CheckResult{}, err
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

// fail answers with the status for one of the package's errors, or 500.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var bad *apierr.Error
	switch {
	case errors.As(err, &bad):
		apierr.WriteError(w, bad)
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w) // the admin was disabled after the request was let in
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

// serverError logs err and answers 500 without details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.ErrorContext(r.Context(), "integrations: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

const maxBody = 64 << 10

// readJSON decodes the request body into v, strictly: unknown fields and trailing data are
// errors. The decoder's message isn't passed on: it can quote the body, which holds secrets.
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
	// A top-level null decodes into a struct as "nothing set", which would answer 200 for a change
	// that wasn't made: the body is an object.
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

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200 with a
// broken body.
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
