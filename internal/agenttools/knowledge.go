package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/search"
)

// Bounds of the knowledge-base tools.
const (
	// readChunk is the most of a document read_doc returns at once, in bytes.
	readChunk = 32 << 10
	// maxSearchLimit is the most results search_docs returns at once.
	maxSearchLimit = 20
	// maxListLimit is the most documents list_docs returns at once.
	maxListLimit = 200
	// maxHistoryLimit is the most commits doc_history returns at once.
	maxHistoryLimit = 50
	// maxTextFile is the largest plain-text file (not Markdown) read_doc reads, in bytes.
	maxTextFile = 1 << 20
)

type spaceReply struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Kind     access.Kind `json:"kind"`
	Access   string      `json:"access"` // "read" or "write"
	Archived bool        `json:"archived"`
}

// listSpaces lists the spaces the user can read in the call's workspace: where
// the other tools' space_id come from.
func (s *Service) listSpaces(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	if err := decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	spaces, err := s.o.Access.Readable(ctx, c.User, c.Workspace)
	if err != nil {
		return nil, err
	}
	out := []spaceReply{}
	for _, sp := range spaces {
		out = append(out, spaceReply{ID: sp.ID, Name: sp.Name, Kind: sp.Kind, Access: string(sp.Access), Archived: sp.ArchivedAt != nil})
	}
	n, res, err := fitting(len(out), func(n int) any { return map[string]any{"spaces": out[:n]} })
	if err == nil && n < len(out) {
		res = map[string]any{"spaces": out[:n], "truncated": true}
	}
	return res, err
}

type searchHit struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	Version   string `json:"version"`
}

// searchDocs searches the knowledge base: one space, or every space the user can read in
// the call's workspace.
func (s *Service) searchDocs(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		Query   string `json:"query"`
		SpaceID *int64 `json:"space_id"`
		Limit   int    `json:"limit"`
		Offset  int    `json:"offset"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Limit < 0 || a.Limit > maxSearchLimit {
		return nil, invalidf("limit must be 1 to %d", maxSearchLimit)
	}
	if a.Limit == 0 {
		a.Limit = 10
	}
	if s.o.Search == nil {
		return nil, errInvalid("search is off on this server: use list_docs and read_doc")
	}
	var spaces []access.Space
	if a.SpaceID != nil {
		sp, err := s.space(ctx, c, *a.SpaceID)
		if err != nil {
			return nil, err
		}
		spaces = []access.Space{sp}
	} else {
		var err error
		if spaces, err = s.o.Access.Readable(ctx, c.User, c.Workspace); err != nil {
			return nil, err
		}
	}
	res, err := s.o.Search.Search(ctx, c.Workspace, spaces, search.Query{Text: a.Query, Limit: a.Limit, Offset: a.Offset})
	if err != nil {
		return nil, err
	}
	hits := []searchHit{}
	for _, r := range res.Results {
		var snippet strings.Builder
		for _, seg := range r.Snippet {
			snippet.WriteString(seg.Text)
		}
		text, _ := cut(snippet.String(), 1000)
		hits = append(hits, searchHit{SpaceID: r.SpaceID, SpaceName: r.SpaceName, Path: r.Path, Title: r.Title, Snippet: text, Version: r.Version})
	}
	reply := map[string]any{"results": hits, "mode": res.Mode}
	if res.Notice != "" {
		reply["notice"] = res.Notice
	}
	if res.NextOffset != nil {
		reply["next_offset"] = *res.NextOffset
	}
	return reply, nil
}

// listDocs lists a space's documents and uploads, by path, a page at a time.
func (s *Service) listDocs(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64  `json:"space_id"`
		Folder  string `json:"folder"`
		After   string `json:"after"`
		Limit   int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Limit < 0 || a.Limit > maxListLimit {
		return nil, invalidf("limit must be 1 to %d", maxListLimit)
	}
	if a.Limit == 0 {
		a.Limit = 100
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	page, err := s.o.Knowledge.List(ctx, sp, strings.Trim(a.Folder, "/"), a.After, a.Limit)
	if err != nil {
		return nil, err
	}
	// Long paths can make a page too large: return fewer, and the next page from there.
	_, res, err := fitting(len(page.Docs), func(n int) any {
		if n == len(page.Docs) {
			return page
		}
		next := a.After
		if n > 0 {
			next = page.Docs[n-1].Path
		}
		return knowledge.Page{Docs: page.Docs[:n], Next: next}
	})
	return res, err
}

// readDoc reads a Markdown document, or a plain-text file of at most maxTextFile bytes,
// readChunk bytes at a time from offset: as it is now, or as it was at the commit at.
func (s *Service) readDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64  `json:"space_id"`
		Path    string `json:"path"`
		Offset  int    `json:"offset"`
		At      string `json:"at"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	doc, err := s.o.Knowledge.Read(ctx, sp, a.Path, a.At)
	if err != nil {
		return nil, err
	}
	if !doc.Versioned {
		// Its bytes are on disk, not in git, and can be any size: only list_docs shows it.
		return nil, invalidf("%s is an unversioned file (a large file, audio, video or an archive): it can't be read as text", doc.Path)
	}
	if !knowledge.IsMarkdown(doc.Path) {
		switch {
		case doc.Size > maxTextFile:
			return nil, invalidf("%s is %d bytes: only text files up to %d KiB can be read", doc.Path, doc.Size, maxTextFile>>10)
		case bytes.IndexByte(doc.Content, 0) >= 0 || !utf8.Valid(doc.Content):
			return nil, invalidf("%s isn't a text file: only Markdown documents and plain-text files can be read", doc.Path)
		}
	}
	if a.Offset < 0 || a.Offset > len(doc.Content) {
		return nil, invalidf("offset must be 0 to %d, the document's size", len(doc.Content))
	}
	rest := string(doc.Content[a.Offset:])
	content, more := cut(rest, readChunk)
	for {
		reply := map[string]any{"path": doc.Path, "version": doc.Version, "size": doc.Size, "offset": a.Offset, "content": content}
		if a.At != "" {
			reply["at"] = a.At
		}
		if more {
			reply["next_offset"] = a.Offset + len(content)
		}
		// JSON escapes characters like < to six bytes: a chunk that fills its 32 KiB of text can
		// make a reply over MaxResult, so it is shortened, and the next piece starts after it.
		if out, err := json.Marshal(reply); err != nil || len(out) <= MaxResult || len(content) == 0 {
			return reply, err
		}
		content, _ = cut(content, len(content)/2)
		more = true
	}
}

// docHistory lists the commits that changed a document, or the whole space's, newest first.
func (s *Service) docHistory(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64  `json:"space_id"`
		Path    string `json:"path"`
		Before  string `json:"before"`
		Limit   int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Limit < 0 || a.Limit > maxHistoryLimit {
		return nil, invalidf("limit must be 1 to %d", maxHistoryLimit)
	}
	if a.Limit == 0 {
		a.Limit = 20
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	h, err := s.o.Knowledge.History(ctx, sp, a.Path, a.Before, a.Limit)
	if err != nil {
		return nil, err
	}
	// A page can be too long with long messages: return fewer, and the next page from there.
	_, res, err := fitting(len(h.Commits), func(n int) any {
		if n == len(h.Commits) {
			return h
		}
		next := a.Before
		if n > 0 {
			next = h.Commits[n-1].Commit
		}
		return knowledge.History{Commits: h.Commits[:n], Next: next}
	})
	return res, err
}

// moveDoc renames a document or folder, as the user, committed by the agent.
func (s *Service) moveDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID     int64  `json:"space_id"`
		From        string `json:"from"`
		To          string `json:"to"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	by, err := s.author(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.o.Knowledge.Move(ctx, sp, a.From, a.To, a.BaseVersion, by, a.Message)
}

// deleteDoc deletes a document or file; its history stays, so restore_doc brings it back.
func (s *Service) deleteDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID     int64  `json:"space_id"`
		Path        string `json:"path"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	by, err := s.author(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.o.Knowledge.Delete(ctx, sp, a.Path, a.BaseVersion, by, a.Message)
}

// restoreDoc makes a document what it was at an older commit, as a new commit.
func (s *Service) restoreDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID     int64  `json:"space_id"`
		Path        string `json:"path"`
		Commit      string `json:"commit"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	by, err := s.author(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.o.Knowledge.Restore(ctx, sp, a.Path, a.Commit, a.BaseVersion, by, a.Message)
}

// writeDoc creates or replaces a Markdown document, as the user, committed by the agent.
func (s *Service) writeDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID     int64  `json:"space_id"`
		Path        string `json:"path"`
		Content     string `json:"content"`
		BaseVersion string `json:"base_version"`
		Message     string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if !knowledge.IsMarkdown(a.Path) {
		return nil, errInvalid("only Markdown documents (.md) can be written")
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	by, err := s.author(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.o.Knowledge.Put(ctx, sp, a.Path, []byte(a.Content), a.BaseVersion, by, a.Message)
}

// author is the call's user, as the author of a change the agent makes for them: the
// committer is the agent, or the access token the call came with.
func (s *Service) author(ctx context.Context, c *call) (knowledge.Author, error) {
	by := knowledge.Author{Agent: AgentName}
	if c.Token != "" {
		by.Agent = auth.TokenAgent(c.Token)
	}
	err := s.o.DB.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ?`, c.User).Scan(&by.Name, &by.Email)
	return by, err
}
