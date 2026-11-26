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

// Unversioned files: large files, audio and video, and heavy binaries such as archives,
// disk images and design files stay out of git's objects.
//
// A file that isn't Markdown is unversioned when it is over Options.LargeFileSize bytes,
// or is of a kind AlwaysUnversioned names, whatever its size. Git still records it, as a small
// pointer at its path in Git LFS's format (version, sha256 oid, size), so listing,
// moving, deleting, conflicts (its version is the pointer's blob ID), its history and
// search by name work as for any file. Its bytes are in the space's directory, in Git
// LFS's local layout:
//
//	.git/lfs/objects/<oid[0:2]>/<oid[2:4]>/<oid>
//
// and the working tree holds the pointer, so git sees a clean checkout. Uploads are
// written to .git/lfs/tmp first, then renamed into place by the change that commits
// their pointer.
//
// Only the current content is kept: after every change that can drop a reference, the
// objects main no longer points to are removed (gc). Reading or restoring an older
// version whose bytes are gone is ErrNotKept. A file that was in git and becomes
// unversioned (it grows past the limit, or is renamed to a kind that always is) leaves
// git: its older versions are rewritten into pointers too, and their bytes dropped
// (rewrite.go).

// Defaults of the unversioned files' limits.
const (
	DefaultLargeFileSize      = 50 << 20 // 50 MiB
	DefaultMaxUnversionedSize = 20 << 30 // 20 GiB
)

// ErrNotKept is for the bytes of an unversioned file's older version: only the current
// one is kept.
var ErrNotKept = errors.New("only the current version of an unversioned file is kept")

var (
	lfsDir     = path.Join(gitDir, "lfs")
	objectsDir = path.Join(lfsDir, "objects")
	tmpDir     = path.Join(lfsDir, "tmp")
)

// tmpMaxAge is how old an upload's staged file is before gc removes it: one that old was
// left by a process that stopped mid-upload.
const tmpMaxAge = 24 * time.Hour

// mediaTypes are the audio and video files, by lower-case extension, with the media type
// a download of them is served as. Extensions other files use too stay out: .ts is
// TypeScript far more often than an MPEG transport stream.
var mediaTypes = map[string]string{
	// Video.
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".wmv": "video/x-ms-wmv",
	".flv": "video/x-flv", ".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".ogv": "video/ogg",
	".3gp": "video/3gpp",
	// Audio.
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg",
	".weba": "audio/webm", ".wma": "audio/x-ms-wma", ".aif": "audio/aiff", ".aiff": "audio/aiff",
	".amr": "audio/amr",
}

// heavyTypes are the other kinds of file that are unversioned whatever their size, by
// lower-case extension: binaries that are usually large and that git can't store any
// smaller than they are, since they are compressed already or change all over at every
// save. Only extensions that are clearly such files are here: fonts, executables, PDFs
// and office documents are often small, and stay versioned under the size limit. A
// double extension counts by its last part: .tar.gz is .gz.
var heavyTypes = map[string]bool{
	// Archives and compressed files.
	".zip": true, ".7z": true, ".rar": true, ".tar": true, ".gz": true, ".tgz": true,
	".xz": true, ".txz": true, ".bz2": true, ".tbz2": true, ".zst": true, ".lz4": true,
	// Disk and virtual-machine images.
	".iso": true, ".dmg": true, ".img": true, ".vmdk": true, ".vdi": true, ".vhd": true,
	".vhdx": true, ".qcow2": true,
	// Installer packages.
	".msi": true, ".pkg": true, ".deb": true, ".rpm": true, ".apk": true, ".ipa": true,
	// Raw camera images.
	".raw": true, ".dng": true, ".cr2": true, ".cr3": true, ".nef": true, ".arw": true,
	".orf": true, ".raf": true, ".rw2": true,
	// Layered images, design and 3D files.
	".psd": true, ".psb": true, ".tif": true, ".tiff": true, ".ai": true, ".indd": true,
	".sketch": true, ".fig": true, ".xcf": true, ".kra": true, ".blend": true,
	// Model weights.
	".gguf": true, ".safetensors": true,
}

// IsMedia reports whether name is an audio or video file, by its extension, whatever its
// case.
func IsMedia(name string) bool { return MediaType(name) != "" }

// MediaType is the media type of an audio or video file, by its extension; "" for other
// files.
func MediaType(name string) string { return mediaTypes[extension(name)] }

// extension is name's last extension, in lower case: "" for a name that is only one, such
// as a dotfile named .zip.
func extension(name string) string {
	base := path.Base(name)
	ext := path.Ext(base)
	if ext == base {
		return ""
	}
	return strings.ToLower(ext)
}

// AlwaysUnversioned reports whether name is a kind of file that is unversioned whatever
// its size: audio, video (IsMedia) and the heavy binaries of heavyTypes, by its extension,
// whatever its case. It is the one place that decides it; docs/api.md lists them.
func AlwaysUnversioned(name string) bool {
	ext := extension(name)
	return mediaTypes[ext] != "" || heavyTypes[ext]
}

// UnversionedExtensions are the extensions AlwaysUnversioned holds, sorted.
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

// LargeFileSize is the size over which a file is unversioned.
func (s *Store) LargeFileSize() int64 { return s.opts.LargeFileSize }

// unversioned reports whether a file named name of size bytes is kept out of git.
func (s *Store) unversioned(name string, size int64) bool {
	return !IsMarkdown(name) && (size > s.opts.LargeFileSize || AlwaysUnversioned(name))
}

// becomesUnversioned reports whether the file name, versioned at the blob cur, is
// unversioned at the blob next.
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

// pointer is what git holds for an unversioned file: its bytes' SHA-256 and size.
type pointer struct {
	oid  string
	size int64
}

const pointerVersion = "version https://git-lfs.github.com/spec/v1\n"

// maxPointerSize bounds a pointer's encoding: blobs bigger than it aren't read to check.
const maxPointerSize = 200

func (p pointer) encode() []byte {
	return fmt.Appendf(nil, "%soid sha256:%s\nsize %d\n", pointerVersion, p.oid, p.size)
}

// parsePointer returns the pointer b encodes, exactly as encode writes it.
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
	// One encoding per pointer: no leading zeros, signs or anything else ParseInt allows.
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

// objectPath is where the bytes of oid are, in the space's directory.
func objectPath(oid string) string { return path.Join(objectsDir, oid[:2], oid[2:4], oid) }

// describe returns the entry of the file name whose blob is h, and the pointer the blob
// holds if the file is unversioned (nil otherwise). Only blobs that can be pointers are
// read: those of at most maxPointerSize bytes. A Markdown document is never unversioned,
// but its older versions can be pointers, once taken out of history (rewrite.go): Put
// refuses Markdown that reads as a pointer, so that one never is anything else.
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

// openObject opens the bytes of p, or returns ErrNotKept for bytes gc has removed.
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

// staged is an upload's bytes, written to .git/lfs/tmp before a change adopts them.
type staged struct {
	root *os.Root
	tmp  string
	ptr  pointer
}

// stage copies src into a new file under sp's .git/lfs/tmp, hashing it as it goes, without
// holding the space's lock: a long upload doesn't stop anyone reading the space. More than
// limit bytes is ErrTooLarge. The repository is created first, if needed, so .git is never
// made by anything but openRepo. The caller discards the result.
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

// openStaged makes the file stage copies into. It holds the space's lock as a reader while it
// makes the directories and the file, only: a removal (RemoveSpace) comes before it, and the
// space is refused, or after it, and takes the file with the rest. The bytes are then copied into
// a file that may already be unlinked, which leaves nothing behind.
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

// bytes reads the staged file back, for one that turns out to belong in git.
func (sg *staged) bytes() ([]byte, error) { return sg.root.ReadFile(sg.tmp) }

// discard removes the staged file, unless a change adopted it.
func (sg *staged) discard() {
	_ = sg.root.Remove(sg.tmp)
	_ = sg.root.Close()
}

// adopt moves the staged bytes to their object path, unless the space already has them.
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

// ensureRepo creates sp's repository if it has none.
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

// unversionedSize is the bytes of the space's unversioned files, all of them on disk.
func (r *repo) unversionedSize() (int64, error) {
	var n int64
	err := r.objects(func(_ string, size int64) { n += size })
	return n, err
}

// objects calls fn for every object file on disk, with its oid and size.
func (r *repo) objects(fn func(oid string, size int64)) error { return walkObjects(r.root.FS(), fn) }

// walkObjects is objects for the space whose directory is fsys.
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

// gc removes the objects main doesn't point to, the folders they leave empty, and staged
// uploads older than tmpMaxAge. A failure leaves what it couldn't remove for the next gc.
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
		return nil // nothing was ever staged
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

// pointers returns the oids main's files point to.
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
