package dictation

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/providers"
)

// Routes mounts the dictation endpoints on mux, behind a's access checks. c finds the spaces
// whose glossaries a dictation may use: those its caller can read.
func (s *Service) Routes(mux *http.ServeMux, a *auth.Service, c *access.Checker) {
	member := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	s.language = a.Language
	mux.Handle("GET /api/workspaces/{workspace}/dictation", member(s.handleStatus))
	mux.Handle("PATCH /api/workspaces/{workspace}/dictation", a.RequireAdmin(http.HandlerFunc(s.handleUpdate)))
	mux.Handle("POST /api/workspaces/{workspace}/dictation/warm", member(s.handleWarm))
	mux.Handle("POST /api/workspaces/{workspace}/dictation/transcriptions", member(s.handleTranscribe(c)))
}

type apiReply struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"` // "set" or "not set", never the key
}

type settingsReply struct {
	Engine   string   `json:"engine"`
	Language string   `json:"language"`
	API      apiReply `json:"api"`
	// AcceptedNotice is the add-on whose notice an admin confirmed, or "".
	AcceptedNotice string `json:"accepted_notice"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type localReply struct {
	Model string `json:"model"`
	LocalState
}

type addonReply struct {
	Addon
	Installed bool `json:"installed"`
}

type statusReply struct {
	// Engine is what a dictation uses now ("api", "local", an add-on's ID), or "" for none.
	Engine string `json:"engine"`
	// Reason says why there is none, or why it can't run yet; ReasonCode and ReasonParams
	// are its code, for the web app to translate (apierr).
	Reason       string        `json:"reason,omitempty"`
	ReasonCode   apierr.Code   `json:"reason_code,omitempty"`
	ReasonParams apierr.Params `json:"reason_params,omitempty"`
	Settings     settingsReply `json:"settings"`
	// Local is the operator's local engine, or null when it is off.
	Local *localReply `json:"local"`
	// Addons are the add-ons taskto knows of, and whether the operator installed them.
	Addons     []addonReply `json:"addons"`
	Credits    []Credit     `json:"credits"`
	MaxSeconds int          `json:"max_seconds"`
}

func (s *Service) status(ctx context.Context, workspaceID int64) (statusReply, error) {
	st, err := s.load(ctx, workspaceID)
	if err != nil {
		return statusReply{}, err
	}
	keyState := "not set"
	if st.sealed != nil {
		keyState = "set"
	}
	reply := statusReply{
		Settings: settingsReply{
			Engine: st.engine, Language: st.language,
			API:            apiReply{BaseURL: st.baseURL, Model: st.model, APIKey: keyState},
			AcceptedNotice: st.notice, UpdatedAt: st.updatedAt,
		},
		Credits:    []Credit{},
		Addons:     []addonReply{},
		MaxSeconds: int(MaxDuration / time.Second),
	}
	for _, a := range Addons {
		_, installed := s.addons[a.ID]
		reply.Addons = append(reply.Addons, addonReply{Addon: a, Installed: installed})
	}
	if s.local != nil {
		reply.Local = &localReply{Model: s.local.Model(), LocalState: s.local.State()}
	}
	name, _, err := s.engine(workspaceID, st)
	if u, ok := unavailable(err); ok {
		reply.Reason, reply.ReasonCode, reply.ReasonParams = u.Reason, u.Code, u.Params
		return reply, nil
	}
	if err != nil {
		return statusReply{}, err
	}
	reply.Engine = name
	if a, ok := s.addons[name]; ok {
		reply.Credits = append(reply.Credits, a.Attribution)
	}
	if name == EngineLocal {
		reply.Credits = append(reply.Credits, s.local.Credits()...)
		if ls := reply.Local.LocalState; ls.State != LocalReady {
			u := localReason(ls)
			reply.Reason, reply.ReasonCode, reply.ReasonParams = u.Reason, u.Code, u.Params
		}
	}
	return reply, nil
}

// localReason says why the local engine can't run yet.
func localReason(ls LocalState) *Unavailable {
	switch ls.State {
	case LocalNotDownloaded:
		return &Unavailable{
			Reason: "the local speech model isn't downloaded yet: it downloads on first use",
			Code:   apierr.CodeDictationLocalNotDownloaded,
		}
	case LocalDownloading:
		percent := int(ls.Progress * 100)
		return &Unavailable{
			Reason: fmt.Sprintf("the local speech model is downloading (%d%%)", percent),
			Code:   apierr.CodeDictationLocalDownloading, Params: apierr.Params{"percent": percent},
		}
	}
	if ls.Error != "" {
		// Every state with an error sets its code (local.go); the fallback keeps a new one
		// from reaching the web app without one.
		return &Unavailable{Reason: ls.Error, Code: cmp.Or(ls.ErrorCode, apierr.CodeDictationLocalUnavailable), Params: ls.ErrorParams}
	}
	return &Unavailable{Reason: "the local speech model can't run on this server", Code: apierr.CodeDictationLocalUnavailable}
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	m, _ := auth.MembershipFrom(r.Context())
	reply, err := s.status(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// optional is a field that may be absent, null or a value.
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

// updateRequest is the body of a PATCH: an absent field stays as it is. api.api_key: a
// string sets a new key, null removes it.
type updateRequest struct {
	Engine   *string `json:"engine"`
	Language *string `json:"language"`
	API      *struct {
		BaseURL *string          `json:"base_url"`
		Model   *string          `json:"model"`
		APIKey  optional[string] `json:"api_key"`
	} `json:"api"`
	// AcceptNotice confirms an add-on's notice, which choosing it as the engine needs.
	AcceptNotice *string `json:"accept_notice"`
}

var languageRe = regexp.MustCompile(`^[a-z]{2,3}$`)

const maxModelLen = 100

// badRequest is a refused setting, a 400 with code: its message is safe to show and never
// quotes a value.
func badRequest(code apierr.Code, msg string, params ...apierr.Params) *apierr.Error {
	return apierr.New(http.StatusBadRequest, code, msg, params...)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	var req updateRequest
	if !readJSON(w, r, &req, maxRequest) {
		return
	}
	// One change at a time, so two admins' changes don't overwrite each other's fields, and
	// the stored key's host is checked against what is stored. One server per data directory
	// (AGENTS.md) makes an in-process lock enough.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	newKey, noticeBy, err := s.apply(r.Context(), &st, req)
	var bad *apierr.Error
	if errors.As(err, &bad) {
		apierr.WriteError(w, bad)
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if err := s.save(r.Context(), m.WorkspaceID, u.ID, st, newKey, noticeBy); err != nil {
		serverError(w, r, err)
		return
	}
	slog.Info("dictation settings changed", "workspace", m.WorkspaceID, "by", u.ID, "engine", st.engine,
		"api_key_changed", newKey != nil)
	reply, err := s.status(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// apply checks req and applies it to st. It returns the new key to store (nil: keep the
// stored one; "": remove it), and whether an add-on's notice was newly confirmed.
func (s *Service) apply(ctx context.Context, st *settings, req updateRequest) (*string, bool, error) {
	if req.Language != nil {
		if l := *req.Language; l != "" && !languageRe.MatchString(l) {
			return nil, false, badRequest(apierr.CodeDictationLanguageInvalid, errLanguage)
		}
		st.language = *req.Language
	}
	var newKey *string
	if a := req.API; a != nil {
		if a.Model != nil {
			model := strings.TrimSpace(*a.Model)
			if model == "" || utf8.RuneCountInString(model) > maxModelLen || strings.ContainsFunc(model, unicode.IsControl) {
				return nil, false, badRequest(apierr.CodeDictationModelInvalid,
					fmt.Sprintf("api.model must be 1 to %d printable characters", maxModelLen), apierr.Params{"max": maxModelLen})
			}
			st.model = model
		}
		baseURL := st.baseURL
		if a.BaseURL != nil {
			baseURL = *a.BaseURL
		}
		hasKey := st.sealed != nil
		if a.APIKey.set {
			hasKey = a.APIKey.value != nil
		}
		normalized, key, err := providers.CheckEndpoint(baseURL, a.APIKey.value, hasKey)
		if bad := (*apierr.Error)(nil); errors.As(err, &bad) {
			// The same refusal, for the field under api.
			e := *bad
			e.Msg = "api." + e.Msg
			return nil, false, &e
		}
		if err != nil {
			return nil, false, err
		}
		// A stored key goes only where it was set for: a new host or port needs it sent again.
		if st.sealed != nil && !a.APIKey.set && hostOf(normalized) != hostOf(st.baseURL) {
			return nil, false, badRequest(apierr.CodeDictationKeyRequired, "api.api_key must be sent again to change api.base_url's host")
		}
		// Checked whenever the API's settings change, the default endpoint included; every
		// connection is checked again anyway.
		if err := s.checkEgress(ctx, hostname(normalized)); err != nil {
			if refused := egress.Refusal(err); refused != nil {
				return nil, false, refused.APIError("api.base_url")
			}
			return nil, false, err
		}
		st.baseURL = normalized
		if a.APIKey.set {
			empty := ""
			newKey = &empty
			if key != nil {
				newKey = key
			}
		}
	}
	noticeBy := false
	if req.AcceptNotice != nil {
		if _, ok := s.addons[*req.AcceptNotice]; !ok {
			return nil, false, badRequest(apierr.CodeBadRequest, "accept_notice must name an add-on installed on this server")
		}
		st.notice, noticeBy = *req.AcceptNotice, true
	}
	if req.Engine != nil {
		switch e := *req.Engine; e {
		case EngineAPI:
		case EngineLocal:
			// Refused only as a change: local is the default, so a form saved unchanged on a
			// server whose local model is off sends it back.
			if s.local == nil && st.engine != EngineLocal {
				return nil, false, badRequest(apierr.CodeDictationLocalOff, "engine: the server's local speech model is off (dictation_local)")
			}
		default:
			if _, ok := s.addons[e]; !ok {
				return nil, false, badRequest(apierr.CodeDictationEngineUnavailable, "engine must be api, local or an add-on installed on this server")
			}
			// An add-on needs its notice confirmed first: in this request or an earlier one.
			if st.notice != e {
				return nil, false, badRequest(apierr.CodeDictationNoticeRequired, "engine: confirm the add-on's notice first (accept_notice)")
			}
		}
		st.engine = *req.Engine
	}
	return newKey, noticeBy, nil
}

// hostOf is where a base URL sends its requests, as compared for the stored key: the host,
// lower-cased and without a trailing dot, and the port, the scheme's by default.
func hostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(hostname(baseURL), port)
}

// hostname is a base URL's host name, lower-cased and without a trailing dot.
func hostname(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func (s *Service) handleWarm(w http.ResponseWriter, r *http.Request) {
	m, _ := auth.MembershipFrom(r.Context())
	st, err := s.load(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	// Only a workspace that uses the local engine starts its download.
	if name, _, err := s.engine(m.WorkspaceID, st); err == nil && name == EngineLocal {
		s.local.Warm()
	}
	reply, err := s.status(r.Context(), m.WorkspaceID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// maxAudioRequest is room for ten minutes and a few seconds of WAV, base64-encoded.
const maxAudioRequest = 28 << 20

type transcribeReply struct {
	Text   string `json:"text"`
	Engine string `json:"engine"`
}

func (s *Service) handleTranscribe(c *access.Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		m, _ := auth.MembershipFrom(r.Context())
		// Slots before the body is read, so the body's memory is bounded too: one per person,
		// so nobody holds the server's few, then one of the server's.
		if !s.claim(u.ID) {
			apierr.Write(w, http.StatusTooManyRequests, apierr.CodeDictationBusy, "you are already dictating: wait for that one to finish")
			return
		}
		defer s.release(u.ID)
		select {
		case s.busy <- struct{}{}:
			defer func() { <-s.busy }()
		default:
			apierr.Write(w, http.StatusTooManyRequests, apierr.CodeDictationServerBusy, "the server is transcribing too many dictations at once: try again in a moment")
			return
		}
		var req struct {
			AudioBase64 string  `json:"audio_base64"`
			Language    *string `json:"language"`
			// BrowserLocale is the browser's language, for a user who picked none.
			BrowserLocale string `json:"browser_locale"`
		}
		// The upload has a deadline of its own, so a slow one can't hold a slot for long.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
		if !readJSON(w, r, &req, maxAudioRequest) {
			return
		}
		if !auth.ValidLocale(req.BrowserLocale) {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, `browser_locale must be a BCP 47 tag such as "pt-BR", or ""`)
			return
		}
		audio, err := decodeAudio(req.AudioBase64)
		req.AudioBase64 = "" // the encoded copy can go before the long call
		var bad *errAudio
		if errors.As(err, &bad) {
			apierr.Write(w, http.StatusBadRequest, bad.code, bad.msg, bad.params)
			return
		}
		st, err := s.load(r.Context(), m.WorkspaceID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		opts := Options{Language: st.language}
		if req.Language != nil {
			if l := *req.Language; l != "" && !languageRe.MatchString(l) {
				apierr.Write(w, http.StatusBadRequest, apierr.CodeDictationLanguageInvalid, errLanguage)
				return
			}
			opts.Language = *req.Language
		}
		if req.Language == nil && opts.Language == "" && s.language != nil {
			// The workspace sets no language: the user's own is the likeliest one they speak
			// (docs/i18n.md#the-agent-and-dictation). An engine that doesn't know it detects the language.
			opts.Language = primaryLanguage(s.language(r, req.BrowserLocale))
		}
		name, engine, err := s.engine(m.WorkspaceID, st)
		if un, ok := unavailable(err); ok {
			writeUnavailable(w, un)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		terms := glossary(r.Context(), c, s.docs, u.ID, m.WorkspaceID)
		if name != EngineAPI {
			opts.Terms = terms // engines on this server only: see Options.Terms
		}
		ctx, cancel := context.WithTimeout(r.Context(), transcribeTimeout)
		defer cancel()
		started := time.Now()
		text, err := engine.Transcribe(ctx, audio, opts)
		if un, ok := unavailable(err); ok {
			writeUnavailable(w, un)
			return
		}
		if err != nil {
			if r.Context().Err() != nil {
				return // the client went away
			}
			slog.Warn("dictation: transcription failed", "workspace", m.WorkspaceID, "engine", name,
				"audio_seconds", int(audio.Duration()/time.Second), "err", err)
			status, code, msg := http.StatusBadGateway, apierr.CodeDictationFailed, "the speech-to-text engine failed; the server log has the details"
			switch refused := egress.Refusal(err); {
			case refused != nil:
				apierr.Write(w, status, apierr.CodeEgressRefused, "the speech-to-text API's endpoint is refused by the server's egress policy",
					apierr.Params{"host": refused.Host})
				return
			case errors.Is(err, context.DeadlineExceeded):
				status, code, msg = http.StatusGatewayTimeout, apierr.CodeDictationTimeout, "the transcription took too long; try again, or with a shorter recording"
			}
			apierr.Write(w, status, code, msg)
			return
		}
		slog.Debug("dictation", "workspace", m.WorkspaceID, "engine", name,
			"audio_seconds", int(audio.Duration()/time.Second), "took", time.Since(started))
		writeJSON(w, http.StatusOK, transcribeReply{Text: Correct(text, terms), Engine: name})
	}
}

// maxRequest is the largest settings body.
const maxRequest = 64 << 10

// readJSON decodes the request body into v strictly, answering 400 or 413 and returning
// false on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, err = dec.Token(); errors.Is(err, io.EOF) {
			return true
		}
		err = errors.New("one object expected")
	}
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeInvalidJSON, fmt.Sprintf("the request body is over %d bytes", limit))
		return false
	}
	apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
	return false
}

// serverError logs err and answers 500 without details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("dictation: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

// writeJSON encodes before writing, so an encoding failure is a 500 rather than a 200
// with a broken body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("dictation: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// writeUnavailable answers 503 with why the engine can't transcribe now.
func writeUnavailable(w http.ResponseWriter, u *Unavailable) {
	apierr.Write(w, http.StatusServiceUnavailable, cmp.Or(u.Code, apierr.CodeDictationEngineUnavailable), u.Reason, u.Params)
}

const errLanguage = `language must be an ISO 639-1 code such as "pt", or ""`

// decodeAudio reads a base64 WAV. Only the samples outlive it.
func decodeAudio(b64 string) (Audio, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Audio{}, &errAudio{code: apierr.CodeAudioInvalid, msg: "audio_base64 isn't valid base64"}
	}
	audio, err := ParseWAV(raw)
	if err == nil && len(audio.Samples) == 0 {
		err = errEmpty
	}
	return audio, err
}

// primaryLanguage is tag's language alone, the form engines take: "pt" for "pt-BR".
func primaryLanguage(tag string) string {
	lang, _, _ := strings.Cut(tag, "-")
	return lang
}
