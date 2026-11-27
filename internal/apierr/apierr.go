package apierr

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type Code string

type Params map[string]any

type body struct {
	Error  string `json:"error"`
	Code   Code   `json:"code"`
	Params Params `json:"params,omitempty"`
}

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

type Error struct {
	Status int
	Code   Code
	Msg    string
	Params Params
}

func (e *Error) Error() string { return e.Msg }

func New(status int, code Code, msg string, params ...Params) *Error {
	return &Error{Status: status, Code: code, Msg: msg, Params: merge(params)}
}

func WriteError(w http.ResponseWriter, e *Error) {
	Write(w, e.Status, e.Code, e.Msg, e.Params)
}

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

func Internal(w http.ResponseWriter) {
	Write(w, http.StatusInternalServerError, CodeInternal, "internal error")
}
