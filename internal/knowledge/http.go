package knowledge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/quota"
)

var RawBodyRoutes = []string{"PUT " + spaceRoute + "/docs/{path...}"}

const spaceRoute = "/api/workspaces/{workspace}/spaces/{space}"

func (s *Store) Routes(mux *http.ServeMux, c *access.Checker) {
	read := func(h http.HandlerFunc) http.Handler { return auth.AcceptTokens(c.RequireSpace(access.Read, h)) }
	write := func(h http.HandlerFunc) http.Handler { return auth.AcceptTokens(c.RequireSpace(access.Write, h)) }
	const space = spaceRoute

	mux.Handle("GET "+space+"/docs", read(s.handleList))
	mux.Handle("GET "+space+"/docs/{path...}", read(s.handleRead))
	mux.Handle("PUT "+space+"/docs/{path...}", write(s.handlePut))
	mux.Handle("PATCH "+space+"/docs/{path...}", write(s.handleMove))
	mux.Handle("DELETE "+space+"/docs/{path...}", write(s.handleDelete))
	mux.Handle("GET "+space+"/history", read(s.handleHistory))
	mux.Handle("GET "+space+"/history/{path...}", read(s.handleHistory))
	mux.Handle("POST "+space+"/restore/{path...}", write(s.handleRestore))
	mux.Handle("DELETE "+space+"/history/{path...}", c.RequireSpaceAdmin(http.HandlerFunc(s.handleRemoveOldVersions)))
}

const maxBody = 64 << 10

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, ok := queryLimit(w, r)
	if !ok {
		return
	}
	page, err := s.List(r.Context(), sp, q.Get("prefix"), q.Get("after"), limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

type DocReply struct {
	Entry
	Content       *string `json:"content,omitempty"`
	ContentBase64 *string `json:"content_base64,omitempty"`
}

func (s *Store) handleRead(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	raw := false
	switch v := q.Get("raw"); v {
	case "", "false":
	case "true":
		raw = true
	default:
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `raw must be "true" or "false"`)
		return
	}
	if raw {
		s.serveRaw(w, r, sp, q.Get("at"))
		return
	}
	doc, err := s.Read(r.Context(), sp, r.PathValue("path"), q.Get("at"))
	if err != nil {
		fail(w, r, err)
		return
	}
	reply := DocReply{Entry: doc.Entry}
	switch {
	case !doc.Versioned:
	case IsMarkdown(doc.Path) && utf8.Valid(doc.Content):
		text := string(doc.Content)
		reply.Content = &text
	default:
		b64 := base64.StdEncoding.EncodeToString(doc.Content)
		reply.ContentBase64 = &b64
	}
	writeJSON(w, http.StatusOK, reply)
}

func (s *Store) serveRaw(w http.ResponseWriter, r *http.Request, sp access.Space, at string) {
	entry, f, err := s.Open(r.Context(), sp, r.PathValue("path"), at)
	if err != nil {
		fail(w, r, err)
		return
	}
	defer f.Close()
	ctype := MediaType(entry.Path)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(entry.Path)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Store) handlePut(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	who, ok := author(w, r)
	if !ok {
		return
	}
	name := r.PathValue("path")
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/octet-stream" {
		s.handlePutRaw(w, r, sp, who, name)
		return
	}
	var req struct {
		Content       *string `json:"content"`
		ContentBase64 *string `json:"content_base64"`
		BaseVersion   string  `json:"base_version"`
		Message       string  `json:"message"`
	}
	if !readJSON(w, r, &req, 2*s.opts.LargeFileSize+maxBody) {
		return
	}
	var content []byte
	switch {
	case IsMarkdown(name) && (req.Content == nil || req.ContentBase64 != nil):
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "a Markdown document takes its text in content")
		return
	case IsMarkdown(name):
		content = []byte(*req.Content)
	case req.ContentBase64 == nil || req.Content != nil:
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "a file other than Markdown takes its bytes in content_base64")
		return
	default:
		var err error
		if content, err = base64.StdEncoding.DecodeString(*req.ContentBase64); err != nil {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "content_base64 isn't valid base64")
			return
		}
	}
	res, err := s.Put(r.Context(), sp, name, content, req.BaseVersion, who, req.Message)
	if err != nil {
		fail(w, r, err)
		return
	}
	status := http.StatusOK
	if req.BaseVersion == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

func (s *Store) handlePutRaw(w http.ResponseWriter, r *http.Request, sp access.Space, who Author, name string) {
	q := r.URL.Query()
	for k := range q {
		if k != "base_version" && k != "message" {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, fmt.Sprintf("unknown parameter %q", k))
			return
		}
	}
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxFileSize+1)
	res, err := s.PutFile(r.Context(), sp, name, body, q.Get("base_version"), who, q.Get("message"))
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		err = errorf(apierr.CodeFileTooLarge, apierr.Params{"path": name, "max": s.opts.MaxFileSize},
			"%w: %s is over %d bytes", ErrTooLarge, name, s.opts.MaxFileSize)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	status := http.StatusOK
	if q.Get("base_version") == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

func (s *Store) handleMove(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	who, ok := author(w, r)
	if !ok {
		return
	}
	var req struct {
		To          string `json:"to"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if !readJSON(w, r, &req, maxBody) {
		return
	}
	res, err := s.Move(r.Context(), sp, r.PathValue("path"), req.To, req.BaseVersion, who, req.Message)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Store) handleDelete(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	who, ok := author(w, r)
	if !ok {
		return
	}
	var req struct {
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if !readOptionalJSON(w, r, &req, maxBody) {
		return
	}
	res, err := s.Delete(r.Context(), sp, r.PathValue("path"), req.BaseVersion, who, req.Message)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Store) handleHistory(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	limit, ok := queryLimit(w, r)
	if !ok {
		return
	}
	h, err := s.History(r.Context(), sp, r.PathValue("path"), r.URL.Query().Get("before"), limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Store) handleRestore(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	who, ok := author(w, r)
	if !ok {
		return
	}
	var req struct {
		Commit      string `json:"commit"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if !readJSON(w, r, &req, maxBody) {
		return
	}
	res, err := s.Restore(r.Context(), sp, r.PathValue("path"), req.Commit, req.BaseVersion, who, req.Message)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Store) handleRemoveOldVersions(w http.ResponseWriter, r *http.Request) {
	sp, ok := space(w, r)
	if !ok {
		return
	}
	who, ok := author(w, r)
	if !ok {
		return
	}
	var req struct {
		BaseVersion string `json:"base_version"`
	}
	if !readOptionalJSON(w, r, &req, maxBody) {
		return
	}
	name := r.PathValue("path")
	res, err := s.RemoveOldVersions(r.Context(), sp, name, req.BaseVersion, who)
	if err != nil {
		fail(w, r, err)
		return
	}
	u, _ := auth.UserFrom(r.Context())
	slog.InfoContext(r.Context(), "knowledge: an admin removed a file's old versions from history",
		"user_id", u.ID, "workspace_id", sp.WorkspaceID, "space", sp.ID, "path", name, "versions", res.Removed, "commit", res.Commit)
	writeJSON(w, http.StatusOK, res)
}

func space(w http.ResponseWriter, r *http.Request) (access.Space, bool) {
	sp, ok := access.SpaceFrom(r.Context())
	if !ok {
		slog.Error("knowledge: no space on the request: route mounted without RequireSpace", "method", r.Method, "path", r.URL.Path)
		apierr.Internal(w)
		return access.Space{}, false
	}
	return sp, true
}

func author(w http.ResponseWriter, r *http.Request) (Author, bool) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return Author{}, false
	}
	by := Author{Name: u.Name, Email: u.Email}
	if t, ok := auth.TokenFrom(r.Context()); ok {
		by.Agent = auth.TokenAgent(t.Name)
	}
	return by, true
}

func queryLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "limit must be a positive number")
		return 0, false
	}
	return n, true
}

func Fail(w http.ResponseWriter, r *http.Request, err error) { fail(w, r, err) }

func fail(w http.ResponseWriter, r *http.Request, err error) {
	if refusal := (*apierr.Error)(nil); errors.As(err, &refusal) {
		apierr.WriteError(w, refusal)
		return
	}
	var status int
	var code apierr.Code
	msg := err.Error()
	if refusal, ok := quota.AsAPI(err); ok {
		apierr.WriteError(w, refusal)
		return
	}
	switch {
	case errors.Is(err, ErrInvalid):
		status, code = http.StatusBadRequest, apierr.CodeBadRequest
	case errors.Is(err, access.ErrNotFound):
		status, code, msg = http.StatusNotFound, apierr.CodeSpaceNotFound, access.ErrNotFound.Error()
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, apierr.CodeDocNotFound
	case errors.Is(err, ErrReadOnly):
		status, code = http.StatusForbidden, apierr.CodeSpaceReadOnly
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, apierr.CodeDocConflict
	case errors.Is(err, ErrTooLarge):
		status, code = http.StatusRequestEntityTooLarge, apierr.CodeFileTooLarge
	case errors.Is(err, ErrRepoFull):
		status, code = http.StatusInsufficientStorage, apierr.CodeKnowledgeFull
	case errors.Is(err, ErrNotKept):
		status, code = http.StatusGone, apierr.CodeDocNotKept
	default:
		if r.Context().Err() != nil {
			return
		}
		slog.Error("knowledge: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apierr.Internal(w)
		return
	}
	var params apierr.Params
	if c := (*codedError)(nil); errors.As(err, &c) {
		code, params = c.code, c.params
	}
	writeError(w, status, code, msg, params)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	return decodeJSON(w, r, v, limit, false)
}

func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	return decodeJSON(w, r, v, limit, true)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if optional && errors.Is(err, io.EOF) {
		return true
	}
	if err == nil {
		if _, err = dec.Token(); errors.Is(err, io.EOF) {
			return true
		}
		err = errors.New("one object expected")
	}
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, apierr.CodeRequestTooLarge,
			fmt.Sprintf("the request body is over %d bytes", limit), apierr.Params{"max": limit})
		return false
	}
	writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("knowledge: encode response", "err", err)
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
