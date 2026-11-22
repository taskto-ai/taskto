package boards

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// Routes mounts the board endpoints on mux, each behind access.Checker.RequireSpace.
// docs/api.md describes them.
func (s *Service) Routes(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.Handler { return auth.AcceptTokens(s.access.RequireSpace(access.Read, h)) }
	write := func(h http.HandlerFunc) http.Handler {
		return auth.AcceptTokens(s.access.RequireSpace(access.Write, h))
	}
	const boards = "/api/workspaces/{workspace}/spaces/{space}/boards"
	const cards = boards + "/{board}/cards"

	mux.Handle("GET "+boards, read(s.handleListBoards))
	mux.Handle("POST "+boards, write(s.handleCreateBoard))
	mux.Handle("GET "+boards+"/{board}", read(s.handleGetBoard))
	mux.Handle("PATCH "+boards+"/{board}", write(s.handleUpdateBoard))
	mux.Handle("GET "+cards, read(s.handleListCards))
	mux.Handle("POST "+cards, write(s.handleCreateCard))
	mux.Handle("GET "+cards+"/{card}", read(s.handleGetCard))
	mux.Handle("PATCH "+cards+"/{card}", write(s.handleUpdateCard))
	mux.Handle("POST "+cards+"/{card}/move", write(s.handleMoveCard))
	mux.Handle("POST "+cards+"/{card}/archive", write(s.handleArchiveCard))
	mux.Handle("POST "+cards+"/{card}/restore", write(s.handleRestoreCard))
	mux.Handle("POST "+cards+"/{card}/comments", write(s.handleComment))
}

const (
	maxBody     = 64 << 10
	maxQueryLen = 200
)

// request is what every board handler starts from: the space RequireSpace checked, the
// caller, and the board and card in the path, if the route has them.
type request struct {
	space access.Space
	actor Actor
	board int64
	card  int64
}

// parse reads the request's space and caller, and the path IDs it names, answering 404
// for a path ID that isn't a number.
func parse(w http.ResponseWriter, r *http.Request, ids ...string) (request, bool) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	req := request{space: sp, actor: Actor{UserID: u.ID}}
	if t, ok := auth.TokenFrom(r.Context()); ok {
		// Changes made with an access token are marked like the agent's, with its name.
		req.actor.Agent, req.actor.Via = true, t.Name
	}
	for _, name := range ids {
		id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
			return request{}, false
		}
		switch name {
		case "board":
			req.board = id
		case "card":
			req.card = id
		}
	}
	return req, true
}

func (s *Service) handleListBoards(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r)
	withArchived, ok := boolParam(w, r, "archived")
	if !ok {
		return
	}
	all, err := s.listBoards(r.Context(), req.space, withArchived != nil && *withArchived)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Board{"boards": all})
}

func (s *Service) handleCreateBoard(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r)
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	b, err := s.createBoard(r.Context(), req.space, req.actor, body.Name)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Service) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board")
	if !ok {
		return
	}
	b, err := s.getBoard(r.Context(), req.space, req.board)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Service) handleUpdateBoard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board")
	if !ok {
		return
	}
	var ch BoardChanges
	if !readJSON(w, r, &ch) {
		return
	}
	b, err := s.updateBoard(r.Context(), req.space, req.actor, req.board, ch)
	if errors.Is(err, ErrStale) {
		current, cerr := board(r.Context(), s.db, req.space.ID, req.board)
		stale(w, r, apierr.CodeBoardChanged, current, cerr)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Service) handleListCards(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board")
	if !ok {
		return
	}
	q := r.URL.Query()
	f := Filter{Column: Column(q.Get("column")), Text: q.Get("q")}
	if utf8.RuneCountInString(f.Text) > maxQueryLen {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "q is too long")
		return
	}
	switch v := q.Get("assignee"); v {
	case "":
	case "me":
		f.Assignee, f.AssigneeID = AssignUser, req.actor.UserID
	case string(AssignAgent), string(Unassigned):
		f.Assignee = AssigneeKind(v)
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `assignee must be "me", "agent", "none" or a user ID`)
			return
		}
		f.Assignee, f.AssigneeID = AssignUser, id
	}
	if f.Blocked, ok = boolParam(w, r, "blocked"); !ok {
		return
	}
	cards, err := s.listCards(r.Context(), req.space, req.board, f)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Card{"cards": cards})
}

func (s *Service) handleCreateCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board")
	if !ok {
		return
	}
	var nc NewCard
	if !readJSON(w, r, &nc) {
		return
	}
	c, err := s.createCard(r.Context(), req.space, req.actor, req.board, nc)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Service) handleGetCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	t, err := s.getThread(r.Context(), req.space, req.board, req.card)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Service) handleUpdateCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	var ch CardChanges
	if !readJSON(w, r, &ch) {
		return
	}
	c, err := s.updateCard(r.Context(), req.space, req.actor, req.board, req.card, ch)
	s.cardResult(w, r, req, c, err)
}

func (s *Service) handleMoveCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	var m Move
	if !readJSON(w, r, &m) {
		return
	}
	c, err := s.moveCard(r.Context(), req.space, req.actor, req.board, req.card, m)
	s.cardResult(w, r, req, c, err)
}

func (s *Service) handleArchiveCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	var body struct {
		Version int64 `json:"version"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	c, err := s.archiveCard(r.Context(), req.space, req.actor, req.board, req.card, body.Version)
	s.cardResult(w, r, req, c, err)
}

func (s *Service) handleRestoreCard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	var body struct {
		Version int64  `json:"version"`
		Column  Column `json:"column"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	c, err := s.restoreCard(r.Context(), req.space, req.actor, req.board, req.card, body.Version, body.Column)
	s.cardResult(w, r, req, c, err)
}

func (s *Service) handleComment(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, "board", "card")
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	cm, err := s.comment(r.Context(), req.space, req.actor, req.board, req.card, body.Body)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cm)
}

// cardResult answers a change to a card: the card, or for ErrStale a 409 with the card as
// it is now, or the error's status.
func (s *Service) cardResult(w http.ResponseWriter, r *http.Request, req request, c Card, err error) {
	switch {
	case errors.Is(err, ErrStale):
		current, cerr := card(r.Context(), s.db, req.space.ID, req.board, req.card)
		stale(w, r, apierr.CodeCardChanged, current, cerr)
	case err != nil:
		fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, c)
	}
}

// stale answers 409 for an update made against an old version, with code and the current
// state so the client can redo it; lookupErr is the error of reading that state.
func stale(w http.ResponseWriter, r *http.Request, code apierr.Code, current any, lookupErr error) {
	if lookupErr != nil {
		fail(w, r, lookupErr)
		return
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": ErrStale.Error(), "code": code, "current": current})
}

// fail answers with the status for one of the package's errors, or 500.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var bad *invalidError
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
	case errors.Is(err, errSpaceArchived):
		writeError(w, http.StatusForbidden, apierr.CodeSpaceArchived, err.Error())
	case errors.Is(err, ErrReadOnly):
		writeError(w, http.StatusForbidden, apierr.CodeSpaceReadOnly, err.Error())
	case errors.Is(err, ErrBoardArchived):
		writeError(w, http.StatusForbidden, apierr.CodeBoardArchived, err.Error())
	case errors.Is(err, ErrStale):
		// Not reached: changes go through stale, which sends the current state too.
		writeError(w, http.StatusConflict, apierr.CodeCardChanged, err.Error())
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.code, bad.msg, bad.params)
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, err.Error())
	default:
		if r.Context().Err() != nil {
			return // the client went away
		}
		slog.Error("boards: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apierr.Internal(w)
	}
}

// boolParam reads an optional "true" or "false" query parameter, answering 400 for
// anything else.
func boolParam(w http.ResponseWriter, r *http.Request, name string) (*bool, bool) {
	switch r.URL.Query().Get(name) {
	case "":
		return nil, true
	case "true":
		v := true
		return &v, true
	case "false":
		v := false
		return &v, true
	}
	writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, name+` must be "true" or "false"`)
	return nil, false
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
		slog.Error("boards: encode response", "err", err)
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
