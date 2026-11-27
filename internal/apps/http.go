package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/quota"
)

func (s *Service) Routes(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.Handler {
		return s.access.ExpectUser(s.access.RequireSpace(access.Read, h))
	}
	write := func(h http.HandlerFunc) http.Handler {
		return s.access.ExpectUser(s.access.RequireSpace(access.Write, h))
	}
	tokenRead := func(h http.HandlerFunc) http.Handler {
		return s.access.ExpectUser(auth.AcceptTokens(s.access.RequireSpace(access.Read, h)))
	}
	const apps = "/api/workspaces/{workspace}/spaces/{space}/apps"

	mux.Handle("GET "+apps, tokenRead(s.handleList))
	mux.Handle("POST "+apps, write(s.handleCreate))
	mux.Handle("GET "+apps+"/{app}", tokenRead(s.handleGet))
	mux.Handle("PATCH "+apps+"/{app}", write(s.handleUpdate))
	mux.Handle("POST "+apps+"/{app}/move", write(s.handleMove))
	mux.Handle("POST "+apps+"/{app}/duplicate", read(s.handleDuplicate))
	mux.Handle("POST "+apps+"/{app}/effect-board", write(s.handleChooseBoard))
	mux.Handle("POST "+"/api/workspaces/{workspace}/spaces/{space}/app-templates/{template}/install", write(s.handleInstall))
	mux.Handle("GET "+apps+"/{app}/surface", tokenRead(s.handleSurface))
	mux.Handle("POST "+apps+"/{app}/actions", tokenRead(s.handleAction))
	mux.Handle("POST "+apps+"/{app}/actions/preview", read(s.handlePreviewAction))
	mux.Handle("GET "+apps+"/{app}/effects", tokenRead(s.handleEffects))
	mux.Handle("GET "+apps+"/{app}/records/{record}/effects", tokenRead(s.handleRecordEffects))
	mux.Handle("POST "+apps+"/{app}/effects/{effect}/retry", write(s.handleRetryEffect))
	mux.Handle("POST "+apps+"/{app}/preview", read(s.handlePreview))
	mux.Handle("POST "+apps+"/{app}/publish", write(s.handlePublish))
	mux.Handle("POST "+apps+"/{app}/rollback", write(s.handleRollback))
	mux.Handle("GET "+apps+"/{app}/releases", tokenRead(s.handleReleases))
	mux.Handle("GET "+apps+"/{app}/releases/{release}", tokenRead(s.handleRelease))
	mux.Handle("GET "+apps+"/{app}/records", tokenRead(s.handleRecords))
	mux.Handle("GET "+apps+"/{app}/records/deleted", tokenRead(s.handleDeleted))
	mux.Handle("GET "+apps+"/{app}/records/query", tokenRead(s.handleQuery))
	mux.Handle("GET "+apps+"/{app}/records/{record}/history", tokenRead(s.handleHistory))
	mux.Handle("POST "+apps+"/{app}/records/{record}/restore", write(s.handleRestore))
	mux.Handle("GET "+apps+"/{app}/offline", read(s.handleOffline))
	mux.Handle("GET "+apps+"/{app}/integrations", read(s.handleIntegrationStatus))
	mux.Handle("POST "+apps+"/{app}/integrations/{slot}/request", write(s.handleRequestApproval))
	mux.Handle("GET "+apps+"/{app}/roles", read(s.handleRoles))
	mux.Handle("PUT "+apps+"/{app}/roles", write(s.handleSetRoles))
	if s.dataDir != "" {
		s.attachmentRoutes(mux, read, write, apps)
	}
}

const maxBody = MaxDefinitionSize + 16<<10

type request struct {
	space access.Space
	actor Actor
	app   int64
}

func parse(w http.ResponseWriter, r *http.Request, withApp bool) (request, bool) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	req := request{space: sp, actor: Actor{UserID: u.ID}}
	if t, ok := auth.TokenFrom(r.Context()); ok {
		req.actor.Agent, req.actor.Via = true, t.Name
	}
	if withApp {
		id, err := strconv.ParseInt(r.PathValue("app"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
			return request{}, false
		}
		req.app = id
	}
	return req, true
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	withArchived := false
	switch r.URL.Query().Get("archived") {
	case "", "false":
	case "true":
		withArchived = true
	default:
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `archived must be "true" or "false"`)
		return
	}
	all, err := s.listApps(r.Context(), req.space, req.actor, withArchived)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]App{"apps": all})
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	var na NewApp
	if !readJSON(w, r, &na) {
		return
	}
	a, err := s.createApp(r.Context(), req.space, req.actor, na)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	a, err := s.getApp(r.Context(), req.space, req.actor, req.app)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var ch Changes
	if !readJSON(w, r, &ch) {
		return
	}
	a, err := s.updateApp(r.Context(), req.space, req.actor, req.app, ch)
	s.result(w, r, req, a, err)
}

func (s *Service) handleMove(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Version int64 `json:"version"`
		SpaceID int64 `json:"space_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	to, err := s.access.Check(r.Context(), req.actor.UserID, body.SpaceID)
	if errors.Is(err, access.ErrNotFound) {
		err = ErrNotFound
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	a, err := s.moveApp(r.Context(), req.space, req.actor, req.app, body.Version, to)
	s.result(w, r, req, a, err)
}

func (s *Service) handleSurface(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	q := r.URL.Query()
	var msgs []Message
	var err error
	if widget := q.Get("widget"); widget != "" {
		if q.Get("release") != "" || q.Get("screen") != "" {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "widget can't be combined with release or screen")
			return
		}
		msgs, err = s.widgetSurface(r.Context(), req.space, req.actor, req.app, widget)
	} else {
		msgs, err = s.surface(r.Context(), req.space, req.actor, req.app, q.Get("release"), q.Get("screen"))
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Service) handleAction(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	widget := r.URL.Query().Get("widget")
	if widget != "" && !namePattern.MatchString(widget) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "widget is a widget's id")
		return
	}
	var action ClientAction
	if !readJSON(w, r, &action) {
		return
	}
	msgs, err := s.act(r.Context(), req.space, req.actor, req.app, action)
	if err == nil && widget != "" {
		msgs = BoundItems(msgs, MaxWidgetItems)
	}
	if errors.Is(err, ErrInvalid) {
		invalid(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Service) handlePreview(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body PreviewRequest
	if !readJSONLimit(w, r, &body, previewMaxBody) {
		return
	}
	reply, err := s.preview(r.Context(), req.space, req.actor, req.app, body)
	if errors.Is(err, ErrInvalid) {
		invalid(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body Publish
	if !readJSON(w, r, &body) {
		return
	}
	a, err := s.publishApp(r.Context(), req.space, req.actor, req.app, body)
	s.result(w, r, req, a, err)
}

func (s *Service) handleRollback(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body Rollback
	if !readJSON(w, r, &body) {
		return
	}
	a, err := s.rollbackApp(r.Context(), req.space, req.actor, req.app, body)
	s.result(w, r, req, a, err)
}

func (s *Service) handleReleases(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	all, err := s.listReleases(r.Context(), req.space, req.actor, req.app)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Release{"releases": all})
}

func (s *Service) handleRelease(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(r.PathValue("release"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeAppReleaseNotFound, ErrReleaseNotFound.Error())
		return
	}
	rel, err := s.getRelease(r.Context(), req.space, req.actor, req.app, n)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Service) handleRecords(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, false)
}

func (s *Service) handleDeleted(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, true)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, deleted bool) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	out, err := s.listRecords(r.Context(), req.space, req.actor, req.app, deleted)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleQuery(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	q, err := QueryFromValues(r.URL.Query())
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := s.queryRecords(r.Context(), req.space, req.actor, req.app, q)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func recordID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("record"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
		return 0, false
	}
	return id, true
}

func (s *Service) handleHistory(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	id, ok := recordID(w, r)
	if !ok {
		return
	}
	out, err := s.recordHistory(r.Context(), req.space, req.actor, req.app, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleRestore(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	id, ok := recordID(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	rec, err := s.restoreRecord(r.Context(), req.space, req.actor, req.app, id, body.ExpectedRevision)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Service) handleRoles(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	grants, err := s.appRoles(r.Context(), req.space, req.actor, req.app)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rolesReply{Grants: grants, Roles: access.AppRoles})
}

type rolesReply struct {
	Grants []access.AppGrant `json:"grants"`
	Roles  []access.AppRole  `json:"roles"`
}

func (s *Service) handleSetRoles(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Grants []access.AppGrant `json:"grants"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	grants, err := s.setAppRoles(r.Context(), req.space, req.actor, req.app, body.Grants)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rolesReply{Grants: grants, Roles: access.AppRoles})
}

func (s *Service) result(w http.ResponseWriter, r *http.Request, req request, a App, err error) {
	switch {
	case errors.Is(err, ErrStale):
		current, cerr := app(r.Context(), s.db, req.space.ID, req.app)
		if cerr == nil {
			cerr = s.present(r.Context(), req.actor, &current)
		}
		if cerr != nil {
			fail(w, r, cerr)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": ErrStale.Error(), "code": apierr.CodeAppChanged, "current": current})
	case err != nil:
		fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, a)
	}
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var full *fullError
	var outdated *outdatedError
	var incompatible *incompatibleError
	var migration *migrationError
	var conflict *conflictError
	var status *statusError
	var apiErr *apierr.Error
	var limited *rateLimitedError
	refusal, quotaRefusal := quota.AsAPI(err)
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(limited.retryAfter()))
		apierr.WriteError(w, limited.api)
	case quotaRefusal:
		apierr.WriteError(w, refusal)
	case errors.As(err, &status):
		writeError(w, status.status, status.code, status.msg, status.params)
	case errors.As(err, &apiErr):
		apierr.WriteError(w, apiErr)
	case errors.As(err, &conflict):
		body := map[string]any{"error": err.Error(), "code": apierr.CodeAppRecordConflict, "params": apierr.Params{"id": conflict.id}}
		if conflict.current != nil {
			body["current"] = conflict.current
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w)
	case errors.Is(err, errSpaceArchived):
		writeError(w, http.StatusForbidden, apierr.CodeSpaceArchived, err.Error())
	case errors.Is(err, ErrReadOnly):
		writeError(w, http.StatusForbidden, apierr.CodeSpaceReadOnly, err.Error())
	case errors.Is(err, ErrForbidden):
		var f *forbiddenError
		params := apierr.Params{}
		if errors.As(err, &f) {
			params["capability"] = f.capability
		}
		writeError(w, http.StatusForbidden, apierr.CodeAppForbidden, err.Error(), params)
	case errors.Is(err, ErrOperationReused):
		writeError(w, http.StatusConflict, apierr.CodeAppOperationReused, err.Error())
	case errors.Is(err, errEffectNotFailed):
		writeError(w, http.StatusConflict, apierr.CodeAppEffectNotFailed, err.Error())
	case errors.Is(err, ErrOffline):
		writeError(w, http.StatusConflict, apierr.CodeAppOffline, err.Error())
	case errors.Is(err, ErrNotPublished):
		writeError(w, http.StatusConflict, apierr.CodeAppNotPublished, err.Error())
	case errors.Is(err, ErrReleaseNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeAppReleaseNotFound, err.Error())
	case errors.As(err, &outdated):
		writeError(w, http.StatusConflict, apierr.CodeAppReleaseOutdated, err.Error(), apierr.Params{"release": outdated.release, "live": outdated.live})
	case errors.As(err, &incompatible):
		writeError(w, http.StatusConflict, apierr.CodeAppReleaseIncompatible, err.Error(), apierr.Params{"fields": incompatible.fields})
	case errors.Is(err, ErrReleaseLive):
		writeError(w, http.StatusConflict, apierr.CodeAppReleaseLive, err.Error())
	case errors.Is(err, ErrNothingToPublish):
		writeError(w, http.StatusConflict, apierr.CodeAppNothingToPublish, err.Error())
	case errors.As(err, &migration):
		code := apierr.CodeAppMigrationUnneeded
		if migration.unacknowledged {
			code = apierr.CodeAppMigrationRequired
		}
		writeError(w, http.StatusConflict, code, err.Error(), apierr.Params{"changes": migration.changes})
	case errors.Is(err, ErrStale):
		writeError(w, http.StatusConflict, apierr.CodeAppChanged, err.Error())
	case errors.As(err, &full):
		writeError(w, http.StatusConflict, full.code, err.Error(), full.params)
	case errors.Is(err, ErrInvalid):
		invalid(w, http.StatusBadRequest, err)
	default:
		if r.Context().Err() != nil {
			return
		}
		slog.Error("apps: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apierr.Internal(w)
	}
}

func invalid(w http.ResponseWriter, status int, err error) {
	var bad *invalidError
	if !errors.As(err, &bad) {
		writeError(w, status, apierr.CodeBadRequest, err.Error())
		return
	}
	writeError(w, status, bad.code, bad.msg, bad.params)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) && limit == previewMaxBody {
			writeError(w, http.StatusRequestEntityTooLarge, apierr.CodeAppPreviewTooLarge,
				fmt.Sprintf("a preview's request is at most %d MiB", previewMaxBody>>20), apierr.Params{"max_mib": previewMaxBody >> 20})
			return false
		}
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: "+err.Error())
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
		slog.Error("apps: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response","code":"internal"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string, params ...apierr.Params) {
	apierr.Write(w, status, code, msg, params...)
}
