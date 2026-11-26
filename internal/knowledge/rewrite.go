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

// History rewrites: taking a file's older versions out of git.
//
// When a versioned file becomes unversioned (its new content is over the size limit, or
// a rename gives it a kind that is always unversioned), its older versions leave git
// too: every commit of main is rewritten so that each earlier blob at the file's path is
// a Git LFS pointer to those bytes, as `git lfs migrate import` does, and the bytes
// themselves are dropped. Reading or restoring those versions is then ErrNotKept, like
// any unversioned file's older version.
//
//   - Each commit keeps its author, committer, dates and message; only its tree (the
//     file's blob, and the trees above it) and its parents change, so its ID does too.
//     Commits from before the file first appeared keep their IDs. Blob IDs, which are
//     documents' versions, change for that file's older versions only: every other
//     document's version, and the file's current one, stay as they were, so a
//     base_version held by an editor still matches, or is a conflict, as before.
//   - It runs under the space's lock, as one change: nobody reads or writes the space
//     halfway through.
//   - Crash safety: a journal (rewriteFile) is written, atomically, before anything
//     else, naming the paths; the new objects are only added; main moves last, by an
//     atomic rename; then the objects nothing reaches are deleted and the journal goes.
//     The journal is found at the next start (Recover) or write, and the rewrite is done
//     again from the start: it is idempotent, so a crash at any point ends with the
//     history rewritten. A journal written for a change that never landed (main is still
//     at its "after" commit) is simply dropped.
//   - Cost: it reads every commit of main, and writes a commit and the trees down to the
//     file for each commit from the file's first version on; deleting what is left reads
//     every object main reaches. Both grow with the history, not with its files' size.

// rewriteFile is the journal of a history rewrite in progress.
var rewriteFile = path.Join(gitDir, "taskto-rewrite")

// rewriteFault, if set, is called at each step of a rewrite, and stops it there when it
// returns an error: tests use it to stop a rewrite as a crash would.
var rewriteFault func(step string) error

func fault(step string) error {
	if rewriteFault == nil {
		return nil
	}
	return rewriteFault(step)
}

// rewritePlan is what a rewrite does: the paths whose older versions leave git, and the
// addresses whose commits lose their author (AnonymizeAuthors).
type rewritePlan struct {
	// after, if set, is main's commit before the change that calls for the rewrite: it is
	// done once main has moved past it, so a change that never landed leaves the history
	// as it was.
	after string
	paths []string
	// authors are addresses, lower case, whose author and committer lines are replaced.
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

// planEmail reports whether value can be one of a journal's addresses: one line, no spaces.
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

// writeAtomic replaces the file name with data at once, synced to disk first, so a crash
// leaves the old content or the new, never part of it.
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

// syncDir syncs the folder dir, so a rename in it survives a crash. A failure is left
// for the next rename: some file systems can't sync a folder.
func (r *repo) syncDir(dir string) {
	if d, err := r.root.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// planRewrite records p before the change it follows: from here on, a crash ends with the
// rewrite done (or dropped, if main never moves past p.after).
func (r *repo) planRewrite(p rewritePlan) error {
	return r.writeAtomic(rewriteFile, p.encode())
}

func (r *repo) dropRewrite() { _ = r.root.Remove(rewriteFile) }

// afterChange completes the rewrite planned for a change that returned commit and err,
// and returns main's commit after it. The change has happened (or not) whatever the
// rewrite does: if it fails, the journal stays for the next write or start, and commit
// is returned.
func (r *repo) afterChange(sp access.Space, commit string, err error) string {
	if err != nil {
		r.dropRewrite() // main didn't move
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

// resumeRewrite does the rewrite the journal describes, if there is one, deletes what it
// leaves behind and the journal, and returns main's commit after it (zero without a
// journal, or for one that is dropped).
func (r *repo) resumeRewrite(sp access.Space) (_ plumbing.Hash, err error) {
	// A rewrite stopped half-way has stored objects that weren't counted.
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
		r.dropRewrite() // the change that called for it never landed
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

// rewriteStats is what a rewrite did: main's new commit, how many commits it read and
// rewrote, and how many of the files' versions it took out.
type rewriteStats struct {
	head                         plumbing.Hash
	commits, rewritten, versions int
}

// rewriteHistory rewrites every commit main reaches from head so that each blob at paths
// is a pointer to its bytes, but for one that already is, and the blob each path has in
// head (its current version stays as it is). It then moves main to the new head.
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

// moveMain points main at h, if it is still at old, by renaming a new ref file over it: a
// crash leaves it at old or at h.
func (r *repo) moveMain(h, old plumbing.Hash) error {
	cur, err := r.st.Reference(branch)
	if err != nil {
		return err
	}
	if cur.Hash() != old {
		return fmt.Errorf("knowledge: main moved during a history rewrite: %s, not %s", cur.Hash(), old)
	}
	// A loose ref, which git and go-git read before packed-refs, where git gc may have put
	// main too.
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
	r     *repo
	paths []string
	// authors are the addresses (lower case) whose signatures are replaced, if any.
	authors map[string]bool
	// keep is each path's blob in the head: its current version, which stays.
	keep map[string]plumbing.Hash
	// pointers, trees and commits map what was read to what replaces it.
	pointers map[plumbing.Hash]plumbing.Hash
	trees    map[treeKey]plumbing.Hash
	commits  map[plumbing.Hash]plumbing.Hash
}

// commit returns the rewritten commit of head, parents first, without recursion: a
// history can be long.
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

// rewriteCommit stores c with its tree rewritten and its parents replaced by theirs, and
// returns its hash: c's own if nothing changed.
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
	// Everything but the tree, the parents and the signatures of the addresses named is kept:
	// authors, committers, their dates and time zones, the message. A signature would no
	// longer match, so it goes.
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

// signature is sig, or the placeholder for a deleted account if it names one of rw.authors.
func (rw *rewriter) signature(sig object.Signature) object.Signature {
	if rw.authors[strings.ToLower(sig.Email)] {
		return object.Signature{Name: DeletedAuthorName, Email: DeletedAuthorEmail, When: sig.When}
	}
	return sig
}

// tree returns the tree h with the file at elems (the end of name) rewritten, storing it
// if it changed.
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
		return h, nil // a folder where the file would be, or a file where a folder would
	}
	if err != nil || nh == e.Hash {
		return h, err
	}
	// The entries stay in their order: only one hash changes.
	entries := slices.Clone(tree.Entries)
	entries[i].Hash = nh
	obj := rw.r.st.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return rw.r.store(obj)
}

// blob returns the pointer that replaces the blob h at name: h itself if it is the
// current version, or a pointer already.
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

// pointerFor returns the pointer to the blob h's bytes, reading them through once; isPtr
// is set if h already is a pointer.
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

// prune deletes the objects no ref reaches: after a rewrite, the old commits, their trees
// and the files' old versions. With packs (made by git gc outside the server),
// they are repacked first, so the packs hold only what main reaches. It returns how many
// loose objects it deleted.
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
	// Indexes git keeps of its objects and commits name the old ones: git rebuilds them.
	for _, cache := range gitCaches {
		if err := r.root.RemoveAll(path.Join(gitDir, cache)); err != nil {
			return 0, err
		}
	}
	r.recount()
	return len(gone), nil
}

// gitCaches are the files in .git that index objects, which git gc can make: none is
// needed, and each would name objects a rewrite deleted.
var gitCaches = []string{
	"objects/info/commit-graph", "objects/info/commit-graphs", "objects/info/packs",
	"objects/pack/multi-pack-index",
}

// reachable returns every object a ref reaches: commits, tags, trees and blobs. main is
// the only ref the server makes; any other (made with git outside it, against the rules)
// is followed too, so that nothing it needs is deleted, and logged, since it keeps
// whatever it reaches of the old history.
func (r *repo) reachable() (map[plumbing.Hash]bool, error) {
	seen := map[plumbing.Hash]bool{}
	refs, err := r.st.IterReferences()
	if err != nil {
		return nil, err
	}
	var todo []plumbing.Hash
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil // HEAD, which names main
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

// toLFS moves the bytes of the blob h to the space's unversioned objects, unless it has
// them, and returns their pointer: for a versioned file that a rename makes unversioned.
// limit is the space's unversioned bytes it may take up.
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

// Recover finishes what a process that stopped left half done in every space under the
// data directory: a history rewrite, and a change (as the next write would). Run it at
// start, before the Store serves anyone, so nobody reads a history a rewrite was taking
// versions out of. A space it can't recover is logged, and tried again at its next write.
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
			// A write that changes nothing: it repairs, and finishes the rewrite, first. The
			// index hears of the new commits like any change's.
			err = s.write(sp, 0, false, func(*repo) error { return nil })
			if err != nil && !errors.Is(err, errNoRepo) && !errors.Is(err, ErrRepoFull) {
				slog.Error("knowledge: finishing a history rewrite", "space", sid, "err", err)
			}
		}
	}
	return nil
}

// Removed is what RemoveOldVersions did: main's commit after it, the file as it is now
// (none once deleted), and how many of its older versions it took out of history.
type Removed struct {
	Commit  string `json:"commit"`
	Doc     *Entry `json:"doc,omitempty"`
	Removed int    `json:"removed"`
}

// RemoveOldVersions takes the older versions of the file name out of sp's history, as a
// file that becomes unversioned has them taken out: each becomes a pointer whose bytes
// are gone, and its current version stays as it is. A deleted file loses every version.
// base, if not empty, must be its current version. A commit by by, which changes no file,
// records it in the space's history; with no version to take out, nothing happens. It is
// for workspace admins only, which the HTTP route checks (access.Checker.RequireSpaceAdmin).
func (s *Store) RemoveOldVersions(ctx context.Context, sp access.Space, name, base string, by Author) (Removed, error) {
	if err := s.checkWrite(sp, name, nil, by, ""); err != nil {
		return Removed{}, err
	}
	if err := checkVersion(base); err != nil {
		return Removed{}, err
	}
	var res Removed
	// It adds a commit, but frees what it takes out: a full repository takes it too.
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

// oldVersions reports whether name has a history on main, and how many of its versions
// in it git still keeps, but keep (its current one).
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

// record commits head's tree again, by by: a change to the history that changes no file.
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

// DeletedAuthorName and DeletedAuthorEmail are what a deleted account's name and address in a
// space's history become (AnonymizeAuthors): the web app shows the address as "Deleted user",
// translated.
const (
	DeletedAuthorName  = "Deleted user"
	DeletedAuthorEmail = "deleted-user@deleted.invalid"
)

// AnonymizeAuthors replaces the name and address of the authors and committers of sp's history
// whose address is one of emails with the placeholder of a deleted account, for the purge of an
// account (internal/accountdeletion): what a person wrote in a shared space stays, and no longer
// says who they were. It returns how many commits it rewrote (zero when none names them, which
// leaves the history as it is). Every commit keeps its tree, dates and message, so documents'
// versions (blob IDs) don't change, but commit IDs do from the first one rewritten on, as in any
// history rewrite above, with the same crash safety: the journal names the addresses, the
// rewrite is done again from the start by the next write or start, and what it leaves unreachable
// (the old commits, which hold the address) is deleted before it ends. It is idempotent.
func (s *Store) AnonymizeAuthors(ctx context.Context, sp access.Space, emails []string) (int, error) {
	return s.AnonymizeAuthorsNoting(ctx, sp, emails, nil)
}

// AnonymizeAuthorsNoting is AnonymizeAuthors, and calls planned (if set) with the number of commits
// to rewrite, under the space's write lock, before the rewrite is journaled: once it returns nil,
// the rewrite of those commits is done by this call or, if it stops, by whatever finishes the
// journal (and a later call then finds nothing and doesn't call it). An error from planned stops
// the call before anything changes. It isn't called when nothing names them. A caller that keeps
// what planned says, replacing what an earlier call's said for the space, counts each commit once
// whatever stops in between (internal/accountdeletion).
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
	// A look first, under the read lock and without telling the search index of a change: most
	// spaces of a server have nothing of the person's. A rewrite a crash left half done (its journal
	// names addresses, and the commits it was to replace may still be in the object store) isn't
	// nothing, though main may already be rewritten: it is finished before the space is called clean.
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
	// It frees what it replaces: a full repository takes it too.
	err = s.write(sp, -1, false, func(r *repo) error {
		// Any rewrite in the journal first, whose it is: a write does this as it starts but goes on
		// when it can't, and what is reported done here must be done.
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

// rewritePending reports whether a history rewrite's journal is there.
func (r *repo) rewritePending() (bool, error) {
	if _, err := r.root.Stat(rewriteFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// countAuthored counts the commits main reaches from head whose author or committer has one of
// the addresses in set (lower case).
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

// RemoveSpace deletes everything sp keeps on disk: its git repository, its unversioned files
// and the large files' bytes, and whatever the apps stored in its directory (photos), for a
// private space whose owner's account is purged (internal/accountdeletion). It takes the space's
// lock, so no write is half-way through, and a call that comes after it, or was waiting for the
// lock, is refused with ErrSpaceRemoved: the space is marked removed first, so a removal that
// fails part-way leaves it refused, not half there for a write to commit into, and nothing makes
// its directory again. The directory is renamed away in one step before it is deleted, so a
// crash in the middle leaves no half-deleted repository at the space's path (a write then makes
// a new, whole one, which the retry removes); the leftover is removed by the retry. The space's
// rows are deleted by the caller, after: the directory goes first, so a purge that stops between
// the two still finds the space to finish, and the caller then calls ForgetSpace. Removing what
// is gone already succeeds.
func (s *Store) RemoveSpace(sp access.Space) error {
	st := s.lockSpace(sp, true, true)
	defer st.mu.Unlock()
	st.removed, st.removedCreated = true, sp.CreatedAt
	dir := sp.Dir(s.dataDir)
	leftover := dir + removingSuffix
	// What an earlier removal renamed and didn't finish deleting, first: the rename below
	// needs the name free.
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

// removingSuffix names, next to a space's directory, what RemoveSpace has renamed and not yet
// deleted. It isn't a number, so nothing that lists the spaces' directories takes it for one.
const removingSuffix = ".removing"

// HoldSpace holds sp's directory for a caller that writes in it besides the knowledge base (the
// apps' attachments): until release is called, RemoveSpace waits for it, so such a write is
// finished before the removal begins; once the space is removed, it is ErrSpaceRemoved, and the
// caller makes nothing in the directory. It holds the same lock as the Store's reads, shared with
// them and with other holders, so a caller keeps it for the writes only.
func (s *Store) HoldSpace(sp access.Space) (release func(), err error) {
	st := s.lockSpace(sp, false, false)
	if err := s.admit(st, sp); err != nil {
		st.mu.RUnlock()
		return nil, err
	}
	return st.mu.RUnlock, nil
}

// ForgetSpace drops what the Store keeps of a space removed by RemoveSpace, once its rows are
// deleted: nothing can reach the space then, and a space made later may be given its ID. A space
// that isn't marked removed is left alone.
func (s *Store) ForgetSpace(sp access.Space) {
	st := s.lockSpace(sp, true, false)
	defer st.mu.Unlock()
	if !st.removed {
		return
	}
	s.mu.Lock()
	delete(s.spaces, sp.ID)
	s.mu.Unlock()
}
