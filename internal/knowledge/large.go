package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

const (
	DefaultLargeFileSize      = 50 << 20
	DefaultMaxUnversionedSize = 20 << 30
)

var ErrNotKept = errors.New("only the current version of an unversioned file is kept")

var (
	lfsDir     = path.Join(gitDir, "lfs")
	objectsDir = path.Join(lfsDir, "objects")
	tmpDir     = path.Join(lfsDir, "tmp")
)

const tmpMaxAge = 24 * time.Hour

var mediaTypes = map[string]string{
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".wmv": "video/x-ms-wmv",
	".flv": "video/x-flv", ".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".ogv": "video/ogg",
	".3gp": "video/3gpp",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg",
	".weba": "audio/webm", ".wma": "audio/x-ms-wma", ".aif": "audio/aiff", ".aiff": "audio/aiff",
	".amr": "audio/amr",
}

var heavyTypes = map[string]bool{
	".zip": true, ".7z": true, ".rar": true, ".tar": true, ".gz": true, ".tgz": true,
	".xz": true, ".txz": true, ".bz2": true, ".tbz2": true, ".zst": true, ".lz4": true,
	".iso": true, ".dmg": true, ".img": true, ".vmdk": true, ".vdi": true, ".vhd": true,
	".vhdx": true, ".qcow2": true,
	".msi": true, ".pkg": true, ".deb": true, ".rpm": true, ".apk": true, ".ipa": true,
	".raw": true, ".dng": true, ".cr2": true, ".cr3": true, ".nef": true, ".arw": true,
	".orf": true, ".raf": true, ".rw2": true,
	".psd": true, ".psb": true, ".tif": true, ".tiff": true, ".ai": true, ".indd": true,
	".sketch": true, ".fig": true, ".xcf": true, ".kra": true, ".blend": true,
	".gguf": true, ".safetensors": true,
}

func IsMedia(name string) bool { return MediaType(name) != "" }

func MediaType(name string) string { return mediaTypes[extension(name)] }

func extension(name string) string {
	base := path.Base(name)
	ext := path.Ext(base)
	if ext == base {
		return ""
	}
	return strings.ToLower(ext)
}

func AlwaysUnversioned(name string) bool {
	ext := extension(name)
	return mediaTypes[ext] != "" || heavyTypes[ext]
}

func UnversionedExtensions() []string {
	exts := make([]string, 0, len(mediaTypes)+len(heavyTypes))
	for e := range mediaTypes {
		exts = append(exts, e)
	}
	for e := range heavyTypes {
		exts = append(exts, e)
	}
	slices.Sort(exts)
	return slices.Compact(exts)
}

func (s *Store) LargeFileSize() int64 { return s.opts.LargeFileSize }

func (s *Store) unversioned(name string, size int64) bool {
	return !IsMarkdown(name) && (size > s.opts.LargeFileSize || AlwaysUnversioned(name))
}

func (r *repo) becomesUnversioned(name string, cur, next plumbing.Hash) (bool, error) {
	if IsMarkdown(name) {
		return false, nil
	}
	_, was, err := r.describe(name, cur)
	if err != nil || was != nil {
		return false, err
	}
	_, is, err := r.describe(name, next)
	return is != nil, err
}

type pointer struct {
	oid  string
	size int64
}

const pointerVersion = "version https://git-lfs.github.com/spec/v1\n"

const maxPointerSize = 200

func (p pointer) encode() []byte {
	return fmt.Appendf(nil, "%soid sha256:%s\nsize %d\n", pointerVersion, p.oid, p.size)
}

func parsePointer(b []byte) (pointer, bool) {
	rest, ok := bytes.CutPrefix(b, []byte(pointerVersion))
	if !ok {
		return pointer{}, false
	}
	rest, ok = bytes.CutPrefix(rest, []byte("oid sha256:"))
	if !ok || len(rest) < 64 {
		return pointer{}, false
	}
	p := pointer{oid: string(rest[:64])}
	sizeLine, ok := bytes.CutPrefix(rest[64:], []byte("\nsize "))
	if !ok || !isOID(p.oid) {
		return pointer{}, false
	}
	digits, ok := bytes.CutSuffix(sizeLine, []byte("\n"))
	if !ok {
		return pointer{}, false
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil || n < 0 {
		return pointer{}, false
	}
	p.size = n
	return p, bytes.Equal(p.encode(), b)
}

func isOID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func objectPath(oid string) string { return path.Join(objectsDir, oid[:2], oid[2:4], oid) }

func (r *repo) describe(name string, h plumbing.Hash) (Entry, *pointer, error) {
	size, err := r.st.EncodedObjectSize(h)
	if err != nil {
		return Entry{}, nil, err
	}
	e := Entry{Path: name, Version: h.String(), Size: size, Versioned: true}
	if size > maxPointerSize {
		return e, nil, nil
	}
	content, err := r.blobBytes(h)
	if err != nil {
		return Entry{}, nil, err
	}
	p, ok := parsePointer(content)
	if !ok {
		return e, nil, nil
	}
	e.Size, e.Versioned = p.size, false
	return e, &p, nil
}

func (r *repo) blobBytes(h plumbing.Hash) ([]byte, error) {
	blob, err := object.GetBlob(r.st, h)
	if err != nil {
		return nil, err
	}
	rd, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

func (r *repo) openObject(p *pointer, name string) (*os.File, error) {
	f, err := r.root.Open(objectPath(p.oid))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotKept, name)
	}
	return f, err
}

func (r *repo) hasObject(oid string) bool {
	fi, err := r.root.Lstat(objectPath(oid))
	return err == nil && fi.Mode().IsRegular()
}

type staged struct {
	root *os.Root
	tmp  string
	ptr  pointer
}

func (s *Store) stage(sp access.Space, name string, src io.Reader, limit int64) (*staged, error) {
	if err := s.ensureRepo(sp); err != nil {
		return nil, err
	}
	sg, f, err := s.openStaged(sp)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = errorf(apierr.CodeFileTooLarge, apierr.Params{"path": name, "max": limit}, "%w: %s is over %d bytes", ErrTooLarge, name, limit)
	}
	if err != nil {
		sg.discard()
		return nil, err
	}
	sg.ptr = pointer{oid: hex.EncodeToString(h.Sum(nil)), size: n}
	return sg, nil
}

func (s *Store) openStaged(sp access.Space) (*staged, *os.File, error) {
	st := s.lockSpace(sp, false, true)
	defer st.mu.RUnlock()
	if err := s.admit(st, sp); err != nil {
		return nil, nil, err
	}
	root, err := access.OpenRoot(s.dataDir, sp)
	if err != nil {
		return nil, nil, err
	}
	if err := root.MkdirAll(tmpDir, 0o700); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	sg := &staged{root: root, tmp: path.Join(tmpDir, "upload-"+randomSuffix())}
	f, err := root.OpenFile(sg.tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return sg, f, nil
}

func (sg *staged) bytes() ([]byte, error) { return sg.root.ReadFile(sg.tmp) }

func (sg *staged) discard() {
	_ = sg.root.Remove(sg.tmp)
	_ = sg.root.Close()
}

func (r *repo) adopt(sg *staged) error {
	dst := objectPath(sg.ptr.oid)
	if r.hasObject(sg.ptr.oid) {
		return nil
	}
	if err := r.root.MkdirAll(path.Dir(dst), 0o700); err != nil {
		return err
	}
	r.state.unv = -1
	return r.root.Rename(sg.tmp, dst)
}

func (s *Store) ensureRepo(sp access.Space) error {
	st := s.lockSpace(sp, true, false)
	defer st.mu.Unlock()
	if err := s.admit(st, sp); err != nil {
		return err
	}
	r, err := openRepo(s.dataDir, sp, st, true)
	if err != nil {
		return err
	}
	r.close()
	return nil
}

func (r *repo) unversionedSize() (int64, error) {
	var n int64
	err := r.objects(func(_ string, size int64) { n += size })
	return n, err
}

func (r *repo) objects(fn func(oid string, size int64)) error { return walkObjects(r.root.FS(), fn) }

func walkObjects(fsys fs.FS, fn func(oid string, size int64)) error {
	err := fs.WalkDir(fsys, objectsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if oid := path.Base(p); isOID(oid) && objectPath(oid) == p {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			fn(oid, fi.Size())
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (r *repo) gc() error {
	var have []string
	if err := r.objects(func(oid string, _ int64) { have = append(have, oid) }); err != nil {
		return err
	}
	if len(have) > 0 {
		used, err := r.pointers()
		if err != nil {
			return err
		}
		for _, oid := range have {
			if used[oid] {
				continue
			}
			p := objectPath(oid)
			r.state.unv = -1
			if err := r.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			dir := path.Dir(p)
			for dir != objectsDir && r.root.Remove(dir) == nil {
				dir = path.Dir(dir)
			}
		}
	}
	entries, err := fs.ReadDir(r.root.FS(), tmpDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now().Sub(fi.ModTime()) > tmpMaxAge {
			_ = r.root.Remove(path.Join(tmpDir, e.Name()))
		}
	}
	return nil
}

func (r *repo) pointers() (map[string]bool, error) {
	used := map[string]bool{}
	head, err := r.head()
	if err != nil || head == nil {
		return used, err
	}
	err = r.walk(head.TreeHash, "", func(name string, e object.TreeEntry) error {
		_, p, err := r.describe(name, e.Hash)
		if p != nil {
			used[p.oid] = true
		}
		return err
	})
	return used, err
}
