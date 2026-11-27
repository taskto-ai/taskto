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
		panic(err)
	}
	return sub
}

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
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}
