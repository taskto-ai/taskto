package auth

import (
	"mime"
	"net/http"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

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
