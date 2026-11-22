// Package webui serves the built web app from inside the binary.
//
// An embed directive can't reach outside its package directory, and fails when its
// pattern matches nothing, while go vet, go test and CI's Go job run without building
// the web app.
// So the build copies web/dist into dist/ here (`make build`), and dist/ always holds
// at least a committed .gitkeep for the pattern to match. Everything else in dist/ is
// ignored by git (the root .gitignore's `dist/`; .gitkeep was force-added). When
// dist/ has no index.html, the binary serves a placeholder page instead.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

//go:embed placeholder
var placeholder embed.FS

// FS returns the built web app and true, or a placeholder page and false when the
// binary was built without the app.
func FS() (fs.FS, bool) {
	return pick(mustSub(dist, "dist"), mustSub(placeholder, "placeholder"))
}

func pick(built, placeholder fs.FS) (fs.FS, bool) {
	if _, err := fs.Stat(built, "index.html"); err == nil {
		return built, true
	}
	return placeholder, false
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // dir is a literal, valid path
	}
	return sub
}

// Handler serves the files of a built single-page app from fsys:
//
//   - a path naming a file serves that file, with its content type from its extension;
//   - files under /assets/ are named by Vite after a hash of their content, so they are
//     cached for a year, and a missing one is a 404 (not the page, which would be
//     served as a script or stylesheet by mistake);
//   - any other path serves index.html, so the app's own router can handle it. The page
//     and other unhashed files are revalidated on every load (ETag): among them the
//     service worker (sw.js) and the manifest, which must never be served stale, or an
//     installed app would not learn of a new version.
//
// Routes the API owns are the caller's business: mount Handler on "/" only after them,
// and keep it from answering their unknown paths with the page.
func Handler(fsys fs.FS) (http.Handler, error) {
	files := map[string]file{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[p] = file{data: data, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files["index.html"]; !ok {
		return nil, errors.New("webui: no index.html")
	}
	return &handler{files: files}, nil
}

// contentTypes are set by extension where the system's MIME table, which Go reads, may lack
// the type or differ: the manifest has none in Go's own table, and a service worker must be
// served as JavaScript.
var contentTypes = map[string]string{
	".webmanifest": "application/manifest+json",
	".js":          "text/javascript; charset=utf-8",
}

type file struct {
	data []byte
	etag string
}

type handler struct {
	files map[string]file
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	f, found := h.files[name]
	hashed := strings.HasPrefix(name, "assets/")
	switch {
	case found && hashed:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case found:
		w.Header().Set("Cache-Control", "no-cache")
	case hashed:
		http.NotFound(w, r)
		return
	default:
		name, f = "index.html", h.files["index.html"]
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", f.etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ct, ok := contentTypes[path.Ext(name)]; ok {
		w.Header().Set("Content-Type", ct)
	}
	// ServeContent sets the content type from the name's extension and answers
	// If-None-Match and Range requests.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}
