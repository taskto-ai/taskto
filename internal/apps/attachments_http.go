package apps

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
)

var RawBodyRoutes = []string{"PUT /api/workspaces/{workspace}/spaces/{space}/apps/{app}/uploads/{upload}"}

func (s *Service) attachmentRoutes(mux *http.ServeMux, read, write func(http.HandlerFunc) http.Handler, apps string) {
	mux.Handle("POST "+apps+"/{app}/uploads", write(s.handleUploadCreate))
	mux.Handle("GET "+apps+"/{app}/uploads/{upload}", read(s.handleUploadState))
	mux.Handle("PUT "+apps+"/{app}/uploads/{upload}", write(s.handleUploadChunk))
	mux.Handle("POST "+apps+"/{app}/uploads/{upload}/finalize", write(s.handleUploadFinalize))
	mux.Handle("DELETE "+apps+"/{app}/uploads/{upload}", write(s.handleUploadAbandon))
	mux.Handle("GET "+apps+"/{app}/attachments/{attachment}", read(s.handleAttachment))
	mux.Handle("GET "+apps+"/{app}/attachments/{attachment}/thumbnail", read(s.handleThumbnail))
}

func (s *Service) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var in NewUpload
	if !readJSON(w, r, &in) {
		return
	}
	up, err := s.createUpload(r.Context(), req.space, req.actor, req.app, in)
	if err != nil {
		s.failUpload(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, up)
}

func (s *Service) handleUploadState(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	up, err := s.uploadState(r.Context(), req.space, req.actor, req.app, r.PathValue("upload"))
	if err != nil {
		s.failUpload(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, up)
}

func (s *Service) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "offset is where this chunk starts, in bytes from 0")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, apierr.CodeContentType, "a chunk is sent as application/octet-stream")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxChunk))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, apierr.CodeAppUploadChunk, "a chunk is at most 2 MiB", apierr.Params{"max": MaxChunk})
		return
	case err != nil:
		return
	}
	up, err := s.putChunk(r.Context(), req.space, req.actor, req.app, r.PathValue("upload"), offset, data)
	if err != nil {
		s.failUpload(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, up)
}

func (s *Service) handleUploadFinalize(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var none struct{}
	if r.ContentLength != 0 && !readJSON(w, r, &none) {
		return
	}
	up, err := s.finalizeUpload(r.Context(), req.space, req.actor, req.app, r.PathValue("upload"))
	if err != nil {
		s.failUpload(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, up)
}

func (s *Service) handleUploadAbandon(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	if err := s.abandonUpload(r.Context(), req.space, req.actor, req.app, r.PathValue("upload")); err != nil {
		s.failUpload(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleAttachment(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, false)
}

func (s *Service) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, true)
}

func (s *Service) serveAttachment(w http.ResponseWriter, r *http.Request, thumb bool) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	f, err := s.openAttachment(r.Context(), req.space, req.actor, req.app, r.PathValue("attachment"), thumb)
	if err != nil {
		fail(w, r, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `inline; filename="`+f.Name+`"`)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", f.ETag)
	http.ServeContent(w, r, f.Name, fi.ModTime(), f)
}

func (s *Service) failUpload(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrInvalid) {
		invalid(w, http.StatusBadRequest, err)
		return
	}
	fail(w, r, err)
}
