package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// app looks like Vite's output.
var app = fstest.MapFS{
	"index.html":                {Data: []byte("<!doctype html><title>app</title>")},
	"favicon.svg":               {Data: []byte("<svg></svg>")},
	"manifest.webmanifest":      {Data: []byte(`{"name":"app"}`)},
	"sw.js":                     {Data: []byte("self.skipWaiting")},
	"icons/icon-192.png":        {Data: []byte("\x89PNG\r\n\x1a\n")},
	"assets/index-B1x2c3d4.js":  {Data: []byte("console.log(1)")},
	"assets/index-Q9w8e7r6.css": {Data: []byte("body{}")},
}

func serve(t *testing.T, h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newHandler(t *testing.T, fsys fs.FS) http.Handler {
	t.Helper()
	h, err := Handler(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFiles(t *testing.T) {
	h := newHandler(t, app)
	tests := []struct {
		path, wantType, wantBody, wantCache string
	}{
		{"/", "text/html", "<title>app</title>", "no-cache"},
		{"/index.html", "text/html", "<title>app</title>", "no-cache"},
		{"/favicon.svg", "image/svg+xml", "<svg>", "no-cache"},
		// A manifest is JSON of its own type; the service worker is JavaScript, and neither
		// is cached: an installed app learns of a new version by fetching them.
		{"/manifest.webmanifest", "application/manifest+json", `{"name"`, "no-cache"},
		{"/sw.js", "text/javascript", "self.skipWaiting", "no-cache"},
		{"/icons/icon-192.png", "image/png", "PNG", "no-cache"},
		{"/assets/index-B1x2c3d4.js", "javascript", "console.log", "public, max-age=31536000, immutable"},
		{"/assets/index-Q9w8e7r6.css", "text/css", "body{}", "public, max-age=31536000, immutable"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := serve(t, h, http.MethodGet, tt.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tt.wantType) {
				t.Errorf("Content-Type = %q, want %s", ct, tt.wantType)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want %q in it", rec.Body.String(), tt.wantBody)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", cc, tt.wantCache)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing X-Content-Type-Options: nosniff")
			}
		})
	}
}

func TestSPAFallback(t *testing.T) {
	h := newHandler(t, app)
	for _, p := range []string{"/boards/42", "/kb/notes/readme.md", "/apps/", "/../../etc/passwd", "/favicon.svg/x"} {
		rec := serve(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
			t.Errorf("GET %s = %d %q, want index.html", p, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s: Content-Type = %q, want text/html", p, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", p, cc)
		}
	}
}

func TestMissingAssetIs404(t *testing.T) {
	rec := serve(t, newHandler(t, app), http.MethodGet, "/assets/index-OLDHASH.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, not the page served as a script", rec.Code)
	}
}

func TestRevalidation(t *testing.T) {
	h := newHandler(t, app)
	etag := serve(t, h, http.MethodGet, "/").Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on index.html")
	}
	if rec := serve(t, h, http.MethodGet, "/boards/1", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for a matching ETag", rec.Code)
	}
}

func TestMethods(t *testing.T) {
	h := newHandler(t, app)
	if rec := serve(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD / = %d with %d bytes, want 200 and no body", rec.Code, rec.Body.Len())
	}
	rec := serve(t, h, http.MethodPost, "/")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST / = %d, Allow %q; want 405 with Allow: GET, HEAD", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestHandlerNeedsAnIndex(t *testing.T) {
	if _, err := Handler(fstest.MapFS{"assets/a.js": {}}); err == nil {
		t.Fatal("Handler accepted an app without index.html")
	}
}

func TestPick(t *testing.T) {
	placeholder := fstest.MapFS{"index.html": {Data: []byte("placeholder")}}
	if fsys, built := pick(app, placeholder); !built || fsys == nil {
		t.Error("a built app wasn't picked")
	}
	// What dist/ holds when the web app hasn't been copied in.
	if fsys, built := pick(fstest.MapFS{".gitkeep": {}}, placeholder); built {
		t.Error("an empty dist was taken for a built app")
	} else if data, _ := fs.ReadFile(fsys, "index.html"); string(data) != "placeholder" {
		t.Errorf("index.html = %q, want the placeholder", data)
	}
}

// TestEmbedded checks the real embedded files, with or without a built web app.
func TestEmbedded(t *testing.T) {
	fsys, built := FS()
	rec := serve(t, newHandler(t, fsys), http.MethodGet, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>taskto.ai</title>") {
		t.Fatalf("GET / = %d %q, want a taskto.ai page", rec.Code, rec.Body.String())
	}
	t.Logf("web app built into the test binary: %v", built)
}
