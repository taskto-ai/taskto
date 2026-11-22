// Package apierr writes the API's errors: {"error": "…", "code": "…", "params": {…}}.
//
// The code is stable, one of the constants in codes.go, and is what the web app translates
// (web/src/i18n/locales/*.json, errors.<code>) and what a client may branch on; the error
// is English, for logs and people reading the API directly; params fill the placeholders
// of the translated message ({max} in "at most {max} characters"). docs/api.md lists the
// codes. CI fails if the web app's English has no message for one (web/scripts/i18n-check.mjs).
package apierr

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Code is an error's stable code. Use the constants: a code is never made from a string
// (TestCodesAreConstants).
type Code string

// Params fill in a translated message's placeholders. Values are strings or numbers.
type Params map[string]any

type body struct {
	Error  string `json:"error"`
	Code   Code   `json:"code"`
	Params Params `json:"params,omitempty"`
}

// Write answers with status and the error: msg in English, code, and params if any (several
// maps are merged, a later one winning).
func Write(w http.ResponseWriter, status int, code Code, msg string, params ...Params) {
	b := body{Error: msg, Code: code, Params: merge(params)}
	out, err := json.Marshal(b)
	if err != nil {
		slog.Error("apierr: encode", "err", err)
		status, out = http.StatusInternalServerError, []byte(`{"error":"internal error","code":"internal"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(out, '\n'))
}

// Error is an error a package returns with its API answer attached, for a handler to
// Write as it is (see As).
type Error struct {
	Status int
	Code   Code
	Msg    string
	Params Params
}

func (e *Error) Error() string { return e.Msg }

// New returns an *Error.
func New(status int, code Code, msg string, params ...Params) *Error {
	return &Error{Status: status, Code: code, Msg: msg, Params: merge(params)}
}

// WriteError writes e.
func WriteError(w http.ResponseWriter, e *Error) {
	Write(w, e.Status, e.Code, e.Msg, e.Params)
}

// merge returns the params as one map, or nil if there are none.
func merge(params []Params) Params {
	var out Params
	for _, p := range params {
		for k, v := range p {
			if out == nil {
				out = Params{}
			}
			out[k] = v
		}
	}
	return out
}

// Internal answers 500 without details, which belong in the server's log.
func Internal(w http.ResponseWriter) {
	Write(w, http.StatusInternalServerError, CodeInternal, "internal error")
}
