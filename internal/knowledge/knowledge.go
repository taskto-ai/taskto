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
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

const (
	DefaultMaxFileSize = 2 << 30
	DefaultMaxRepoSize = 1 << 30
)

const (
	MaxDepth            = 16
	MaxPathLen          = 1024
	MaxNameLen          = 255
	MaxListLimit        = 1000
	DefaultListLimit    = 200
	MaxHistoryLimit     = 200
	DefaultHistoryLimit = 50
	MaxMessageLen       = 200
)

type Options struct {
	MaxFileSize        int64
	LargeFileSize      int64
	MaxRepoSize        int64
	MaxUnversionedSize int64
}

var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrTooLarge = errors.New("file too large")
	ErrRepoFull = errors.New("the space's knowledge base is full")
	ErrReadOnly = errors.New("you can only read this space")

	ErrSpaceRemoved = fmt.Errorf("%w: the space was removed", access.ErrNotFound)

	errNoRepo = fmt.Errorf("%w: this space has no documents yet", ErrNotFound)
)

type codedError struct {
	error
	code   apierr.Code
	params apierr.Params
}

func (e *codedError) Unwrap() error { return e.error }

func errorf(code apierr.Code, params apierr.Params, format string, args ...any) error {
	return &codedError{error: fmt.Errorf(format, args...), code: code, params: params}
}

func pathParams(name string) apierr.Params { return apierr.Params{"path": name} }

type Author struct {
	Name  string
	Email string
	Agent string
}

type Entry struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	Versioned bool   `json:"versioned"`
}

type Doc struct {
	Entry
	Content []byte
}

type Page struct {
	Docs []Entry `json:"docs"`
	Next string  `json:"next,omitempty"`
}

type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Commit struct {
	Commit    string `json:"commit"`
	Author    Person `json:"author"`
	Agent     string `json:"agent,omitempty"`
	Time      string `json:"time"`
	Message   string `json:"message"`
	Version   string `json:"version,omitempty"`
	Change    string `json:"change,omitempty"`
	Versioned *bool  `json:"versioned,omitempty"`
}

type History struct {
	Commits []Commit `json:"commits"`
	Next    string   `json:"next,omitempty"`
}

type Result struct {
	Commit string `json:"commit"`
	Doc    *Entry `json:"doc,omitempty"`
}

type Store struct {
	dataDir string
	opts    Options

	mu     sync.Mutex
	spaces map[int64]*spaceState

	onChange []func(access.Space)

	quota func(sp access.Space, adding int64) (release func(), err error)

	afterMeasure func(sp access.Space)

	spaceExists func(sp access.Space) (bool, error)

	beforeLock func(sp access.Space)
}

type spaceState struct {
	mu             sync.RWMutex
	size           int64
	anc            ancestry
	unv            int64
	removed        bool
	removedCreated string

	cacheMu sync.Mutex
	cache   usageCache
}

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

func (s *Store) MaxFileSize() int64 { return s.opts.MaxFileSize }

func (s *Store) SetSpaceCheck(exists func(sp access.Space) (bool, error)) { s.spaceExists = exists }

func (s *Store) admit(st *spaceState, sp access.Space) error {
	if st.removed && (st.removedCreated == "" || sp.CreatedAt == "" || sp.CreatedAt == st.removedCreated) {
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
	return nil
}

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

func (s *Store) List(ctx context.Context, sp access.Space, prefix, after string, limit int) (Page, error) {
	return s.ListAt(ctx, sp, "", prefix, after, limit)
}

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
		entry, p, err := r.describe(name, e.Hash)
		if err != nil {
			return err
		}
		switch {
		case p != nil:
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

func (s *Store) Put(_ context.Context, sp access.Space, name string, content []byte, base string, by Author, message string) (Result, error) {
	if err := s.checkWrite(sp, name, content, by, message); err != nil {
		return Result{}, err
	}
	if err := checkVersion(base); err != nil {
		return Result{}, err
	}
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

func (s *Store) putUnversioned(sp access.Space, name string, sg *staged, base string, by Author, message string) (Result, error) {
	return s.putCore(sp, name, maxPointerSize, false, base, by, message, nil, func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error) {
		r.collect = true
		if !r.hasObject(sg.ptr.oid) {
			used, err := r.unversionedSize()
			if err != nil {
				return plumbing.ZeroHash, err
			}
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
			if err := s.reserveQuota(r, sp, sg.ptr.size-freed+changeOverhead); err != nil {
				return plumbing.ZeroHash, err
			}
			if err := r.adopt(sg); err != nil {
				return plumbing.ZeroHash, err
			}
		} else if err := s.reserveQuota(r, sp, changeOverhead); err != nil {
			return plumbing.ZeroHash, err
		}
		return r.storeBlob(sg.ptr.encode())
	})
}

func (s *Store) put(sp access.Space, name string, adding int64, base string, by Author, message string, store func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error)) (Result, error) {
	return s.putIf(sp, name, adding, base, by, message, nil, store)
}

func (s *Store) putIf(sp access.Space, name string, adding int64, base string, by Author, message string, guard func() error, store func(r *repo, cur *object.TreeEntry) (plumbing.Hash, error)) (Result, error) {
	return s.putCore(sp, name, adding, true, base, by, message, guard, store)
}

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
			r.collect = r.collect || !IsMarkdown(name)
		}
		blob, err := store(r, cur)
		if err != nil {
			return err
		}
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

type File interface {
	io.ReadSeeker
	io.Closer
}

type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

func (s *Store) Move(ctx context.Context, sp access.Space, from, to, base string, by Author, message string) (Result, error) {
	return s.move(ctx, sp, from, to, base, by, message, false)
}

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
				r.collect = true
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

func (s *Store) Delete(ctx context.Context, sp access.Space, name, base string, by Author, message string) (Result, error) {
	return s.delete(ctx, sp, name, base, by, message, false)
}

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

func (s *Store) Restore(ctx context.Context, sp access.Space, name, at, base string, by Author, message string) (Result, error) {
	return s.restore(ctx, sp, name, at, base, by, message, false)
}

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

const changeOverhead = 1 << 10

func (s *Store) write(sp access.Space, adding int64, create bool, fn func(*repo) error) error {
	return s.writeQ(sp, adding, true, create, fn)
}

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
	if err := r.repair(); err != nil {
		return err
	}
	if _, err := r.resumeRewrite(sp); err != nil {
		slog.Warn("knowledge: taking a file's old versions out of history; it is retried at the next change", "space", sp.ID, "err", err)
	}
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
	if adding >= 0 && size+adding+changeOverhead > s.opts.MaxRepoSize {
		return fmt.Errorf("%w (limit %d bytes)", ErrRepoFull, s.opts.MaxRepoSize)
	}
	if adding > 0 && quotaFirst {
		if err := s.reserveQuota(r, sp, adding+changeOverhead); err != nil {
			return err
		}
	}
	err = fn(r)
	if r.collect {
		if gcErr := r.gc(); gcErr != nil {
			slog.Warn("knowledge: removing unversioned files' old bytes", "space", sp.ID, "err", gcErr)
		}
	}
	return err
}

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
		limit = s.opts.LargeFileSize
	}
	if int64(len(content)) > limit {
		return errorf(apierr.CodeFileTooLarge, apierr.Params{"path": name, "max": limit}, "%w: %s is over %d bytes", ErrTooLarge, name, limit)
	}
	if IsMarkdown(name) && !utf8.Valid(content) {
		return errorf(apierr.CodeDocNotUTF8, pathParams(name), "%w: %s isn't UTF-8 text", ErrInvalid, name)
	}
	if _, isPtr := parsePointer(content); isPtr && IsMarkdown(name) {
		return fmt.Errorf("%w: a Markdown document can't be exactly a Git LFS pointer", ErrInvalid)
	}
	if strings.TrimSpace(by.Name) == "" || strings.TrimSpace(by.Email) == "" {
		return fmt.Errorf("%w: a change needs its author's name and email", ErrInvalid)
	}
	return checkMessage(message)
}

func IsMarkdown(name string) bool { return strings.EqualFold(path.Ext(name), ".md") }

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

const timeFormat = "2006-01-02T15:04:05.000Z"

var now = time.Now
