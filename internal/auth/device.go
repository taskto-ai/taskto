package auth

import (
	"context"
	"net/http"
	"strings"
)

// A session remembers what made its login, as two short names (a browser and a system) taken from
// the User-Agent header and nothing else: the header itself and the client's address are not
// kept. The names are the products' own, so the web app puts them in a sentence of its language.

type userAgentKey struct{}

// withUserAgent puts the request's User-Agent on its context, where insertSession finds it for a
// login made while serving the request. CSRF, which wraps the whole server, does it.
func withUserAgent(r *http.Request) *http.Request {
	ua := r.UserAgent()
	if ua == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), userAgentKey{}, ua))
}

// deviceOf returns the browser and the system the context's request names, "" for what it doesn't
// recognise.
func deviceOf(ctx context.Context) (browser, os string) {
	ua, _ := ctx.Value(userAgentKey{}).(string)
	return parseUserAgent(ua)
}

// parseUserAgent reads the names of the browser and the system out of a User-Agent header. The
// order of the checks matters: most browsers' headers name several others.
func parseUserAgent(ua string) (browser, os string) {
	if len(ua) > 1024 {
		ua = ua[:1024]
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(ua, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("Edg/", "EdgA/", "EdgiOS/"):
		browser = "Edge"
	case has("OPR/", "Opera"):
		browser = "Opera"
	case has("Firefox/", "FxiOS/"):
		browser = "Firefox"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("Chrome/", "CriOS/"):
		browser = "Chrome"
	case has("Safari/") && has("Version/"):
		browser = "Safari"
	}
	switch {
	case has("iPhone"):
		os = "iOS"
	case has("iPad"):
		os = "iPadOS"
	case has("Android"):
		os = "Android"
	case has("Windows"):
		os = "Windows"
	case has("CrOS"):
		os = "ChromeOS"
	case has("Mac OS X", "Macintosh"):
		os = "macOS"
	case has("Linux", "X11"):
		os = "Linux"
	}
	return browser, os
}
