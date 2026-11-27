package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

const (
	gitDir = ".git"
	branch = plumbing.ReferenceName("refs/heads/main")
)

type repo struct {
	root     *os.Root
	st       *filesystem.Storage
	state    *spaceState
	added    []plumbing.Hash
	collect  bool
	releases []func()
}

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

func (r *repo) revision(ctx context.Context, at string) (*object.Commit, error) {
	head, err := r.head()
	if at == "" || err != nil {
		return head, err
	}
	if !isHash(at) {
		return nil, fmt.Errorf("%w: %q isn't a commit ID", ErrInvalid, at)
	}
	want := plumbing.NewHash(at)
	notFound := fmt.Errorf("%w: commit %s", ErrNotFound, at)
	if head == nil || r.st.HasEncodedObject(want) != nil {
		return nil, notFound
	}
	in, err := r.state.anc.reaches(ctx, r, head, want)
	switch {
	case errors.Is(err, plumbing.ErrObjectNotFound):
		return nil, notFound
	case err != nil:
		return nil, err
	case !in:
		return nil, notFound
	}
	c, err := object.GetCommit(r.st, want)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, notFound
	}
	return c, err
}

type ancestry struct {
	mu    sync.Mutex
	chain []plumbing.Hash
	idx   map[plumbing.Hash]int
}

func (a *ancestry) reaches(ctx context.Context, r *repo, head *object.Commit, want plumbing.Hash) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var fresh []plumbing.Hash
	keep := 0
	for c := head; c != nil; {
		if k, ok := a.idx[c.Hash]; ok {
			keep = k + 1
			break
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		fresh = append(fresh, c.Hash)
		var err error
		if c, err = r.parent(c); err != nil {
			return false, err
		}
	}
	if a.idx == nil {
		a.idx = map[plumbing.Hash]int{}
	}
	for _, h := range a.chain[keep:] {
		delete(a.idx, h)
	}
	a.chain = a.chain[:keep]
	for i := len(fresh) - 1; i >= 0; i-- {
		a.idx[fresh[i]] = len(a.chain)
		a.chain = append(a.chain, fresh[i])
	}
	_, ok := a.idx[want]
	return ok, nil
}

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

type nameKey struct{ nfc, fold string }

func newNameKey(s string) nameKey {
	nfc := norm.NFC.String(s)
	return nameKey{nfc: nfc, fold: foldNFC(nfc)}
}

func foldNFC(nfc string) string { return norm.NFC.String(cases.Fold().String(nfc)) }

func (k nameKey) same(name string) bool {
	nfc := norm.NFC.String(name)
	return strings.EqualFold(k.nfc, nfc) || k.fold == foldNFC(nfc)
}

func findEntry(entries []object.TreeEntry, name string) *object.TreeEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

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
		return Doc{Entry: entry}, err
	}
	content, err := r.blobBytes(e.Hash)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Entry: entry, Content: content}, nil
}

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

type edit struct {
	name    string
	hash    plumbing.Hash
	checked int
}

func (r *repo) moveEdits(cur *object.TreeEntry, from, to string) ([]edit, error) {
	if cur.Mode != filemode.Dir {
		return []edit{{name: from}, {name: to, hash: cur.Hash}}, nil
	}
	var removes, adds []edit
	destDepth := strings.Count(to, "/") + 1
	err := r.walk(cur.Hash, from, func(name string, e object.TreeEntry) error {
		dest := to + strings.TrimPrefix(name, from)
		if err := checkPath(dest); err != nil {
			return err
		}
		removes = append(removes, edit{name: name})
		adds = append(adds, edit{name: dest, hash: e.Hash, checked: destDepth})
		return nil
	})
	return append(removes, adds...), err
}

func (r *repo) change(head *object.Commit, edits []edit, by Author, message string) (_ string, err error) {
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
		elems := strings.Split(e.name, "/")
		checked := e.checked
		if checked == 0 {
			checked = len(elems)
		}
		if root, err = r.setPath(root, elems, e.name, e.hash, checked); err != nil {
			return "", err
		}
	}
	if root.IsZero() {
		var err error
		if root, err = r.storeTree(nil); err != nil {
			return "", err
		}
	}
	if head != nil && root == head.TreeHash {
		return head.Hash.String(), nil
	}
	commit, old, err := r.newCommit(head, root, by, message)
	if err != nil {
		return "", err
	}
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

func (r *repo) result(commit, name string, h plumbing.Hash) (Result, error) {
	e, _, err := r.describe(name, h)
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: commit, Doc: &e}, nil
}

func (r *repo) setPath(treeHash plumbing.Hash, elems []string, name string, blob plumbing.Hash, checked int) (plumbing.Hash, error) {
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
		var key nameKey
		if checked > 0 {
			key = newNameKey(elems[0])
		}
		if j := slices.IndexFunc(entries, func(e object.TreeEntry) bool { return checked > 0 && key.same(e.Name) }); j >= 0 {
			return plumbing.ZeroHash, errorf(apierr.CodeDocCaseConflict, apierr.Params{"name": elems[0], "other": entries[j].Name},
				"%w: %q differs only by case or Unicode form from %q, which exists", ErrConflict, elems[0], entries[j].Name)
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
		h, err := r.setPath(sub, elems[1:], name, blob, checked-1)
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

func (r *repo) setMain(h plumbing.Hash, old *plumbing.Reference) error {
	if err := r.st.CheckAndSetReference(plumbing.NewHashReference(branch, h), old); err != nil {
		return err
	}
	r.countAdded()
	return nil
}

func (r *repo) undo(head *object.Commit, edits []edit) error {
	r.recount()
	err := r.rollback(head, edits)
	if err == nil {
		r.clearPending()
	}
	return err
}

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

func sigText(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

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
		idx.Entries = append(idx.Entries, &index.Entry{Name: e.name, Hash: e.hash, Mode: filemode.Regular, Size: indexSize(size)})
	}
	slices.SortFunc(idx.Entries, func(a, b *index.Entry) int { return strings.Compare(a.Name, b.Name) })
	return r.st.SetIndex(idx)
}

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

func (r *repo) removeFile(name string) error {
	if err := r.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := path.Dir(name)
	for dir != "." && r.root.Remove(dir) == nil {
		dir = path.Dir(dir)
	}
	return nil
}

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
			continue
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
	r.collect = true
	r.recount()
	r.clearPending()
	return nil
}

func (r *repo) recount() {
	r.added = nil
	r.state.size = -1
}

func (r *repo) countAdded() {
	if r.state.size < 0 {
		return
	}
	for _, h := range r.added {
		hex := h.String()
		if fi, err := r.root.Lstat(path.Join(gitDir, "objects", hex[:2], hex[2:])); err == nil {
			r.state.size += fi.Size()
		}
	}
	r.added = nil
}

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

var errPageFull = errors.New("page full")

func (r *repo) list(ctx context.Context, at, prefix, after string, limit int) (Page, error) {
	head, err := r.revision(ctx, at)
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
					continue
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

func (r *repo) history(ctx context.Context, name, before string, limit int) (History, error) {
	c, err := r.head()
	if err != nil || c == nil {
		return History{}, err
	}
	if before != "" {
		if c, err = r.revision(ctx, before); err != nil {
			return History{}, err
		}
		if c, err = r.parent(c); err != nil {
			return History{}, err
		}
	}
	var h History
	var cur plumbing.Hash
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

func indexSize(size int64) uint32 {
	if size < 0 || size > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(size)
}
