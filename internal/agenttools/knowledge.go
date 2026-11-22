package agenttools

import (
	"context"
	"encoding/json"
	"strings"

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

// readDoc reads a Markdown document, readChunk bytes at a time from offset.
func (s *Service) readDoc(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64  `json:"space_id"`
		Path    string `json:"path"`
		Offset  int    `json:"offset"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if !knowledge.IsMarkdown(a.Path) {
		return nil, errInvalid("only Markdown documents (.md) can be read; other files are uploads")
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	doc, err := s.o.Knowledge.Read(ctx, sp, a.Path, "")
	if err != nil {
		return nil, err
	}
	if a.Offset < 0 || a.Offset > len(doc.Content) {
		return nil, invalidf("offset must be 0 to %d, the document's size", len(doc.Content))
	}
	rest := string(doc.Content[a.Offset:])
	content, more := cut(rest, readChunk)
	reply := map[string]any{"path": doc.Path, "version": doc.Version, "size": doc.Size, "offset": a.Offset, "content": content}
	if more {
		reply["next_offset"] = a.Offset + len(content)
	}
	return reply, nil
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
