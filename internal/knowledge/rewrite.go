package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

var rewriteFile = path.Join(gitDir, "taskto-rewrite")

var rewriteFault func(step string) error

func fault(step string) error {
	if rewriteFault == nil {
		return nil
	}
	return rewriteFault(step)
}

type rewritePlan struct {
	after   string
	paths   []string
	authors []string
}

const planHeader = "taskto-rewrite 1\n"

func (p rewritePlan) encode() []byte {
	var b strings.Builder
	b.WriteString(planHeader)
	after := p.after
	if after == "" {
		after = "-"
	}
	b.WriteString("after " + after + "\n")
	for _, name := range p.paths {
		b.WriteString("path " + name + "\n")
	}
	for _, email := range p.authors {
		b.WriteString("author " + email + "\n")
	}
	return []byte(b.String())
}

func planEmail(value string) bool {
	return value != "" && len(value) <= 320 && !strings.ContainsAny(value, " \t\r\n\x00") && value == strings.ToLower(value)
}

func parsePlan(b []byte) (rewritePlan, error) {
	rest, ok := bytes.CutPrefix(b, []byte(planHeader))
	if !ok {
		return rewritePlan{}, errors.New("knowledge: the history rewrite's journal is unreadable")
	}
	var p rewritePlan
	for _, line := range strings.Split(strings.TrimSuffix(string(rest), "\n"), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch {
		case key == "after" && value == "-":
		case key == "after" && isHash(value):
			p.after = value
		case key == "path" && checkPath(value) == nil:
			p.paths = append(p.paths, value)
		case key == "author" && planEmail(value):
			p.authors = append(p.authors, value)
		default:
			return rewritePlan{}, fmt.Errorf("knowledge: the history rewrite's journal has %q", line)
		}
	}
	return p, nil
}

func (r *repo) writeAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.root.Rename(tmp, name)
	}
	if err != nil {
		_ = r.root.Remove(tmp)
		return err
	}
	r.syncDir(path.Dir(name))
	return nil
}

func (r *repo) syncDir(dir string) {
	if d, err := r.root.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func (r *repo) planRewrite(p rewritePlan) error {
	return r.writeAtomic(rewriteFile, p.encode())
}

func (r *repo) dropRewrite() { _ = r.root.Remove(rewriteFile) }

func (r *repo) afterChange(sp access.Space, commit string, err error) string {
	if err != nil {
		r.dropRewrite()
		return commit
	}
	head, rerr := r.resumeRewrite(sp)
	if rerr != nil {
		slog.Warn("knowledge: taking a file's old versions out of history; it is retried at the next change", "space", sp.ID, "err", rerr)
		return commit
	}
	if !head.IsZero() {
		return head.String()
	}
	return commit
}

func (r *repo) resumeRewrite(sp access.Space) (_ plumbing.Hash, err error) {
	defer func() {
		if err != nil {
			r.recount()
		}
	}()
	raw, err := r.root.ReadFile(rewriteFile)
	if errors.Is(err, fs.ErrNotExist) {
		return plumbing.ZeroHash, nil
	} else if err != nil {
		return plumbing.ZeroHash, err
	}
	p, err := parsePlan(raw)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	head, err := r.head()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if head == nil || (p.after != "" && head.Hash.String() == p.after) {
		r.dropRewrite()
		return plumbing.ZeroHash, nil
	}
	start := time.Now()
	stats, err := r.rewriteHistory(head, p.paths, p.authors)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if err := fault("ref"); err != nil {
		return plumbing.ZeroHash, err
	}
	pruned, err := r.prune()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	r.dropRewrite()
	slog.Info("knowledge: took old versions out of history", "space", sp.ID, "paths", p.paths,
		"commits", stats.commits, "rewritten", stats.rewritten, "versions", stats.versions,
		"objects_deleted", pruned, "took", time.Since(start).Round(time.Millisecond))
	return stats.head, nil
}

type rewriteStats struct {
	head                         plumbing.Hash
	commits, rewritten, versions int
}

func (r *repo) rewriteHistory(head *object.Commit, paths, authors []string) (rewriteStats, error) {
	rw := &rewriter{
		r:        r,
		keep:     map[string]plumbing.Hash{},
		pointers: map[plumbing.Hash]plumbing.Hash{},
		trees:    map[treeKey]plumbing.Hash{},
		commits:  map[plumbing.Hash]plumbing.Hash{},
	}
	for _, name := range paths {
		h, err := r.blobAt(head, name)
		if err != nil {
			return rewriteStats{}, err
		}
		rw.keep[name] = h
	}
	rw.paths = paths
	if len(authors) > 0 {
		rw.authors = map[string]bool{}
		for _, email := range authors {
			rw.authors[email] = true
		}
	}
	newHead, err := rw.commit(head.Hash)
	if err != nil {
		return rewriteStats{}, err
	}
	stats := rewriteStats{head: newHead, commits: len(rw.commits), versions: len(rw.pointers)}
	for old, h := range rw.commits {
		if old != h {
			stats.rewritten++
		}
	}
	if err := fault("objects"); err != nil {
		return rewriteStats{}, err
	}
	if newHead != head.Hash {
		if err := r.moveMain(newHead, head.Hash); err != nil {
			return rewriteStats{}, err
		}
	}
	return stats, nil
}

func (r *repo) moveMain(h, old plumbing.Hash) error {
	cur, err := r.st.Reference(branch)
	if err != nil {
		return err
	}
	if cur.Hash() != old {
		return fmt.Errorf("knowledge: main moved during a history rewrite: %s, not %s", cur.Hash(), old)
	}
	ref := path.Join(gitDir, branch.String())
	if err := r.root.MkdirAll(path.Dir(ref), 0o700); err != nil {
		return err
	}
	if err := r.writeAtomic(ref, []byte(h.String()+"\n")); err != nil {
		return err
	}
	got, err := r.st.Reference(branch)
	if err != nil {
		return fmt.Errorf("knowledge: reading main back after a history rewrite: %w", err)
	}
	if got.Hash() != h {
		return fmt.Errorf("knowledge: main reads back as %s, not %s, after a history rewrite", got.Hash(), h)
	}
	return nil
}

type treeKey struct {
	tree  plumbing.Hash
	name  string
	depth int
}

type rewriter struct {
	r        *repo
	paths    []string
	authors  map[string]bool
	keep     map[string]plumbing.Hash
	pointers map[plumbing.Hash]plumbing.Hash
	trees    map[treeKey]plumbing.Hash
	commits  map[plumbing.Hash]plumbing.Hash
}

func (rw *rewriter) commit(head plumbing.Hash) (plumbing.Hash, error) {
	read := map[plumbing.Hash]*object.Commit{}
	stack := []plumbing.Hash{head}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		if _, ok := rw.commits[h]; ok {
			stack = stack[:len(stack)-1]
			continue
		}
		c, ok := read[h]
		if !ok {
			var err error
			if c, err = object.GetCommit(rw.r.st, h); err != nil {
				return plumbing.ZeroHash, err
			}
			read[h] = c
		}
		waiting := false
		for _, p := range c.ParentHashes {
			if _, ok := rw.commits[p]; !ok {
				stack = append(stack, p)
				waiting = true
			}
		}
		if waiting {
			continue
		}
		stack = stack[:len(stack)-1]
		nh, err := rw.rewriteCommit(c)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		rw.commits[h] = nh
		delete(read, h)
	}
	return rw.commits[head], nil
}

func (rw *rewriter) rewriteCommit(c *object.Commit) (plumbing.Hash, error) {
	tree := c.TreeHash
	for _, name := range rw.paths {
		var err error
		if tree, err = rw.tree(tree, name, strings.Split(name, "/")); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	parents := make([]plumbing.Hash, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		parents[i] = rw.commits[p]
	}
	author, committer := rw.signature(c.Author), rw.signature(c.Committer)
	if tree == c.TreeHash && slices.Equal(parents, c.ParentHashes) && author == c.Author && committer == c.Committer {
		return c.Hash, nil
	}
	nc := &object.Commit{
		Author: author, Committer: committer, MergeTag: c.MergeTag, Message: c.Message,
		TreeHash: tree, ParentHashes: parents, Encoding: c.Encoding, ExtraHeaders: c.ExtraHeaders,
	}
	obj := rw.r.st.NewEncodedObject()
	if err := nc.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return rw.r.store(obj)
}

func (rw *rewriter) signature(sig object.Signature) object.Signature {
	if rw.authors[strings.ToLower(sig.Email)] {
		return object.Signature{Name: DeletedAuthorName, Email: DeletedAuthorEmail, When: sig.When}
	}
	return sig
}

func (rw *rewriter) tree(h plumbing.Hash, name string, elems []string) (plumbing.Hash, error) {
	key := treeKey{tree: h, name: name, depth: len(elems)}
	if nh, ok := rw.trees[key]; ok {
		return nh, nil
	}
	nh, err := rw.rewriteTree(h, name, elems)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	rw.trees[key] = nh
	return nh, nil
}

func (rw *rewriter) rewriteTree(h plumbing.Hash, name string, elems []string) (plumbing.Hash, error) {
	tree, err := object.GetTree(rw.r.st, h)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	i := slices.IndexFunc(tree.Entries, func(e object.TreeEntry) bool { return e.Name == elems[0] })
	if i < 0 {
		return h, nil
	}
	e := tree.Entries[i]
	var nh plumbing.Hash
	switch {
	case len(elems) == 1 && e.Mode.IsFile():
		nh, err = rw.blob(name, e.Hash)
	case len(elems) > 1 && e.Mode == filemode.Dir:
		nh, err = rw.tree(e.Hash, name, elems[1:])
	default:
		return h, nil
	}
	if err != nil || nh == e.Hash {
		return h, err
	}
	entries := slices.Clone(tree.Entries)
	entries[i].Hash = nh
	obj := rw.r.st.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return rw.r.store(obj)
}

func (rw *rewriter) blob(name string, h plumbing.Hash) (plumbing.Hash, error) {
	if h == rw.keep[name] {
		return h, nil
	}
	if ph, ok := rw.pointers[h]; ok {
		return ph, nil
	}
	p, isPtr, err := rw.r.pointerFor(h)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if isPtr {
		return h, nil
	}
	ph, err := rw.r.storeBlob(p.encode())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	rw.pointers[h] = ph
	return ph, nil
}

func (r *repo) pointerFor(h plumbing.Hash) (_ pointer, isPtr bool, _ error) {
	blob, err := object.GetBlob(r.st, h)
	if err != nil {
		return pointer{}, false, err
	}
	if blob.Size <= maxPointerSize {
		content, err := r.blobBytes(h)
		if err != nil {
			return pointer{}, false, err
		}
		if p, ok := parsePointer(content); ok {
			return p, true, nil
		}
	}
	rd, err := blob.Reader()
	if err != nil {
		return pointer{}, false, err
	}
	defer rd.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, rd)
	if err != nil {
		return pointer{}, false, err
	}
	return pointer{oid: hex.EncodeToString(sum.Sum(nil)), size: n}, false, nil
}

func (r *repo) prune() (int, error) {
	reach, err := r.reachable()
	if err != nil {
		return 0, err
	}
	packs, err := r.st.ObjectPacks()
	if err != nil {
		return 0, err
	}
	if len(packs) > 0 {
		repo, err := git.Open(r.st, nil)
		if err != nil {
			return 0, err
		}
		if err := repo.RepackObjects(&git.RepackConfig{}); err != nil {
			return 0, fmt.Errorf("knowledge: repacking: %w", err)
		}
	}
	var gone []plumbing.Hash
	if err := r.st.ForEachObjectHash(func(h plumbing.Hash) error {
		if !reach[h] {
			gone = append(gone, h)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	for _, h := range gone {
		if err := r.st.DeleteLooseObject(h); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
	}
	for _, cache := range gitCaches {
		if err := r.root.RemoveAll(path.Join(gitDir, cache)); err != nil {
			return 0, err
		}
	}
	r.recount()
	return len(gone), nil
}

var gitCaches = []string{
	"objects/info/commit-graph", "objects/info/commit-graphs", "objects/info/packs",
	"objects/pack/multi-pack-index",
}

func (r *repo) reachable() (map[plumbing.Hash]bool, error) {
	seen := map[plumbing.Hash]bool{}
	refs, err := r.st.IterReferences()
	if err != nil {
		return nil, err
	}
	var todo []plumbing.Hash
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		if ref.Name() != branch {
			slog.Warn("knowledge: a ref other than main, made outside the server, keeps what it reaches of the old history", "ref", ref.Name().String())
		}
		todo = append(todo, ref.Hash())
		return nil
	})
	if err != nil {
		return nil, err
	}
	for len(todo) > 0 {
		h := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[h] {
			continue
		}
		obj, err := r.st.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, err
		}
		switch obj.Type() {
		case plumbing.CommitObject:
			seen[h] = true
			c, err := object.DecodeCommit(r.st, obj)
			if err != nil {
				return nil, err
			}
			if err := r.reachTree(c.TreeHash, seen); err != nil {
				return nil, err
			}
			todo = append(todo, c.ParentHashes...)
		case plumbing.TagObject:
			seen[h] = true
			tag, err := object.DecodeTag(r.st, obj)
			if err != nil {
				return nil, err
			}
			todo = append(todo, tag.Target)
		case plumbing.TreeObject:
			if err := r.reachTree(h, seen); err != nil {
				return nil, err
			}
		default:
			seen[h] = true
		}
	}
	return seen, nil
}

func (r *repo) reachTree(h plumbing.Hash, seen map[plumbing.Hash]bool) error {
	if seen[h] {
		return nil
	}
	seen[h] = true
	tree, err := object.GetTree(r.st, h)
	if err != nil {
		return err
	}
	for _, e := range tree.Entries {
		switch {
		case e.Mode == filemode.Dir:
			if err := r.reachTree(e.Hash, seen); err != nil {
				return err
			}
		case e.Mode != filemode.Submodule:
			seen[e.Hash] = true
		}
	}
	return nil
}

func (r *repo) toLFS(h plumbing.Hash, limit int64) (pointer, error) {
	p, isPtr, err := r.pointerFor(h)
	if err != nil || isPtr {
		return p, err
	}
	if r.hasObject(p.oid) {
		return p, nil
	}
	used, err := r.unversionedSize()
	if err != nil {
		return pointer{}, err
	}
	if used+p.size > limit {
		return pointer{}, fmt.Errorf("%w: its unversioned files would be over %d bytes", ErrRepoFull, limit)
	}
	if err := r.root.MkdirAll(tmpDir, 0o700); err != nil {
		return pointer{}, err
	}
	tmp := path.Join(tmpDir, "move-"+randomSuffix())
	if err := r.copyBlob(h, tmp); err != nil {
		_ = r.root.Remove(tmp)
		return pointer{}, err
	}
	dst := objectPath(p.oid)
	if err := r.root.MkdirAll(path.Dir(dst), 0o700); err != nil {
		_ = r.root.Remove(tmp)
		return pointer{}, err
	}
	r.state.unv = -1
	if err := r.root.Rename(tmp, dst); err != nil {
		_ = r.root.Remove(tmp)
		return pointer{}, err
	}
	return p, nil
}

func (r *repo) copyBlob(h plumbing.Hash, name string) error {
	blob, err := object.GetBlob(r.st, h)
	if err != nil {
		return err
	}
	rd, err := blob.Reader()
	if err != nil {
		return err
	}
	defer rd.Close()
	f, err := r.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rd)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *Store) Recover(ctx context.Context) error {
	workspaces, err := os.ReadDir(filepath.Join(s.dataDir, "workspaces"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, w := range workspaces {
		wid, err := strconv.ParseInt(w.Name(), 10, 64)
		if err != nil || !w.IsDir() {
			continue
		}
		spaces, err := os.ReadDir(filepath.Join(s.dataDir, "workspaces", w.Name(), "spaces"))
		if err != nil {
			continue
		}
		for _, d := range spaces {
			sid, err := strconv.ParseInt(d.Name(), 10, 64)
			if err != nil || !d.IsDir() {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			sp := access.Space{ID: sid, WorkspaceID: wid, Access: access.Write}
			if _, err := os.Lstat(filepath.Join(sp.Dir(s.dataDir), filepath.FromSlash(rewriteFile))); err != nil {
				continue
			}
			err = s.write(sp, 0, false, func(*repo) error { return nil })
			if err != nil && !errors.Is(err, errNoRepo) && !errors.Is(err, ErrRepoFull) {
				slog.Error("knowledge: finishing a history rewrite", "space", sid, "err", err)
			}
		}
	}
	return nil
}

type Removed struct {
	Commit  string `json:"commit"`
	Doc     *Entry `json:"doc,omitempty"`
	Removed int    `json:"removed"`
}

func (s *Store) RemoveOldVersions(ctx context.Context, sp access.Space, name, base string, by Author) (Removed, error) {
	if err := s.checkWrite(sp, name, nil, by, ""); err != nil {
		return Removed{}, err
	}
	if err := checkVersion(base); err != nil {
		return Removed{}, err
	}
	var res Removed
	err := s.write(sp, -1, false, func(r *repo) error {
		head, err := r.head()
		if err != nil {
			return err
		}
		cur, err := r.entryAt(head, name)
		switch {
		case errors.Is(err, ErrNotFound) && base != "":
			return errorf(apierr.CodeDocDeleted, pathParams(name), "%w: %s was deleted since version %s", ErrConflict, name, base)
		case errors.Is(err, ErrNotFound):
			cur = nil
		case err != nil:
			return err
		case cur.Mode == filemode.Dir:
			return fmt.Errorf("%w: %s is a folder", ErrInvalid, name)
		default:
			if err := checkBase(cur, name, base); err != nil {
				return err
			}
		}
		var keep plumbing.Hash
		if cur != nil {
			keep = cur.Hash
			e, _, err := r.describe(name, cur.Hash)
			if err != nil {
				return err
			}
			res.Doc = &e
		}
		existed, old, err := r.oldVersions(ctx, name, keep)
		if err != nil {
			return err
		}
		if !existed {
			return fmt.Errorf("%w: %s has no history", ErrNotFound, name)
		}
		res.Commit, res.Removed = head.Hash.String(), old
		if old == 0 {
			return nil
		}
		if err := r.planRewrite(rewritePlan{after: head.Hash.String(), paths: []string{name}}); err != nil {
			return err
		}
		commit, err := r.record(head, by, "Remove the old versions of "+name+" from history")
		res.Commit = r.afterChange(sp, commit, err)
		return err
	})
	return res, err
}

func (r *repo) oldVersions(ctx context.Context, name string, keep plumbing.Hash) (bool, int, error) {
	h, err := r.history(ctx, name, "", math.MaxInt)
	if err != nil {
		return false, 0, err
	}
	seen := map[string]bool{}
	n := 0
	for _, c := range h.Commits {
		if c.Version == "" || c.Version == keep.String() || seen[c.Version] {
			continue
		}
		seen[c.Version] = true
		if c.Versioned != nil && *c.Versioned {
			n++
		}
	}
	return len(h.Commits) > 0, n, nil
}

func (r *repo) record(head *object.Commit, by Author, message string) (string, error) {
	commit, old, err := r.newCommit(head, head.TreeHash, by, message)
	if err != nil {
		return "", err
	}
	if err := r.setMain(commit, old); err != nil {
		r.recount()
		return "", err
	}
	return commit.String(), nil
}

const (
	DeletedAuthorName  = "Deleted user"
	DeletedAuthorEmail = "deleted-user@deleted.invalid"
)

func (s *Store) AnonymizeAuthors(ctx context.Context, sp access.Space, emails []string) (int, error) {
	return s.AnonymizeAuthorsNoting(ctx, sp, emails, nil)
}

func (s *Store) AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(n int) error) (int, error) {
	set := map[string]bool{}
	var plan []string
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if !planEmail(e) || set[e] {
			continue
		}
		set[e] = true
		plan = append(plan, e)
	}
	if len(plan) == 0 {
		return 0, nil
	}
	slices.Sort(plan)
	sp.Access = access.Write
	var found bool
	err := s.read(sp, func(r *repo) error {
		head, err := r.head()
		if err != nil || head == nil {
			return err
		}
		pending, err := r.rewritePending()
		if err != nil {
			return err
		}
		n, err := r.countAuthored(ctx, head, set)
		found = n > 0 || pending
		return err
	})
	if errors.Is(err, errNoRepo) {
		return 0, nil
	}
	if err != nil || !found {
		return 0, err
	}
	var rewritten int
	err = s.write(sp, -1, false, func(r *repo) error {
		if _, err := r.resumeRewrite(sp); err != nil {
			return fmt.Errorf("finishing a history rewrite that was left half done: %w", err)
		}
		if pending, err := r.rewritePending(); err != nil {
			return err
		} else if pending {
			return errors.New("a history rewrite is still waiting in the journal")
		}
		head, err := r.head()
		if err != nil || head == nil {
			return err
		}
		n, err := r.countAuthored(ctx, head, set)
		if err != nil || n == 0 {
			return err
		}
		if planned != nil {
			if err := planned(n); err != nil {
				return err
			}
		}
		rewritten = n
		if err := r.planRewrite(rewritePlan{authors: plan}); err != nil {
			return err
		}
		_, err = r.resumeRewrite(sp)
		return err
	})
	if errors.Is(err, errNoRepo) {
		return 0, nil
	}
	return rewritten, err
}

func (r *repo) rewritePending() (bool, error) {
	if _, err := r.root.Stat(rewriteFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *repo) countAuthored(ctx context.Context, head *object.Commit, set map[string]bool) (int, error) {
	n := 0
	seen := map[plumbing.Hash]bool{head.Hash: true}
	stack := []*object.Commit{head}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if set[strings.ToLower(c.Author.Email)] || set[strings.ToLower(c.Committer.Email)] {
			n++
		}
		for _, p := range c.ParentHashes {
			if seen[p] {
				continue
			}
			seen[p] = true
			pc, err := object.GetCommit(r.st, p)
			if err != nil {
				return 0, err
			}
			stack = append(stack, pc)
		}
	}
	return n, nil
}

func (s *Store) RemoveSpace(sp access.Space) error {
	st := s.lockSpace(sp, true, true)
	defer st.mu.Unlock()
	if sp.Generation != 0 {
		if st.lastGen.Load() > sp.Generation {
			return nil
		}
		if s.spaceExists != nil {
			ok, err := s.spaceExists(sp)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
	}
	st.serve(sp.Generation)
	st.removed, st.removedGen = true, sp.Generation
	st.size, st.unv = -1, -1
	st.anc.reset()
	st.publish(nil)
	dir := sp.Dir(s.dataDir)
	leftover := dir + removingSuffix
	if err := os.RemoveAll(leftover); err != nil {
		return fmt.Errorf("knowledge: removing space %d: %w", sp.ID, err)
	}
	if err := os.Rename(dir, leftover); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("knowledge: removing space %d: %w", sp.ID, err)
	}
	if err := os.RemoveAll(leftover); err != nil {
		return fmt.Errorf("knowledge: removing space %d: %w", sp.ID, err)
	}
	return nil
}

const removingSuffix = ".removing"

func (s *Store) HoldSpace(sp access.Space) (release func(), err error) {
	st := s.lockSpace(sp, false, false)
	if err := s.admit(st, sp); err != nil {
		st.mu.RUnlock()
		return nil, err
	}
	return st.mu.RUnlock, nil
}

func (s *Store) ForgetSpace(sp access.Space) {
	st := s.lockSpace(sp, true, false)
	defer st.mu.Unlock()
	if !st.removed || (st.removedGen != 0 && sp.Generation != 0 && sp.Generation != st.removedGen) {
		return
	}
	s.mu.Lock()
	delete(s.spaces, sp.ID)
	s.mu.Unlock()
}
