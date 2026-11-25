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

// Routes mounts the app endpoints on mux, each behind access.Checker.RequireSpace, so
// they need a session, and the CSRF guard (auth.CSRF) checks every one that isn't a GET.
// docs/api.md describes them.
func (s *Service) Routes(mux *http.ServeMux) {
	// Each route first checks the account the request says it is for, if it says (a queued
	// offline record is sent later, and the browser's session may be someone else's by then).
	read := func(h http.HandlerFunc) http.Handler {
		return s.access.ExpectUser(s.access.RequireSpace(access.Read, h))
	}
	write := func(h http.HandlerFunc) http.Handler {
		return s.access.ExpectUser(s.access.RequireSpace(access.Write, h))
	}
	const apps = "/api/workspaces/{workspace}/spaces/{space}/apps"

	mux.Handle("GET "+apps, read(s.handleList))
	mux.Handle("POST "+apps, write(s.handleCreate))
	mux.Handle("GET "+apps+"/{app}", read(s.handleGet))
	mux.Handle("PATCH "+apps+"/{app}", write(s.handleUpdate))
	mux.Handle("POST "+apps+"/{app}/move", write(s.handleMove))
	// A copy of the app's definition and nothing else, and a board for its effects (templates.go).
	// Duplicating reads the source, so it is the readers' route; the new app is made in a space the
	// caller writes, which duplicateApp checks.
	mux.Handle("POST "+apps+"/{app}/duplicate", read(s.handleDuplicate))
	mux.Handle("POST "+apps+"/{app}/effect-board", write(s.handleChooseBoard))
	mux.Handle("POST "+"/api/workspaces/{workspace}/spaces/{space}/app-templates/{template}/install", write(s.handleInstall))
	mux.Handle("GET "+apps+"/{app}/surface", read(s.handleSurface))
	// Actions are the readers' route too: navigating changes nothing, and act asks for the
	// right to write the space itself for every other kind of action.
	mux.Handle("POST "+apps+"/{app}/actions", read(s.handleAction))
	// A preview of what an action of the draft would set going: it changes nothing.
	mux.Handle("POST "+apps+"/{app}/actions/preview", read(s.handlePreviewAction))
	// The effects of actions (effects_api.go): listed to whoever reads the records, retried by
	// the user who acted.
	mux.Handle("GET "+apps+"/{app}/effects", read(s.handleEffects))
	mux.Handle("GET "+apps+"/{app}/records/{record}/effects", read(s.handleRecordEffects))
	mux.Handle("POST "+apps+"/{app}/effects/{effect}/retry", write(s.handleRetryEffect))
	// The draft's preview runs on sample rows the caller holds and writes nothing, so it is the
	// readers' route as far as the space goes; the app's roles say who may see a draft.
	mux.Handle("POST "+apps+"/{app}/preview", read(s.handlePreview))
	mux.Handle("POST "+apps+"/{app}/publish", write(s.handlePublish))
	mux.Handle("POST "+apps+"/{app}/rollback", write(s.handleRollback))
	mux.Handle("GET "+apps+"/{app}/releases", read(s.handleReleases))
	mux.Handle("GET "+apps+"/{app}/releases/{release}", read(s.handleRelease))
	mux.Handle("GET "+apps+"/{app}/records", read(s.handleRecords))
	mux.Handle("GET "+apps+"/{app}/records/deleted", read(s.handleDeleted))
	mux.Handle("GET "+apps+"/{app}/records/query", read(s.handleQuery))
	mux.Handle("GET "+apps+"/{app}/records/{record}/history", read(s.handleHistory))
	mux.Handle("POST "+apps+"/{app}/records/{record}/restore", write(s.handleRestore))
	// The live release and the caller's records, to keep on a device (offline.go).
	mux.Handle("GET "+apps+"/{app}/offline", read(s.handleOffline))
	// The integrations the app declares and the approval of each (integration_approvals.go): its
	// designers and publishers read them, and a publisher asks for a contract to be approved. The
	// admin's side is ApprovalRoutes.
	mux.Handle("GET "+apps+"/{app}/integrations", read(s.handleIntegrationStatus))
	mux.Handle("POST "+apps+"/{app}/integrations/{slot}/request", write(s.handleRequestApproval))
	mux.Handle("GET "+apps+"/{app}/roles", read(s.handleRoles))
	mux.Handle("PUT "+apps+"/{app}/roles", write(s.handleSetRoles))
	// Attachments (attachments.go): the uploads, and the photos kept for records.
	if s.dataDir != "" {
		s.attachmentRoutes(mux, read, write, apps)
	}
}

// maxBody bounds a request: a definition is at most MaxDefinitionSize, with room for the
// JSON around it.
const maxBody = MaxDefinitionSize + 16<<10

// request is what every app handler starts from: the space RequireSpace checked, the
// caller, and the app in the path, if the route has one.
type request struct {
	space access.Space
	actor Actor
	app   int64
}

func parse(w http.ResponseWriter, r *http.Request, withApp bool) (request, bool) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	req := request{space: sp, actor: Actor{UserID: u.ID}}
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
	// The space it goes to, as the caller sees it: one they can't read is not found.
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
		// A widget is drawn from the live release alone.
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
	// An action of a widget (?widget=) is answered with the rows a widget holds, not all. The
	// request is checked before the action runs: a refusal must mean nothing was done.
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
		// The screen sent something the app doesn't allow: the request was well formed.
		invalid(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// handlePreview draws the draft, or runs one of its actions, on sample rows (preview.go).
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

// handlePublish makes the app's draft its live release.
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

// handleRollback makes an earlier release the live one.
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

// handleRecords lists the app's live records the caller may read, with their provenance.
func (s *Service) handleRecords(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, false)
}

// handleDeleted lists the app's deleted records the caller may read.
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

// handleQuery answers a page of the app's records, filtered, searched and sorted, with the totals
// of all that match (query.go).
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

// recordID reads the record in the path; one that isn't a number is as unknown as any.
func recordID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("record"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
		return 0, false
	}
	return id, true
}

// handleHistory answers a record's versions, to whoever may read the record.
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

// handleRestore brings a deleted record back.
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
		// ExpectedRevision is the revision of the deleted record the caller saw.
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

// handleRoles answers the app's role assignments, to whoever manages them.
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

// rolesReply is an app's role assignments, with every role there is.
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

// result answers a change to an app: the app, or for ErrStale a 409 with the app as it is
// now, or the error's status.
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

// fail answers with the status for one of the package's errors, or 500.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var full *fullError
	var outdated *outdatedError
	var incompatible *incompatibleError
	var migration *migrationError
	var conflict *conflictError
	var status *statusError
	var apiErr *apierr.Error
	refusal, quotaRefusal := quota.AsAPI(err)
	switch {
	case quotaRefusal:
		apierr.WriteError(w, refusal)
	case errors.As(err, &status):
		writeError(w, status.status, status.code, status.msg, status.params)
	case errors.As(err, &apiErr):
		apierr.WriteError(w, apiErr)
	case errors.As(err, &conflict):
		// With the record as it is now, if the caller reads it, so they can reapply their change.
		body := map[string]any{"error": err.Error(), "code": apierr.CodeAppRecordConflict, "params": apierr.Params{"id": conflict.id}}
		if conflict.current != nil {
			body["current"] = conflict.current
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w) // the admin was disabled after the request was let in
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
		// Not reached: changes go through result, which sends the current app too.
		writeError(w, http.StatusConflict, apierr.CodeAppChanged, err.Error())
	case errors.As(err, &full):
		writeError(w, http.StatusConflict, full.code, err.Error(), full.params)
	case errors.Is(err, ErrInvalid):
		invalid(w, http.StatusBadRequest, err)
	default:
		if r.Context().Err() != nil {
			return // the client went away
		}
		slog.Error("apps: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apierr.Internal(w)
	}
}

// invalid answers a validation error with status, its code and its params.
func invalid(w http.ResponseWriter, status int, err error) {
	var bad *invalidError
	if !errors.As(err, &bad) {
		writeError(w, status, apierr.CodeBadRequest, err.Error())
		return
	}
	writeError(w, status, bad.code, bad.msg, bad.params)
}

// readJSON decodes the request body into v, strictly: unknown fields and trailing data
// are errors. It answers 400 and returns false on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

// readJSONLimit is readJSON for a body of up to limit bytes. A preview's body carries all its
// sample rows, so it is bigger than a definition's: its own limit, and its own answer to a body over it.
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

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200
// with a broken body.
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
