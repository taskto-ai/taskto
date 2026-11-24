// Package knowledge is the knowledge base: Markdown documents, and other files people
// upload, kept in git, one repository per space. Large files, and audio and video, are
// unversioned: git records a pointer to them and their bytes stay beside it (large.go).
//
// A space's repository is its directory (access.SpaceDir): the files are its working
// tree, so any agent can read them directly, and .git holds every version. Each change,
// by a person or by an agent acting for one, is a commit on main with that person as its
// author. The repository is created at the first write.
//
// Safety:
//
//   - Names come from people and models, so every one is checked with access.CheckName
//     (which refuses ".git" in any spelling, "..", absolute names) and the limits below
//     before anything else happens. Git never sees a raw path: documents are found by
//     walking git trees, and commits are built from trees here, not by go-git scanning
//     the working tree.
//   - Every file access, go-git's included, goes through the space's os.Root
//     (access.OpenRoot): go-git's storage runs on rootFS, so nothing can follow a link
//     out of the space.
//   - go-git is pure Go: no git binary runs, so no hook or config in the repository ever
//     does.
//   - The Store refuses to read a space its caller can't read, and to write one it can't
//     write; the HTTP routes check access first with access.Checker.RequireSpace.
//
// One writer at a time changes a space's repository, so a commit's parent is always the
// head its change was checked against: an update must name the version it was based on
// and gets ErrConflict if the document has changed since.
package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// Default limits.
const (
	DefaultMaxFileSize = 2 << 30 // 2 GiB
	DefaultMaxRepoSize = 1 << 30 // 1 GiB
)

// Fixed limits on names, listings and messages.
const (
	// MaxDepth is how many elements a document's path may have ("a/b/c.md" has 3).
	MaxDepth = 16
	// MaxPathLen is the longest path, in bytes; MaxNameLen the longest element.
	MaxPathLen = 1024
	MaxNameLen = 255
	// MaxListLimit is the most documents one listing returns; DefaultListLimit applies
	// when the caller asks for none.
	MaxListLimit     = 1000
	DefaultListLimit = 200
	// MaxHistoryLimit and DefaultHistoryLimit are the same for history.
	MaxHistoryLimit     = 200
	DefaultHistoryLimit = 50
	// MaxMessageLen is the longest commit message, in characters; it is one line.
	MaxMessageLen = 200
)

// Options are the knowledge base's limits. Zero values take the defaults.
type Options struct {
	// MaxFileSize is the largest file of any kind, in bytes.
	MaxFileSize int64
	// LargeFileSize is the size, in bytes, over which a file other than Markdown is
	// unversioned (large.go); it is also the largest Markdown document. It is at most
	// MaxFileSize.
	LargeFileSize int64
	// MaxRepoSize caps a space's repository (.git, every version included, but not the
	// unversioned files' bytes), in bytes. A change that would take it past the cap is
	// refused; deleting a document doesn't free space, since its history stays.
	MaxRepoSize int64
	// MaxUnversionedSize caps the bytes of a space's unversioned files, in bytes.
	// Deleting or replacing one frees its bytes.
	MaxUnversionedSize int64
}

// The errors the Store's methods wrap. Their messages name only what the caller sent.
var (
	// ErrInvalid is for a bad name, message, revision or content.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound is for a document, folder or revision that doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is for a change based on an old version, or that would replace another
	// document or folder.
	ErrConflict = errors.New("conflict")
	// ErrTooLarge is for a file over the size limit.
	ErrTooLarge = errors.New("file too large")
	// ErrRepoFull is for a change that would take the repository over its size limit.
	ErrRepoFull = errors.New("the space's knowledge base is full")
	// ErrReadOnly is for a write to a space the caller may only read.
	ErrReadOnly = errors.New("you can only read this space")

	// errNoRepo is for a space without a repository: nothing was ever written to it.
	errNoRepo = fmt.Errorf("%w: this space has no documents yet", ErrNotFound)
)

// codedError is one of the errors above with the API's code for it and its message's
// params (internal/apierr), which the HTTP routes answer with; its message is unchanged.
// Without one, an error gets its sentinel's code (fail, in http.go).
type codedError struct {
	error
	code   apierr.Code
	params apierr.Params
}

func (e *codedError) Unwrap() error { return e.error }

// errorf is fmt.Errorf, with the API's code and params for the error.
func errorf(code apierr.Code, params apierr.Params, format string, args ...any) error {
	return &codedError{error: fmt.Errorf(format, args...), code: code, params: params}
}

// pathParams is the params of a message about the document or folder name.
func pathParams(name string) apierr.Params { return apierr.Params{"path": name} }

// Author is who makes a change: a person, or an agent acting for one. The commit's author
// is always the person, with their name and email; an agent is recorded as the commit's
// committer, so `git log` shows both and the history reports the agent.
type Author struct {
	Name  string
	Email string
	// Agent names the agent that made the change for the person; empty when they made it
	// themselves.
	Agent string
}

// Entry is a document as listed: its path, version (the git blob ID of its content, or
// of the pointer to it for an unversioned file) and size in bytes, and whether git keeps
// its versions.
type Entry struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	Versioned bool   `json:"versioned"`
}

// Doc is a document with its content. An unversioned file's Content is nil: Open reads
// it.
type Doc struct {
	Entry
	Content []byte
}

// Page is one page of a listing. Next, if not empty, is the After of the next page.
type Page struct {
	Docs []Entry `json:"docs"`
	Next string  `json:"next,omitempty"`
}

// Person is a commit's author.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Commit is one change in a history.
type Commit struct {
	Commit string `json:"commit"`
	Author Person `json:"author"`
	// Agent is the agent that made the change for Author, if one did.
	Agent   string `json:"agent,omitempty"`
	Time    string `json:"time"`
	Message string `json:"message"`
	// In a document's history: the document's version after the commit ("" once
	// deleted), and what happened to it: "create", "update" or "delete".
	Version string `json:"version,omitempty"`
	Change  string `json:"change,omitempty"`
	// Versioned, with Version, is false for a version whose bytes git doesn't keep: an
	// unversioned file's, or one taken out of history.
	Versioned *bool `json:"versioned,omitempty"`
}

// History is one page of a history, newest first. Next, if not empty, is the Before of
// the next page.
type History struct {
	Commits []Commit `json:"commits"`
	Next    string   `json:"next,omitempty"`
}

// Result is what a change made: the commit (the head, if nothing changed), and the
// document it leaves: none after a delete or a folder's move.
type Result struct {
	Commit string `json:"commit"`
	Doc    *Entry `json:"doc,omitempty"`
}

// Store is the knowledge base of every space under a data directory.
type Store struct {
	dataDir string
	opts    Options

	mu     sync.Mutex
	spaces map[int64]*spaceState

	onChange []func(access.Space) // see OnChange

	// quota is the storage quotas' check (SetQuota); nil for none.
	quota func(sp access.Space, adding int64) error
}

// spaceState is what the Store keeps per space while it runs.
type spaceState struct {
	// mu lets one writer, or many readers, at the space's repository.
	mu sync.RWMutex
	// size is the bytes in .git, counted at the first write; -1 until then.
	size int64
	// unv is the bytes of the unversioned files, counted when a write needs them; -1 when
	// unknown (usage.go). Only a writer, holding mu, reads or sets it.
	unv int64

	// cache is the usage the last write published, under cacheMu, which no one holds for
	// long and a writer may take while holding mu.
	cacheMu sync.Mutex
	cache   usageCache
}

// New returns the knowledge base of the spaces under dataDir.
func New(dataDir string, opts Options) *Store {
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = DefaultMaxFileSize
	}
	if opts.MaxRepoSize <= 0 {
		opts.MaxRepoSize = DefaultMaxRepoSize
	}
	if opts.LargeFileSize <= 0 {
		opts.LargeFileSize = DefaultLargeFileSize
	}
	opts.LargeFileSize = min(opts.LargeFileSize, opts.MaxFileSize)
	if opts.MaxUnversionedSize <= 0 {
		opts.MaxUnversionedSize = DefaultMaxUnversionedSize
	}
	return &Store{dataDir: dataDir, opts: opts, spaces: map[int64]*spaceState{}}
}

// MaxFileSize is the largest file the Store accepts.
func (s *Store) MaxFileSize() int64 { return s.opts.MaxFileSize }

func (s *Store) state(spaceID int64) *spaceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.spaces[spaceID]
	if !ok {
		st = &spaceState{size: -1, unv: -1}
		s.spaces[spaceID] = st
	}
	return st
}

// List returns the documents of sp, by path, from the folder prefix ("" for all), after
// the path after, at most limit of them (0 for DefaultListLimit).
func (s *Store) List(ctx context.Context, sp access.Space, prefix, after string, limit int) (Page, error) {
	return s.ListAt(ctx, sp, "", prefix, after, limit)
}

// ListAt is List as of the commit at (the head if empty), so a reader that lists in pages, and
// reads each document at the same commit (Open, Stream), sees one state of the space however
// the space changes meanwhile. A commit the repository no longer has is ErrNotFound.
func (s *Store) ListAt(ctx context.Context, sp access.Space, at, prefix, after string, limit int) (Page, error) {
	if prefix != "" {
		if err := checkPath(prefix); err != nil {
			return Page{}, err
		}
	}
	limit, err := pageLimit(limit, DefaultListLimit, MaxListLimit)
	if err != nil {
		return Page{}, err
	}
	var page Page
	err = s.read(sp, func(r *repo) error {
		page, err = r.list(ctx, at, prefix, after, limit)
		return err
	})
	if errors.Is(err, errNoRepo) && prefix == "" && at == "" {
		err = nil
	}
	if page.Docs == nil {
		page.Docs = []Entry{}
	}
	return page, err
}

// Read returns the document name of sp, as it is now, or as it was at the commit at.
func (s *Store) Read(_ context.Context, sp access.Space, name, at string) (Doc, error) {
	if err := checkPath(name); err != nil {
		return Doc{}, err
	}
	var doc Doc
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(at)
		if err != nil {
			return err
		}
		doc, err = r.readDoc(c, name)
		return err
	})
	return doc, err
}

// Put creates or replaces the document name of sp with content. To create it, base must
// be empty; to replace it, base must be its current version, or the result is
// ErrConflict. message is the commit message; empty means one saying what changed. A
// file other than Markdown over the large-file size, or audio or video, is unversioned.
func (s *Store) Put(_ context.Context, sp access.Space, name string, content []byte, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, name, content, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	// Bytes that read as a pointer are kept as an unversioned file's, so they read back as
	// they were rather than as the file they would point to.
	if _, isPtr := parsePointer(content); s.unversioned(name, int64(len(content))) || (isPtr && !IsMarkdown(name)) {
		sg, err := s.stage(sp, name, bytes.NewReader(content), s.opts.MaxFileSize)
		if err != nil {
			return Result{}, err
		}
		defer sg.discard()
		return s.putUnversioned(sp, name, sg, base, by, message)
	}
	return s.put(sp, name, int64(len(content)), base, by, message, func(r *repo, _ *object.TreeEntry) (plumbing.Hash, error) {
		return r.storeBlob(content)
	})
}

// PutIf is Put for a Markdown document, with a guard that runs while the space's write lock is
// held, right before the commit: a caller that must check, as of the moment of the write, that
// it may still make the change (an authorization that can be withdrawn while the change waits
// for the lock) does it there. A guard that returns an error stops the change, and the error is
// returned as it is; nothing is committed. The guard must not call back into the Store.
func (s *Store) PutIf(_ context.Context, sp access.Space, name string, content []byte, base string, by Author, message string, guard func() error) (Result, error) {
	if err := s.checkWrite(sp, name, content, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	if !IsMarkdown(name) {
		return Result{}, fmt.Errorf("%w: a guarded change is for a Markdown document", ErrInvalid)
	}
	return s.putIf(sp, name, int64(len(content)), base, by, message, guard, func(r *repo, _ *object.TreeEntry) (plumbing.Hash, error) {
		return r.storeBlob(content)
	})
}

// FindCommit looks through the space's history, newest first, for the commit whose message is
// exactly message and whose committer is the agent (empty for none): the commit an earlier try
// of a change made, found again by the message it was given.
func (s *Store) FindCommit(ctx context.Context, sp access.Space, message, agent string) (Commit, bool, error) {
	before := ""
	for {
		page, err := s.History(ctx, sp, "", before, MaxHistoryLimit)
		if err != nil {
			return Commit{}, false, err
		}
		for _, c := range page.Commits {
			if c.Message == message && c.Agent == agent {
				return c, true, nil
			}
		}
		if page.Next == "" {
			return Commit{}, false, nil
		}
		before = page.Next
		if err := ctx.Err(); err != nil {
			return Commit{}, false, err
		}
	}
}

// PutFile is Put for content read from src, of any size up to the largest file: it streams
// the bytes of a file other than Markdown to disk, without holding them in memory.
func (s *Store) PutFile(ctx context.Context, sp access.Space, name string, src io.Reader, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, name, nil, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	if IsMarkdown(name) {
		content, err := io.ReadAll(io.LimitReader(src, s.opts.LargeFileSize+1))
		if err != nil {
			return Result{}, err
		}
		return s.Put(ctx, sp, name, content, base, by, message)
	}
	sg, err := s.stage(sp, name, src, s.opts.MaxFileSize)
	if err != nil {
		return Result{}, err
	}
	defer sg.discard()
	if !s.unversioned(name, sg.ptr.size) {
		content, err := sg.bytes()
		if err != nil {
			return Result{}, err
		}
		if _, isPtr := parsePointer(content); !isPtr {
			return s.put(sp, name, int64(len(content)), base, by, message, func(r *repo, _ *object.TreeEntry) (plumbing.Hash, error) {
				return r.storeBlob(content)
			})
		}
	}
	return s.putUnversioned(sp, name, sg, base, by, message)
}

// putUnversioned commits a pointer to the staged bytes at name, once they fit in the
// space's unversioned size, and moves them into place.
func (s *Store) putUnversioned(sp access.Space, name string, sg *staged, base string, by Author, message string) (Result, error) {
	return s.put(sp, name, maxPointerSize, base, by, message, func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error) {
		r.collect = true
		if !r.hasObject(sg.ptr.oid) {
			used, err := r.unversionedSize()
			if err != nil {
				return plumbing.ZeroHash, err
			}
			// The bytes of the file it replaces are freed by this change.
			var freed int64
			if cur != nil {
				if _, p, err := r.describe(name, cur.Hash); err != nil {
					return plumbing.ZeroHash, err
				} else if p != nil && r.hasObject(p.oid) {
					freed = p.size
					used -= p.size
				}
			}
			if used+sg.ptr.size > s.opts.MaxUnversionedSize {
				return plumbing.ZeroHash, fmt.Errorf("%w: its unversioned files would be over %d bytes", ErrRepoFull, s.opts.MaxUnversionedSize)
			}
			// The bytes the change adds to the space, the replaced file's taken off: the write
			// checked the pointer and the commit, these are the file's own.
			if err := s.checkQuota(sp, sg.ptr.size-freed); err != nil {
				return plumbing.ZeroHash, err
			}
			if err := r.adopt(sg); err != nil {
				return plumbing.ZeroHash, err
			}
		}
		return r.storeBlob(sg.ptr.encode())
	})
}

// put creates or replaces name with the blob store makes, as Put describes; adding is the
// size of what store adds to git.
func (s *Store) put(sp access.Space, name string, adding int64, base string, by Author, message string, store func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error)) (Result, error) {
	return s.putIf(sp, name, adding, base, by, message, nil, store)
}

// putIf is put with a guard, run once the write lock is held and before anything is read or
// committed: if it returns an error, nothing is written and the error is the result.
func (s *Store) putIf(sp access.Space, name string, adding int64, base string, by Author, message string, guard func() error, store func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error)) (Result, error) {
	var res Result
	err := s.write(sp, adding, true, func(r *repo) error {
		if guard != nil {
			if err := guard(); err != nil {
				return err
			}
		}
		head, err := r.head()
		if err != nil {
			return err
		}
		cur, err := r.entryAt(head, name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		verb := "Create"
		switch {
		case cur == nil && base != "":
			return errorf(apierr.CodeDocDeleted, pathParams(name), "%w: %s was deleted since version %s", ErrConflict, name, base)
		case cur != nil && base == "":
			return errorf(apierr.CodeDocExists, pathParams(name), "%w: %s already exists; send the version you edited as base_version", ErrConflict, name)
		case cur != nil && cur.Hash.String() != base:
			return errorf(apierr.CodeDocChanged, pathParams(name), "%w: %s has changed since version %s", ErrConflict, name, base)
		case cur != nil:
			verb = "Update"
			// What it replaces may be an unversioned file's pointer, whose bytes go.
			r.collect = r.collect || !IsMarkdown(name)
		}
		blob, err := store(r, cur)
		if err != nil {
			return err
		}
		// A versioned file whose new content is unversioned leaves history: its older
		// versions are taken out of git once the change has landed (rewrite.go).
		rewrite := false
		if cur != nil && cur.Mode != filemode.Dir {
			if rewrite, err = r.becomesUnversioned(name, cur.Hash, blob); err != nil {
				return err
			}
		}
		if rewrite {
			if err := r.planRewrite(rewritePlan{after: head.Hash.String(), paths: []string{name}}); err != nil {
				return err
			}
		}
		commit, err := r.change(head, []edit{{name: name, hash: blob}}, by, orDefault(message, verb+" "+name))
		if rewrite {
			commit = r.afterChange(sp, commit, err)
		}
		if err != nil {
			return err
		}
		res, err = r.result(commit, name, blob)
		return err
	})
	return res, err
}

// Open returns the file name of sp, as it is now or as it was at the commit at, with its
// bytes to read: from git, or from disk for an unversioned file. The caller closes them;
// they stay readable after a change replaces the file. An unversioned file's older
// version whose bytes are gone is ErrNotKept.
func (s *Store) Open(_ context.Context, sp access.Space, name, at string) (Entry, File, error) {
	if err := checkPath(name); err != nil {
		return Entry{}, nil, err
	}
	var entry Entry
	var file File
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(at)
		if err != nil {
			return err
		}
		e, err := r.entryAt(c, name)
		if err != nil {
			return err
		}
		if e.Mode == filemode.Dir {
			return fmt.Errorf("%w: %s is a folder", ErrNotFound, name)
		}
		var p *pointer
		if entry, p, err = r.describe(name, e.Hash); err != nil {
			return err
		}
		if p != nil {
			f, err := r.openObject(p, name)
			file = f
			return err
		}
		content, err := r.blobBytes(e.Hash)
		file = bytesFile{bytes.NewReader(content)}
		return err
	})
	if err != nil {
		return Entry{}, nil, err
	}
	return entry, file, nil
}

// File is a file's bytes, from Open.
type File interface {
	io.ReadSeeker
	io.Closer
}

type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

// Move renames the document or folder from to to, which mustn't exist. base, if not
// empty, must be the document's current version; it can't be given for a folder.
func (s *Store) Move(_ context.Context, sp access.Space, from, to, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, from, nil, by, message); err != nil {
		return Result{}, err
	}
	if err := checkPath(to); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	if from == to || strings.HasPrefix(to, from+"/") {
		return Result{}, errorf(apierr.CodeDocMoveIntoItself, pathParams(from), "%w: can't move %s into itself", ErrInvalid, from)
	}
	var res Result
	err := s.write(sp, 0, false, func(r *repo) error {
		head, err := r.head()
		if err != nil {
			return err
		}
		cur, err := r.entryAt(head, from)
		if err != nil {
			return err
		}
		if err := checkBase(cur, from, base); err != nil {
			return err
		}
		if cur.Mode != filemode.Dir && IsMarkdown(from) != IsMarkdown(to) {
			// A Markdown document is never unversioned, so the bytes of an unversioned file
			// renamed to one would be lost, and a document holding a pointer would become
			// one: neither crosses over.
			other := from
			if IsMarkdown(from) {
				other = to
			}
			if _, p, err := r.describe(other, cur.Hash); err != nil {
				return err
			} else if p != nil {
				return errorf(apierr.CodeDocMoveKind, pathParams(from), "%w: %s is unversioned and can't become a Markdown document, or the other way round", ErrInvalid, from)
			}
		}
		if _, err := r.entryAt(head, to); err == nil {
			return errorf(apierr.CodeDocExists, pathParams(to), "%w: %s already exists", ErrConflict, to)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		edits, err := r.moveEdits(cur, from, to)
		if err != nil {
			return err
		}
		// A versioned file whose new name makes it unversioned (a kind that always is, say)
		// moves its bytes out of git, and its older versions, at from, leave history.
		moved, rewrite := cur.Hash, false
		if cur.Mode != filemode.Dir {
			entry, p, err := r.describe(from, cur.Hash)
			if err != nil {
				return err
			}
			if p == nil && s.unversioned(to, entry.Size) {
				r.collect = true // the bytes it adopts, if the change fails
				ptr, err := r.toLFS(cur.Hash, s.opts.MaxUnversionedSize)
				if err != nil {
					return err
				}
				if moved, err = r.storeBlob(ptr.encode()); err != nil {
					return err
				}
				edits = []edit{{name: from}, {name: to, hash: moved}}
				rewrite = true
				if err := r.planRewrite(rewritePlan{after: head.Hash.String(), paths: []string{from}}); err != nil {
					return err
				}
			}
		}
		commit, err := r.change(head, edits, by, orDefault(message, "Move "+from+" to "+to))
		if rewrite {
			commit = r.afterChange(sp, commit, err)
		}
		if err != nil || cur.Mode == filemode.Dir {
			res = Result{Commit: commit}
			return err
		}
		res, err = r.result(commit, to, moved)
		return err
	})
	return res, err
}

// Delete removes the document name. base, if not empty, must be its current version.
// Its history stays.
func (s *Store) Delete(_ context.Context, sp access.Space, name, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, name, nil, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.write(sp, 0, false, func(r *repo) error {
		head, err := r.head()
		if err != nil {
			return err
		}
		cur, err := r.entryAt(head, name)
		if err != nil {
			return err
		}
		if cur.Mode == filemode.Dir {
			return errorf(apierr.CodeDocDeleteFolder, pathParams(name), "%w: %s is a folder; delete its documents", ErrInvalid, name)
		}
		if err := checkBase(cur, name, base); err != nil {
			return err
		}
		r.collect = !IsMarkdown(name)
		commit, err := r.change(head, []edit{{name: name}}, by, orDefault(message, "Delete "+name))
		res = Result{Commit: commit}
		return err
	})
	return res, err
}

// History lists the commits that changed the document name, or every commit of the
// space if name is empty, newest first: before the commit before (empty for the newest),
// at most limit of them (0 for DefaultHistoryLimit). It doesn't follow renames.
func (s *Store) History(ctx context.Context, sp access.Space, name, before string, limit int) (History, error) {
	if name != "" {
		if err := checkPath(name); err != nil {
			return History{}, err
		}
	}
	limit, err := pageLimit(limit, DefaultHistoryLimit, MaxHistoryLimit)
	if err != nil {
		return History{}, err
	}
	var h History
	err = s.read(sp, func(r *repo) error {
		h, err = r.history(ctx, name, before, limit)
		return err
	})
	if errors.Is(err, errNoRepo) && before == "" {
		err = nil
	}
	if h.Commits == nil {
		h.Commits = []Commit{}
	}
	return h, err
}

// Restore makes the document name what it was at the commit at, as a new commit; it
// also brings back a deleted document. base, if not empty, must be its current version.
func (s *Store) Restore(_ context.Context, sp access.Space, name, at, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, name, nil, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
	if at == "" {
		return Result{}, fmt.Errorf("%w: the commit to restore is required", ErrInvalid)
	}
	// The old content's blob is already stored: restoring adds only trees and a commit.
	var res Result
	err := s.write(sp, 0, false, func(r *repo) error {
		c, err := r.revision(at)
		if err != nil {
			return err
		}
		old, err := r.entryAt(c, name)
		if err != nil {
			return err
		}
		if old.Mode == filemode.Dir {
			return fmt.Errorf("%w: %s was a folder at %s", ErrInvalid, name, at)
		}
		if _, p, err := r.describe(name, old.Hash); err != nil {
			return err
		} else if p != nil && !r.hasObject(p.oid) {
			return fmt.Errorf("%w: %s at %s", ErrNotKept, name, at)
		}
		r.collect = !IsMarkdown(name)
		head, err := r.head()
		if err != nil {
			return err
		}
		cur, err := r.entryAt(head, name)
		if errors.Is(err, ErrNotFound) {
			if base != "" {
				return errorf(apierr.CodeDocDeleted, pathParams(name), "%w: %s was deleted since version %s", ErrConflict, name, base)
			}
		} else if err != nil {
			return err
		} else if err := checkBase(cur, name, base); err != nil {
			return err
		}
		commit, err := r.change(head, []edit{{name: name, hash: old.Hash}}, by,
			orDefault(message, "Restore "+name+" from "+at[:7]))
		if err != nil {
			return err
		}
		res, err = r.result(commit, name, old.Hash)
		return err
	})
	return res, err
}

// read runs fn on sp's repository with readers' access. Without one, it is errNoRepo.
func (s *Store) read(sp access.Space, fn func(*repo) error) error {
	if !sp.Access.Allows(access.Read) {
		return access.ErrNotFound
	}
	st := s.state(sp.ID)
	st.mu.RLock()
	defer st.mu.RUnlock()
	r, err := openRepo(s.dataDir, sp, st, false)
	if err != nil {
		return err
	}
	if r == nil {
		return errNoRepo
	}
	defer r.close()
	return fn(r)
}

// changeOverhead is what a change adds besides its content: a commit object and the trees
// on the path to what it changed, compressed. A generous estimate, so no sequence of
// changes, renames included, can take the repository far past its limit.
const changeOverhead = 1 << 10

// write runs fn on sp's repository as its only writer. With create, a space without a
// repository gets one; without, it is ErrNotFound. adding is the size of the content fn
// will store, checked against the repository's limit first; < 0 for a change that frees
// space, which a full repository still takes. The OnChange functions run
// after it, once the space is unlocked.
func (s *Store) write(sp access.Space, adding int64, create bool, fn func(*repo) error) error {
	err := s.writeLocked(sp, adding, create, fn)
	s.changed(sp)
	return err
}

func (s *Store) writeLocked(sp access.Space, adding int64, create bool, fn func(*repo) error) error {
	st := s.state(sp.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	r, err := openRepo(s.dataDir, sp, st, create)
	if err != nil {
		return err
	}
	if r == nil {
		return errNoRepo
	}
	defer r.close()
	// A change the last process didn't finish (it stopped mid-way) is undone first.
	if err := r.repair(); err != nil {
		return err
	}
	// So is a history rewrite: done again, or dropped if its change never landed. If it
	// fails, the change goes ahead and the journal stays for the next one.
	if _, err := r.resumeRewrite(sp); err != nil {
		slog.Warn("knowledge: taking a file's old versions out of history; it is retried at the next change", "space", sp.ID, "err", err)
	}
	// Whatever the change does (the repair above may have changed things already), the usage
	// is published again once the write is over.
	defer s.publishUsage(r)
	size, err := r.size()
	if err != nil {
		return err
	}
	// Every change adds a commit and trees, whatever its content, and deleting frees
	// nothing (history keeps it): once full, the repository refuses every change, but
	// those that only free space (adding < 0).
	if adding >= 0 && size+adding+changeOverhead > s.opts.MaxRepoSize {
		return fmt.Errorf("%w (limit %d bytes)", ErrRepoFull, s.opts.MaxRepoSize)
	}
	// The storage quotas: a change that adds content (not one that deletes or moves, which a
	// full space must be able to do) must fit. The usage they read is this space's as the
	// repair above left it.
	if adding > 0 && s.quota != nil {
		s.publishUsage(r)
		if err := s.checkQuota(sp, adding+changeOverhead); err != nil {
			return err
		}
	}
	err = fn(r)
	// Whether it succeeded or not: a failed change can leave adopted bytes nothing points
	// to.
	if r.collect {
		if gcErr := r.gc(); gcErr != nil {
			slog.Warn("knowledge: removing unversioned files' old bytes", "space", sp.ID, "err", gcErr)
		}
	}
	return err
}

// checkWrite checks what every change needs: write access, a valid name, content within
// the limit and for its kind, an author and a valid message.
func (s *Store) checkWrite(sp access.Space, name string, content []byte, by Author, message string) error {
	switch {
	case !sp.Access.Allows(access.Read):
		return access.ErrNotFound
	case !sp.Access.Allows(access.Write):
		return ErrReadOnly
	}
	if err := checkPath(name); err != nil {
		return err
	}
	limit := s.opts.MaxFileSize
	if IsMarkdown(name) {
		limit = s.opts.LargeFileSize // a Markdown document is always versioned
	}
	if int64(len(content)) > limit {
		return errorf(apierr.CodeFileTooLarge, apierr.Params{"path": name, "max": limit}, "%w: %s is over %d bytes", ErrTooLarge, name, limit)
	}
	if IsMarkdown(name) && !utf8.Valid(content) {
		return errorf(apierr.CodeDocNotUTF8, pathParams(name), "%w: %s isn't UTF-8 text", ErrInvalid, name)
	}
	if _, isPtr := parsePointer(content); isPtr && IsMarkdown(name) {
		// It would read as one of its older versions taken out of history.
		return fmt.Errorf("%w: a Markdown document can't be exactly a Git LFS pointer", ErrInvalid)
	}
	if strings.TrimSpace(by.Name) == "" || strings.TrimSpace(by.Email) == "" {
		return fmt.Errorf("%w: a change needs its author's name and email", ErrInvalid)
	}
	return checkMessage(message)
}

// IsMarkdown reports whether name is a Markdown document, by its extension; other files
// are uploads.
func IsMarkdown(name string) bool { return strings.EqualFold(path.Ext(name), ".md") }

// checkPath checks a name from a caller: access.CheckName, then the length and depth
// limits.
func checkPath(name string) error {
	if err := access.CheckName(name); err != nil {
		return errorf(apierr.CodeDocNameInvalid, apierr.Params{"name": name}, "%w: %w", ErrInvalid, err)
	}
	if len(name) > MaxPathLen {
		return errorf(apierr.CodeDocPathTooLong, apierr.Params{"max": MaxPathLen}, "%w: a path is at most %d bytes", ErrInvalid, MaxPathLen)
	}
	elems := strings.Split(name, "/")
	if len(elems) > MaxDepth {
		return errorf(apierr.CodeDocPathTooDeep, apierr.Params{"max": MaxDepth}, "%w: a path has at most %d levels", ErrInvalid, MaxDepth)
	}
	for _, e := range elems {
		if len(e) > MaxNameLen {
			return errorf(apierr.CodeDocNameTooLong, apierr.Params{"max": MaxNameLen}, "%w: a file or folder name is at most %d bytes", ErrInvalid, MaxNameLen)
		}
	}
	return nil
}

// checkVersion checks the form of a version or commit ID: 40 lower-case hex digits, or
// empty.
func checkVersion(v string) error {
	if v != "" && !isHash(v) {
		return fmt.Errorf("%w: %q isn't a version", ErrInvalid, v)
	}
	return nil
}

func isHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func checkMessage(m string) error {
	if utf8.RuneCountInString(m) > MaxMessageLen {
		return errorf(apierr.CodeDocMessageInvalid, apierr.Params{"max": MaxMessageLen}, "%w: a message is at most %d characters", ErrInvalid, MaxMessageLen)
	}
	for _, r := range m {
		if unicode.IsControl(r) {
			return errorf(apierr.CodeDocMessageInvalid, apierr.Params{"max": MaxMessageLen}, "%w: a message is one line of text", ErrInvalid)
		}
	}
	return nil
}

// checkBase checks that base, if given, is the current version of the document cur.
func checkBase(cur *object.TreeEntry, name, base string) error {
	switch {
	case base == "":
		return nil
	case cur.Mode == filemode.Dir:
		return fmt.Errorf("%w: %s is a folder; base_version is for documents", ErrInvalid, name)
	case cur.Hash.String() != base:
		return errorf(apierr.CodeDocChanged, pathParams(name), "%w: %s has changed since version %s", ErrConflict, name, base)
	}
	return nil
}

func orDefault(message, def string) string {
	if m := strings.TrimSpace(message); m != "" {
		return m
	}
	return def
}

func pageLimit(n, def, maxN int) (int, error) {
	switch {
	case n == 0:
		return def, nil
	case n < 0 || n > maxN:
		return 0, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalid, maxN)
	}
	return n, nil
}

// timeFormat is the API's time format.
const timeFormat = "2006-01-02T15:04:05.000Z"

// now is the clock for commit times; tests replace it.
var now = time.Now
