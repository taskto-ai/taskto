package auth

import (
	"context"
	"net/http"
	"strings"
)

type userAgentKey struct{}

func withUserAgent(r *http.Request) *http.Request {
	ua := r.UserAgent()
	if ua == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), userAgentKey{}, ua))
}

func deviceOf(ctx context.Context) (browser, os string) {
	ua, _ := ctx.Value(userAgentKey{}).(string)
	return parseUserAgent(ua)
}

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
