package whiteboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/live"
	"taskto.ai/taskto/internal/whiteboard/shapes"
)

const (
	maxBody  = 48 << 10
	opsTail  = "/ops"
	liveTail = "/live"
)

type OpsRequest struct {
	Ops []shapes.Op `json:"ops"`
}

type OpsReply struct {
	Seq int64 `json:"seq"`
}

func (s *Service) Routes(mux *http.ServeMux, c *access.Checker) {
	const route = "/api/workspaces/{workspace}/spaces/{space}/whiteboards/{path...}"
	mux.Handle("GET "+route, c.RequireSpace(access.Read, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleGet(w, r, c) })))
	mux.Handle("POST "+route, c.RequireSpace(access.Write, http.HandlerFunc(s.handlePost)))
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request, c *access.Checker) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	path, isLive := strings.CutSuffix(r.PathValue("path"), liveTail)
	if !shapes.IsPath(path) {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "not a whiteboard")
		return
	}
	if !isLive {
		snap, err := s.Snapshot(r.Context(), sp, path)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, snap)
		return
	}
	b, err := s.board(r.Context(), sp, path)
	if err != nil {
		fail(w, r, err)
		return
	}
	spaceCheck := live.RecheckSpace(c, u.ID, sp)
	s.hub.Serve(w, r, b.topic(), u.ID, func(ctx context.Context) error {
		if err := spaceCheck(ctx); err != nil {
			return err
		}
		return s.stillThere(ctx, b)
	})
}

func (s *Service) handlePost(w http.ResponseWriter, r *http.Request) {
	sp, _ := access.SpaceFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	path, isOps := strings.CutSuffix(r.PathValue("path"), opsTail)
	if !isOps || !shapes.IsPath(path) {
		apierr.Write(w, http.StatusNotFound, apierr.CodeDocNotFound, "not a whiteboard route")
		return
	}
	var req OpsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeRequestTooLarge,
			fmt.Sprintf("the request body is over %d bytes", maxBody), apierr.Params{"max": maxBody})
		return
	}
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return
	}
	seq, err := s.Apply(r.Context(), u.ID, sp, path, req.Ops)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, OpsReply{Seq: seq})
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *apierr.Error
	switch {
	case errors.As(err, &refusal):
		apierr.WriteError(w, refusal)
	case errors.Is(err, access.ErrNotFound):
		apierr.Write(w, http.StatusNotFound, apierr.CodeSpaceNotFound, access.ErrNotFound.Error())
	default:
		knowledge.Fail(w, r, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
