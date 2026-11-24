package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

const (
	gitDir = ".git"
	// branch is the one branch: every change is a commit on it.
	branch = plumbing.ReferenceName("refs/heads/main")
)

// repo is a space's repository, open for one call of the Store.
type repo struct {
	root  *os.Root // the space's directory, the working tree
	st    *filesystem.Storage
	state *spaceState
	// added are the objects this call stored, for the size count.
	added []plumbing.Hash
	// collect is set by a change that can leave unversioned files' bytes nothing points
	// to: the write runs gc after it.
	collect bool
	// releases give back what the write reserved of the storage quotas (Store.reserveQuota),
	// once the usage is published.
	releases []func()
}

// openRepo opens sp's repository. Without one, it creates it if create is set, and
// returns nil otherwise.
func openRepo(dataDir string, sp access.Space, state *spaceState, create bool) (*repo, error) {
	root, err := access.OpenRoot(dataDir, sp)
	if err != nil {
		return nil, err
	}
	fi, err := root.Lstat(gitDir)
	exists := err == nil
	switch {
	case errors.Is(err, fs.ErrNotExist) && !create:
		_ = root.Close()
		return nil, nil
	case errors.Is(err, fs.ErrNotExist):
		err = root.Mkdir(gitDir, 0o700)
	case err == nil && !fi.IsDir():
		err = fmt.Errorf("knowledge: %s in space %d isn't a directory", gitDir, sp.ID)
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	r := &repo{
		root:  root,
		st:    filesystem.NewStorage(rootFS{root: root, prefix: gitDir}, cache.NewObjectLRUDefault()),
		state: state,
	}
	if !exists {
		if err := r.init(); err != nil {
			r.close()
			return nil, fmt.Errorf("knowledge: create the repository of space %d: %w", sp.ID, err)
		}
	}
	return r, nil
}

// init makes the new .git a repository whose HEAD is main, with the space's directory as
// its working tree.
func (r *repo) init() error {
	if err := r.st.Init(); err != nil {
		return err
	}
	cfg := config.NewConfig()
	cfg.Core.IsBare = false
	if err := r.st.SetConfig(cfg); err != nil {
		return err
	}
	r.state.size = -1
	return r.st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch))
}

func (r *repo) close() {
	_ = r.st.Close()
	_ = r.root.Close()
}

// head returns main's last commit, or nil before the first.
func (r *repo) head() (*object.Commit, error) {
	ref, err := r.st.Reference(branch)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return object.GetCommit(r.st, ref.Hash())
}

// revision returns the commit at, or the head if at is empty.
func (r *repo) revision(at string) (*object.Commit, error) {
	if at == "" {
		return r.head()
	}
	if !isHash(at) {
		return nil, fmt.Errorf("%w: %q isn't a commit ID", ErrInvalid, at)
	}
	c, err := object.GetCommit(r.st, plumbing.NewHash(at))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, fmt.Errorf("%w: commit %s", ErrNotFound, at)
	}
	return c, err
}

// entryAt finds name in c's tree, walking it one element at a time. A nil c is the empty
// tree.
func (r *repo) entryAt(c *object.Commit, name string) (*object.TreeEntry, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	elems := strings.Split(name, "/")
	for i, elem := range elems {
		e := findEntry(tree.Entries, elem)
		if e == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		if i == len(elems)-1 {
			return e, nil
		}
		if e.Mode != filemode.Dir {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		if tree, err = object.GetTree(r.st, e.Hash); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func findEntry(entries []object.TreeEntry, name string) *object.TreeEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// readDoc returns the document name in c.
func (r *repo) readDoc(c *object.Commit, name string) (Doc, error) {
	e, err := r.entryAt(c, name)
	if err != nil {
		return Doc{}, err
	}
	if e.Mode == filemode.Dir {
		return Doc{}, fmt.Errorf("%w: %s is a folder", ErrNotFound, name)
	}
	entry, p, err := r.describe(name, e.Hash)
	if err != nil || p != nil {
		return Doc{Entry: entry}, err // an unversioned file: Open reads its bytes
	}
	content, err := r.blobBytes(e.Hash)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Entry: entry, Content: content}, nil
}

// storeBlob stores content and returns its hash.
func (r *repo) storeBlob(content []byte) (plumbing.Hash, error) {
	obj := r.st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.store(obj)
}

// store stores obj, unless the repository has it already.
func (r *repo) store(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	h := obj.Hash()
	if r.st.HasEncodedObject(h) == nil {
		return h, nil
	}
	if _, err := r.st.SetEncodedObject(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	r.added = append(r.added, h)
	return h, nil
}

// edit sets a file of the tree to a blob, or removes it if hash is zero.
type edit struct {
	name string
	hash plumbing.Hash
}

// moveEdits are the edits that move cur, the entry at from, to to: one for a document,
// two for each document of a folder.
func (r *repo) moveEdits(cur *object.TreeEntry, from, to string) ([]edit, error) {
	if cur.Mode != filemode.Dir {
		return []edit{{name: from}, {name: to, hash: cur.Hash}}, nil
	}
	var removes, adds []edit
	err := r.walk(cur.Hash, from, func(name string, e object.TreeEntry) error {
		dest := to + strings.TrimPrefix(name, from)
		// The destination folder passed the checks, but what's inside it can still end up
		// too deep or too long: every document's new path must be one we'd accept.
		if err := checkPath(dest); err != nil {
			return err
		}
		removes = append(removes, edit{name: name})
		adds = append(adds, edit{name: dest, hash: e.Hash})
		return nil
	})
	return append(removes, adds...), err
}

// change commits edits on top of head, then brings the working tree and the index up to
// date, and returns the commit. Edits apply in order. If they change nothing, nothing is
// committed and the commit is the head.
func (r *repo) change(head *object.Commit, edits []edit, by Author, message string) (_ string, err error) {
	// Any failure may leave stored objects behind (a blob before a path conflict, say):
	// measure .git again at the next write rather than trust the cached size.
	defer func() {
		if err != nil {
			r.recount()
		}
	}()
	root := plumbing.ZeroHash
	if head != nil {
		root = head.TreeHash
	}
	for _, e := range edits {
		var err error
		if root, err = r.setPath(root, strings.Split(e.name, "/"), e.name, e.hash); err != nil {
			return "", err
		}
	}
	if root.IsZero() {
		// The last document went: an empty tree, which git stores like any other.
		var err error
		if root, err = r.storeTree(nil); err != nil {
			return "", err
		}
	}
	if head != nil && root == head.TreeHash {
		return head.Hash.String(), nil
	}
	// The commit object first, then the files and the index, and main moves last: a
	// failure on the way leaves main where it was and the files as they were, so the
	// change either happened completely or not at all, and can simply be retried.
	commit, old, err := r.newCommit(head, root, by, message)
	if err != nil {
		return "", err
	}
	// Recorded first, so a crash before main moves can be undone by the next write.
	if err := r.markPending(edits); err != nil {
		return "", err
	}
	if err := r.checkout(edits); err != nil {
		return "", errors.Join(fmt.Errorf("knowledge: updating the files: %w", err), r.undo(head, edits))
	}
	if err := r.setMain(commit, old); err != nil {
		return "", errors.Join(err, r.undo(head, edits))
	}
	r.clearPending()
	return commit.String(), nil
}

// result is a change's Result, with the document name at the blob h.
func (r *repo) result(commit, name string, h plumbing.Hash) (Result, error) {
	e, _, err := r.describe(name, h)
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: commit, Doc: &e}, nil
}

// setPath returns the hash of the tree treeHash (zero for none) with the file at elems
// set to blob, or removed if blob is zero. A tree left empty is the zero hash. name is
// the whole path, for errors.
func (r *repo) setPath(treeHash plumbing.Hash, elems []string, name string, blob plumbing.Hash) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	if !treeHash.IsZero() {
		tree, err := object.GetTree(r.st, treeHash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = slices.Clone(tree.Entries)
	}
	i := slices.IndexFunc(entries, func(e object.TreeEntry) bool { return e.Name == elems[0] })
	if i < 0 {
		if blob.IsZero() {
			return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		// Two names that differ only by case are one file on macOS and Windows.
		if j := slices.IndexFunc(entries, func(e object.TreeEntry) bool { return strings.EqualFold(e.Name, elems[0]) }); j >= 0 {
			return plumbing.ZeroHash, errorf(apierr.CodeDocCaseConflict, apierr.Params{"name": elems[0], "other": entries[j].Name},
				"%w: %q differs only by case from %q, which exists", ErrConflict, elems[0], entries[j].Name)
		}
	}

	var e object.TreeEntry
	if len(elems) == 1 {
		if i >= 0 && entries[i].Mode == filemode.Dir {
			return plumbing.ZeroHash, errorf(apierr.CodeDocPathTaken, pathParams(name), "%w: %s is a folder", ErrConflict, name)
		}
		e = object.TreeEntry{Name: elems[0], Mode: filemode.Regular, Hash: blob}
	} else {
		sub := plumbing.ZeroHash
		if i >= 0 {
			if entries[i].Mode != filemode.Dir {
				return plumbing.ZeroHash, errorf(apierr.CodeDocPathTaken, pathParams(name), "%w: a file is in the way of %s", ErrConflict, name)
			}
			sub = entries[i].Hash
		}
		h, err := r.setPath(sub, elems[1:], name, blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		e = object.TreeEntry{Name: elems[0], Mode: filemode.Dir, Hash: h}
	}

	switch {
	case e.Hash.IsZero():
		entries = slices.Delete(entries, i, i+1)
	case i >= 0:
		entries[i] = e
	default:
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return plumbing.ZeroHash, nil
	}
	return r.storeTree(entries)
}

// storeTree stores a tree of entries, in git's order: by name, a folder's name followed
// by a slash.
func (r *repo) storeTree(entries []object.TreeEntry) (plumbing.Hash, error) {
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	slices.SortFunc(entries, func(a, b object.TreeEntry) int { return strings.Compare(key(a), key(b)) })
	obj := r.st.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.store(obj)
}

// newCommit stores a commit of tree on top of parent (nil for the first), by by. It
// returns the commit and the reference main must still be at for setMain to move it.
func (r *repo) newCommit(parent *object.Commit, tree plumbing.Hash, by Author, message string) (plumbing.Hash, *plumbing.Reference, error) {
	author := object.Signature{Name: sigText(by.Name), Email: sigText(by.Email), When: now().UTC()}
	committer := author
	if by.Agent != "" {
		committer.Name = sigText(by.Agent)
	}
	c := &object.Commit{Author: author, Committer: committer, Message: message + "\n", TreeHash: tree}
	var old *plumbing.Reference
	if parent != nil {
		c.ParentHashes = []plumbing.Hash{parent.Hash}
		old = plumbing.NewHashReference(branch, parent.Hash)
	}
	obj := r.st.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	h, err := r.store(obj)
	return h, old, err
}

// setMain moves main to the commit h, if main is still at old.
func (r *repo) setMain(h plumbing.Hash, old *plumbing.Reference) error {
	if err := r.st.CheckAndSetReference(plumbing.NewHashReference(branch, h), old); err != nil {
		return err
	}
	r.countAdded()
	return nil
}

// undo rolls back a change that failed in this process. The pending mark stays if the
// files couldn't all be put back, so the next write repairs them.
func (r *repo) undo(head *object.Commit, edits []edit) error {
	r.recount()
	err := r.rollback(head, edits)
	if err == nil {
		r.clearPending()
	}
	return err
}

// rollback puts the files the edits touched back as they are in head, after a change
// that didn't complete. The index is only written once every file is, so it still
// matches head. It carries on past a failure, and returns them all.
func (r *repo) rollback(head *object.Commit, edits []edit) error {
	var errs []error
	seen := map[string]bool{}
	for _, e := range edits {
		if seen[e.name] {
			continue
		}
		seen[e.name] = true
		var h plumbing.Hash
		if head != nil {
			var err error
			if h, err = r.blobAt(head, e.name); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if h.IsZero() {
			errs = append(errs, r.removeFile(e.name))
		} else {
			_, err := r.writeFile(e.name, h)
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("knowledge: putting the files back: %w", err)
	}
	return nil
}

// sigText makes s safe in a commit's author line, where angle brackets and line breaks
// would end the name or the email early.
func sigText(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

// checkout writes the edits to the working tree, through the space's os.Root, and to the
// index, so the directory is a clean checkout of main for anyone reading it, git
// included.
func (r *repo) checkout(edits []edit) error {
	idx, err := r.st.Index()
	if err != nil {
		return err
	}
	for _, e := range edits {
		idx.Entries = slices.DeleteFunc(idx.Entries, func(ie *index.Entry) bool { return ie.Name == e.name })
		if e.hash.IsZero() {
			if err := r.removeFile(e.name); err != nil {
				return err
			}
			continue
		}
		size, err := r.writeFile(e.name, e.hash)
		if err != nil {
			return err
		}
		// No stat data: git sees the file as possibly changed and compares its content.
		idx.Entries = append(idx.Entries, &index.Entry{Name: e.name, Hash: e.hash, Mode: filemode.Regular, Size: uint32(min(size, 1<<32-1))}) // #nosec G115 -- clamped
	}
	slices.SortFunc(idx.Entries, func(a, b *index.Entry) int { return strings.Compare(a.Name, b.Name) })
	return r.st.SetIndex(idx)
}

// writeFile writes the blob h to the file name, replacing it at once: readers see the
// old content or the new, never part of it.
func (r *repo) writeFile(name string, h plumbing.Hash) (int64, error) {
	blob, err := object.GetBlob(r.st, h)
	if err != nil {
		return 0, err
	}
	rd, err := blob.Reader()
	if err != nil {
		return 0, err
	}
	defer rd.Close()
	dir := path.Dir(name)
	if err := r.root.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	tmp := path.Join(dir, ".taskto-"+randomSuffix()+".tmp")
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(f, rd)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.root.Rename(tmp, name)
	}
	if err != nil {
		_ = r.root.Remove(tmp)
		return 0, err
	}
	return blob.Size, nil
}

// removeFile removes the file name, and the folders it leaves empty.
func (r *repo) removeFile(name string) error {
	if err := r.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Remove fails on the first folder that isn't empty.
	dir := path.Dir(name)
	for dir != "." && r.root.Remove(dir) == nil {
		dir = path.Dir(dir)
	}
	return nil
}

// size returns the bytes in .git, but for .git/lfs (unversioned files have a limit of
// their own), counting them the first time.
func (r *repo) size() (int64, error) {
	if r.state.size >= 0 {
		return r.state.size, nil
	}
	n, err := gitSize(r.root.FS())
	if err != nil {
		return 0, err
	}
	r.state.size = n
	return n, nil
}

// gitSize is the bytes under .git in fsys, the unversioned files' (.git/lfs) apart.
func gitSize(fsys fs.FS) (int64, error) {
	var n int64
	err := fs.WalkDir(fsys, gitDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p == lfsDir {
			return fs.SkipDir
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n += fi.Size()
		return nil
	})
	return n, err
}

// pendingFile lists, one per line, the files a change in progress is writing. It exists
// only while a change runs: finding it means the last process stopped mid-change.
var pendingFile = path.Join(gitDir, "taskto-pending")

func (r *repo) markPending(edits []edit) error {
	var b strings.Builder
	for _, e := range edits {
		b.WriteString(e.name)
		b.WriteByte('\n')
	}
	return r.root.WriteFile(pendingFile, []byte(b.String()), 0o600)
}

func (r *repo) clearPending() { _ = r.root.Remove(pendingFile) }

// repair undoes a change the last process left half done: the files it listed, and
// their index entries, go back to what main holds. main moves last, so it is the truth:
// a change that got as far as moving main is simply completed.
func (r *repo) repair() error {
	raw, err := r.root.ReadFile(pendingFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	head, err := r.head()
	if err != nil {
		return err
	}
	var edits []edit
	for _, name := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if name == "" || checkPath(name) != nil {
			continue // never act on a name a change couldn't have made
		}
		var h plumbing.Hash
		if head != nil {
			if h, err = r.blobAt(head, name); err != nil {
				return err
			}
		}
		edits = append(edits, edit{name: name, hash: h})
	}
	if err := r.checkout(edits); err != nil {
		return fmt.Errorf("knowledge: repairing an unfinished change: %w", err)
	}
	r.collect = true // it may have adopted an upload's bytes
	r.recount()
	r.clearPending()
	return nil
}

// recount drops the cached size after a change that didn't complete: the objects it
// stored are on disk but weren't counted, so the next write measures .git again.
func (r *repo) recount() {
	r.added = nil
	r.state.size = -1
}

// countAdded adds the loose objects this call stored to the size count.
func (r *repo) countAdded() {
	if r.state.size < 0 {
		return // counted in full at the next write
	}
	for _, h := range r.added {
		hex := h.String()
		if fi, err := r.root.Lstat(path.Join(gitDir, "objects", hex[:2], hex[2:])); err == nil {
			r.state.size += fi.Size()
		}
	}
	r.added = nil
}

// walk calls fn for every file under the tree h, whose path is dir, in path order.
func (r *repo) walk(h plumbing.Hash, dir string, fn func(name string, e object.TreeEntry) error) error {
	tree, err := object.GetTree(r.st, h)
	if err != nil {
		return err
	}
	for _, e := range tree.Entries {
		name := path.Join(dir, e.Name)
		if e.Mode == filemode.Dir {
			err = r.walk(e.Hash, name, fn)
		} else {
			err = fn(name, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// errPageFull stops a walk once a page has one entry more than it shows.
var errPageFull = errors.New("page full")

// list returns the documents under the folder prefix, after the path after, at most
// limit of them, as of the commit at (the head if empty). A tree's entries are in git's order,
// which is the order of the full paths, so a walk lists them sorted and can skip the folders
// before after.
func (r *repo) list(ctx context.Context, at, prefix, after string, limit int) (Page, error) {
	head, err := r.revision(at)
	if err != nil || head == nil {
		return Page{}, err
	}
	start := head.TreeHash
	if prefix != "" {
		e, err := r.entryAt(head, prefix)
		if err != nil {
			return Page{}, err
		}
		if e.Mode != filemode.Dir {
			return Page{}, fmt.Errorf("%w: %s isn't a folder", ErrNotFound, prefix)
		}
		start = e.Hash
	}

	var docs []Entry
	var visit func(h plumbing.Hash, dir string) error
	visit = func(h plumbing.Hash, dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tree, err := object.GetTree(r.st, h)
		if err != nil {
			return err
		}
		for _, e := range tree.Entries {
			name := path.Join(dir, e.Name)
			if e.Mode == filemode.Dir {
				if after > name+"/" && !strings.HasPrefix(after, name+"/") {
					continue // every path in it sorts before after
				}
				if err := visit(e.Hash, name); err != nil {
					return err
				}
				continue
			}
			if name <= after {
				continue
			}
			if len(docs) == limit {
				return errPageFull
			}
			entry, _, err := r.describe(name, e.Hash)
			if err != nil {
				return err
			}
			docs = append(docs, entry)
		}
		return nil
	}
	page := Page{}
	switch err := visit(start, prefix); {
	case errors.Is(err, errPageFull):
		page.Next = docs[len(docs)-1].Path
	case err != nil:
		return Page{}, err
	}
	page.Docs = docs
	return page, nil
}

// history walks main's commits from the head, or from the parent of before, and returns
// those that changed name (all of them if name is empty), at most limit.
func (r *repo) history(ctx context.Context, name, before string, limit int) (History, error) {
	c, err := r.head()
	if err != nil || c == nil {
		return History{}, err
	}
	if before != "" {
		if c, err = r.revision(before); err != nil {
			return History{}, err
		}
		if c, err = r.parent(c); err != nil {
			return History{}, err
		}
	}
	var h History
	var cur plumbing.Hash // name's blob in c
	if c != nil && name != "" {
		if cur, err = r.blobAt(c, name); err != nil {
			return History{}, err
		}
	}
	for c != nil {
		if err := ctx.Err(); err != nil {
			return History{}, err
		}
		parent, err := r.parent(c)
		if err != nil {
			return History{}, err
		}
		var prev plumbing.Hash
		if parent != nil && name != "" {
			if prev, err = r.blobAt(parent, name); err != nil {
				return History{}, err
			}
		}
		if name == "" || cur != prev {
			if len(h.Commits) == limit {
				h.Next = h.Commits[limit-1].Commit
				break
			}
			entry := commitEntry(c)
			if name != "" {
				entry.Change = "update"
				switch {
				case prev.IsZero():
					entry.Change = "create"
				case cur.IsZero():
					entry.Change = "delete"
				}
				if !cur.IsZero() {
					entry.Version = cur.String()
					e, _, err := r.describe(name, cur)
					if err != nil {
						return History{}, err
					}
					entry.Versioned = &e.Versioned
				}
			}
			h.Commits = append(h.Commits, entry)
		}
		c, cur = parent, prev
	}
	return h, nil
}

func (r *repo) parent(c *object.Commit) (*object.Commit, error) {
	if len(c.ParentHashes) == 0 {
		return nil, nil
	}
	return object.GetCommit(r.st, c.ParentHashes[0])
}

// blobAt returns the hash of the document name in c, or zero if there is none.
func (r *repo) blobAt(c *object.Commit, name string) (plumbing.Hash, error) {
	e, err := r.entryAt(c, name)
	switch {
	case errors.Is(err, ErrNotFound):
		return plumbing.ZeroHash, nil
	case err != nil:
		return plumbing.ZeroHash, err
	case e.Mode == filemode.Dir:
		return plumbing.ZeroHash, nil
	}
	return e.Hash, nil
}

func commitEntry(c *object.Commit) Commit {
	entry := Commit{
		Commit:  c.Hash.String(),
		Author:  Person{Name: c.Author.Name, Email: c.Author.Email},
		Time:    c.Author.When.UTC().Format(timeFormat),
		Message: strings.TrimSpace(c.Message),
	}
	if c.Committer.Name != c.Author.Name || c.Committer.Email != c.Author.Email {
		entry.Agent = c.Committer.Name
	}
	return entry
}
