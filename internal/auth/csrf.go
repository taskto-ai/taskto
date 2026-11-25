package auth

import (
	"mime"
	"net/http"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

// CSRF wraps the whole server. On /api paths it refuses requests with a state-changing
// method (anything but GET, HEAD and OPTIONS) that
//
//   - a browser marks as cross-origin, by Sec-Fetch-Site or, in older browsers, an Origin
//     that isn't the Host (403); or
//   - carry a body that isn't Content-Type: application/json (415). A cross-site HTML form
//     can only send form or text types, and a script on another site can't set JSON
//     without a CORS preflight, which this server never grants. A request without a body
//     (logout, DELETE) needs no type; the cross-origin check above still applies to it.
//     The routes named in rawBody (ServeMux patterns, such as an upload of a file's bytes)
//     take application/octet-stream as well, which no cross-site form can send either and a
//     script on another site can't set without a preflight.
//
// Either check alone stops a cross-site request riding on the session cookie; both
// together also cover browsers without Sec-Fetch-Site. Requests from non-browser clients,
// which send neither header, pass the first check. Safe methods must never change state.
func CSRF(next http.Handler, rawBody ...string) http.Handler {
	raw := http.NewServeMux()
	for _, pattern := range rawBody {
		raw.Handle(pattern, next)
	}
	isRaw := func(r *http.Request) bool {
		_, pattern := raw.Handler(r)
		return pattern != ""
	}
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, apierr.CodeCrossOrigin, "cross-origin request refused")
	}))
	checked := cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasBody := r.ContentLength != 0 || len(r.TransferEncoding) > 0
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if hasBody && mt != "application/json" && (mt != "application/octet-stream" || !isRaw(r)) {
			writeError(w, http.StatusUnsupportedMediaType, apierr.CodeContentType, "Content-Type must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Also the one place every request passes: a login made while serving it records the
		// names of the browser and system (device.go).
		r = withUserAgent(r)
		if isAPI(r.URL.Path) && !safeMethod(r.Method) {
			checked.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAPI(path string) bool { return path == "/api" || strings.HasPrefix(path, "/api/") }

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}
