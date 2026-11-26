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
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
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

	// ErrSpaceRemoved is for a call on a space that RemoveSpace removed (an account's purge): it
	// is refused, and never made again, until the caller has deleted the space's rows
	// (ForgetSpace). It is access.ErrNotFound's, so the API answers it as a space that isn't there.
	ErrSpaceRemoved = fmt.Errorf("%w: the space was removed", access.ErrNotFound)

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
	quota func(sp access.Space, adding int64) (release func(), err error)

	// afterMeasure is for tests: it runs once a space's first measurement is made, before it is stored.
	afterMeasure func(sp access.Space)

	// spaceExists is SetSpaceCheck's; nil for none.
	spaceExists func(sp access.Space) (bool, error)

	// beforeLock is for tests: it runs in a write after the space's state is taken and before
	// its lock is, the moment a removal can slip in between.
	beforeLock func(sp access.Space)
}

// spaceState is what the Store keeps per space while it runs.
type spaceState struct {
	// mu lets one writer, or many readers, at the space's repository.
	mu sync.RWMutex
	// size is the bytes in .git, counted at the first write; -1 until then.
	size int64
	// anc is main's commits, which tell a read whether a commit is one of them.
	anc ancestry
	// unv is the bytes of the unversioned files, counted when a write needs them; -1 when
	// unknown (usage.go). Only a writer, holding mu, reads or sets it.
	unv int64
	// removed is set by RemoveSpace, under mu, and read by every call under mu once it has the
	// lock: a call that took the state before the removal and got the lock after it sees it here,
	// and so does one that arrives later, so nothing makes the space's directory again. It stays
	// set, in the Store's map, until ForgetSpace.
	removed bool
	// removedGen is the generation of the space that was removed (access.Space.Generation; 0 when
	// the caller didn't have it). A space made later with the same ID has another, so the mark
	// never refuses it, whether or not ForgetSpace has run.
	removedGen int64
	// lastGen is the generation of the space whose files this state has been used for (0 until a
	// call that knows its space's generation has been let in): RemoveSpace leaves the directory
	// alone when it is called for another space that once had the ID, since the files there are
	// not that one's. Atomic, since holders and readers set it under the shared lock.
	lastGen atomic.Int64

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

// SetSpaceCheck sets the check of a space's row that every call makes once it has the space's
// lock: false is a space that is gone, or is not the one the caller was authorized for (a space
// made later with a purged one's ID), and is refused as ErrSpaceRemoved. It closes the gap that is
// left when the purge has deleted the rows and the Store has forgotten the removal (ForgetSpace), for
// a call that resolved the space before: it would make the directory again. It is for the server's
// start, before the Store is used (access.SpaceRowCheck); without one, the removal's mark is all
// there is.
func (s *Store) SetSpaceCheck(exists func(sp access.Space) (bool, error)) { s.spaceExists = exists }

// admit is what every call that has st's lock asks first: it refuses sp when it was removed (and is
// the space that was: a space made later with the same ID, whose generation differs, is not
// refused by the mark) or when its row is gone.
func (s *Store) admit(st *spaceState, sp access.Space) error {
	if st.removed && (st.removedGen == 0 || sp.Generation == 0 || sp.Generation == st.removedGen) {
		return ErrSpaceRemoved
	}
	if s.spaceExists != nil {
		ok, err := s.spaceExists(sp)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSpaceRemoved
		}
	}
	if sp.Generation != 0 {
		st.lastGen.Store(sp.Generation)
	}
	return nil
}

// refusedIfRemoved is err, or ErrSpaceRemoved when err is a missing file and the space was removed
// meanwhile: an upload staged its bytes, and the removal took them with the rest.
func (s *Store) refusedIfRemoved(sp access.Space, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	st := s.lockSpace(sp, false, false)
	defer st.mu.RUnlock()
	if refused := s.admit(st, sp); refused != nil {
		return refused
	}
	return err
}

// lockSpace takes the lock of sp's state, for writing or reading, and returns the state with the
// lock held, once it is the state the Store has for the space. A state that was forgotten
// (ForgetSpace) while its lock was waited for is left, and the current one is taken: a call must
// never hold the lock of a state that no one else will find, or it would run beside a call that holds
// the lock of the new one. hook is for the calls whose test pauses them before the lock
// (Store.beforeLock).
func (s *Store) lockSpace(sp access.Space, write, hook bool) *spaceState {
	for {
		st := s.state(sp.ID)
		if hook && s.beforeLock != nil {
			s.beforeLock(sp)
		}
		if write {
			st.mu.Lock()
		} else {
			st.mu.RLock()
		}
		s.mu.Lock()
		current := s.spaces[sp.ID] == st
		s.mu.Unlock()
		if current {
			return st
		}
		if write {
			st.mu.Unlock()
		} else {
			st.mu.RUnlock()
		}
	}
}

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
func (s *Store) Read(ctx context.Context, sp access.Space, name, at string) (Doc, error) {
	if err := checkPath(name); err != nil {
		return Doc{}, err
	}
	var doc Doc
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(ctx, at)
		if err != nil {
			return err
		}
		doc, err = r.readDoc(c, name)
		return err
	})
	return doc, err
}

// ReadText returns the text file name of sp, as it is now or as it was at the commit at, for a
// reader that wants text and nothing else (the agent's read_doc). What the file is is decided
// from its metadata, under the same read lock as the read, before any content is loaded: an
// unversioned file, and a file other than Markdown over maxSize bytes, are ErrInvalid without
// their bytes being read. A file with a NUL byte or that isn't valid UTF-8 is ErrInvalid too,
// whatever its name says, so what is returned is always text that reads back as it was written.
func (s *Store) ReadText(ctx context.Context, sp access.Space, name, at string, maxSize int64) (Doc, error) {
	if err := checkPath(name); err != nil {
		return Doc{}, err
	}
	var doc Doc
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(ctx, at)
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
		entry, p, err := r.describe(name, e.Hash) // reads a blob only if it is small enough to be a pointer
		if err != nil {
			return err
		}
		switch {
		case p != nil:
			// Its bytes are on disk, not in git, and can be any size: only list_docs shows it.
			return fmt.Errorf("%w: %s is an unversioned file (a large file, audio, video or an archive): it can't be read as text", ErrInvalid, name)
		case !IsMarkdown(name) && entry.Size > maxSize:
			return fmt.Errorf("%w: %s is %d bytes: only text files up to %d KiB can be read", ErrInvalid, name, entry.Size, maxSize>>10)
		}
		content, err := r.blobBytes(e.Hash)
		if err != nil {
			return err
		}
		if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
			return fmt.Errorf("%w: %s isn't a text file: only Markdown documents and plain-text files can be read", ErrInvalid, name)
		}
		doc = Doc{Entry: entry, Content: content}
		return nil
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
			return Result{}, s.refusedIfRemoved(sp, err)
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
	// The quota is checked in here, once the bytes of the file it replaces are known: a replacement
	// or a shrink near the cap is a change of its net size, not of the new file's.
	return s.putCore(sp, name, maxPointerSize, false, base, by, message, nil, func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error) {
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
			// What the change adds to the space: the file's bytes, the replaced file's taken off,
			// and the commit and its trees.
			if err := s.reserveQuota(r, sp, sg.ptr.size-freed+changeOverhead); err != nil {
				return plumbing.ZeroHash, err
			}
			if err := r.adopt(sg); err != nil {
				return plumbing.ZeroHash, err
			}
		} else if err := s.reserveQuota(r, sp, changeOverhead); err != nil {
			// The bytes are in the space already (a path points to them): only the commit is new.
			return plumbing.ZeroHash, err
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
	return s.putCore(sp, name, adding, true, base, by, message, guard, store)
}

// putCore is putIf; the storage quota is checked before store runs for adding bytes if
// quotaFirst, and by store itself if not (an unversioned file, whose net size it knows).
func (s *Store) putCore(sp access.Space, name string, adding int64, quotaFirst bool, base string, by Author, message string, guard func() error, store func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error)) (Result, error) {
	var res Result
	err := s.writeQ(sp, adding, quotaFirst, true, func(r *repo) error {
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
func (s *Store) Open(ctx context.Context, sp access.Space, name, at string) (Entry, File, error) {
	if err := checkPath(name); err != nil {
		return Entry{}, nil, err
	}
	var entry Entry
	var file File
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(ctx, at)
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
func (s *Store) Move(ctx context.Context, sp access.Space, from, to, base string, by Author, message string) (Result, error) {
	return s.move(ctx, sp, from, to, base, by, message, false)
}

// MoveKept is Move for a caller that can't ask a person first (the agent's tools): it refuses,
// under the same lock as the move, one that would change a file between versioned and
// unversioned storage, as renaming draft.md to draft.zip would: the file's older versions leave
// history. The error is ErrInvalid; a person does such a move in the web app.
func (s *Store) MoveKept(ctx context.Context, sp access.Space, from, to, base string, by Author, message string) (Result, error) {
	return s.move(ctx, sp, from, to, base, by, message, true)
}

func (s *Store) move(_ context.Context, sp access.Space, from, to, base string, by Author, message string, kept bool) (Result, error) {
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
				if kept {
					return fmt.Errorf("%w: moving %s to %s would make it an unversioned file, kept outside git, and take its older versions out of history for good; keep a name of the same kind (the same kind of extension), or ask the person to rename it in the web app",
						ErrInvalid, from, to)
				}
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
func (s *Store) Delete(ctx context.Context, sp access.Space, name, base string, by Author, message string) (Result, error) {
	return s.delete(ctx, sp, name, base, by, message, false)
}

// DeleteKept is Delete for a caller that can't ask a person first (the agent's tools): it
// refuses an unversioned file, whose bytes a delete removes for good, under the same lock as
// the delete. A versioned document's history keeps it. The error is ErrInvalid.
func (s *Store) DeleteKept(ctx context.Context, sp access.Space, name, base string, by Author, message string) (Result, error) {
	return s.delete(ctx, sp, name, base, by, message, true)
}

func (s *Store) delete(_ context.Context, sp access.Space, name, base string, by Author, message string, kept bool) (Result, error) {
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
		if kept {
			if _, p, err := r.describe(name, cur.Hash); err != nil {
				return err
			} else if p != nil {
				return fmt.Errorf("%w: %s is an unversioned file (a large file, audio, video or an archive): deleting it removes its bytes for good and no tool can bring them back; ask the person to delete it in the web app", ErrInvalid, name)
			}
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
func (s *Store) Restore(ctx context.Context, sp access.Space, name, at, base string, by Author, message string) (Result, error) {
	return s.restore(ctx, sp, name, at, base, by, message, false)
}

// RestoreKept is Restore for a caller that can't ask a person first (the agent's tools): under the
// same lock as the restore, it refuses one that would throw bytes away or bring back what
// shouldn't be. It won't replace an unversioned file with different content, since a restore
// would leave the current bytes unreferenced and they would be removed for good, nor restore a
// pointer where the file would be versioned (a Markdown document, or a name of a versioned kind).
// The error is ErrInvalid; a person restores such a file in the web app.
func (s *Store) RestoreKept(ctx context.Context, sp access.Space, name, at, base string, by Author, message string) (Result, error) {
	return s.restore(ctx, sp, name, at, base, by, message, true)
}

func (s *Store) restore(ctx context.Context, sp access.Space, name, at, base string, by Author, message string, kept bool) (Result, error) {
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
		c, err := r.revision(ctx, at)
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
		oldEntry, oldPtr, err := r.describe(name, old.Hash)
		if err != nil {
			return err
		}
		if oldPtr != nil && !r.hasObject(oldPtr.oid) {
			return fmt.Errorf("%w: %s at %s", ErrNotKept, name, at)
		}
		if kept && oldPtr != nil && !s.unversioned(name, oldEntry.Size) {
			return fmt.Errorf("%w: %s at %s was an unversioned file, and under this name it would be a versioned one; restoring it would make a document of a pointer", ErrInvalid, name, at)
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
		} else if kept && cur.Hash != old.Hash {
			if _, curPtr, err := r.describe(name, cur.Hash); err != nil {
				return err
			} else if curPtr != nil && (oldPtr == nil || oldPtr.oid != curPtr.oid) {
				return fmt.Errorf("%w: %s is an unversioned file and restoring %s would replace it with different content, removing its current bytes for good; ask the person to restore it in the web app", ErrInvalid, name, at[:7])
			}
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
	st := s.lockSpace(sp, false, false)
	defer st.mu.RUnlock()
	if err := s.admit(st, sp); err != nil {
		return err
	}
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
	return s.writeQ(sp, adding, true, create, fn)
}

// writeQ is write; quotaFirst says whether the storage quotas are checked, for adding bytes,
// before fn runs. A change that knows better what it adds checks them itself (Store.reserveQuota).
func (s *Store) writeQ(sp access.Space, adding int64, quotaFirst, create bool, fn func(*repo) error) error {
	err := s.writeLocked(sp, adding, quotaFirst, create, fn)
	s.changed(sp)
	return err
}

func (s *Store) writeLocked(sp access.Space, adding int64, quotaFirst, create bool, fn func(*repo) error) error {
	st := s.lockSpace(sp, true, true)
	defer st.mu.Unlock()
	if err := s.admit(st, sp); err != nil {
		return err
	}
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
	// is published again once the write is over. What the change reserved of the quotas is held
	// until then, so no other write counts the space as smaller than it is, and given back once
	// it is published.
	defer func() {
		s.publishUsage(r)
		for _, release := range r.releases {
			release()
		}
	}()
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
	if adding > 0 && quotaFirst {
		if err := s.reserveQuota(r, sp, adding+changeOverhead); err != nil {
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
