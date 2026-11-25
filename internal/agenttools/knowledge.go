package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
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

// searchArgs are the arguments of search_docs and search_cards.
type searchArgs struct {
	Query string `json:"query"`
	// SpaceID, Limit and Offset are raw to tell "not given" from null and from 0: null is never
	// a value (a null space would widen the search to every space), and 10.0 is a whole number.
	SpaceID json.RawMessage `json:"space_id"`
	Limit   json.RawMessage `json:"limit"`
	Offset  json.RawMessage `json:"offset"`

	// offset is Offset's value, set by the handlers once they have checked it (searchOffset).
	offset int
}

// searchSpaces checks the arguments of a search, and returns them with the limit and the
// spaces to search: the one asked for, or every space the user can read in the call's
// workspace.
func (s *Service) searchSpaces(ctx context.Context, c *call, args json.RawMessage, off string) (searchArgs, int, []access.Space, error) {
	var a searchArgs
	if err := decode(args, &a); err != nil {
		return a, 0, nil, err
	}
	// The schema counts characters and the index bytes: a query is at most search.MaxQueryLen
	// bytes of UTF-8, which is that many characters only if they are ASCII.
	if len(a.Query) > search.MaxQueryLen {
		return a, 0, nil, invalidf("query is at most %d bytes of UTF-8", search.MaxQueryLen)
	}
	limit := 10
	if a.Limit != nil {
		n, ok := wholeNumber(a.Limit)
		if !ok || n < 1 || n > maxSearchLimit {
			return a, 0, nil, invalidf("limit must be 1 to %d", maxSearchLimit)
		}
		limit = int(n)
	}
	var err error
	if a.offset, err = searchOffset(a.Offset); err != nil {
		return a, 0, nil, err
	}
	if s.o.Search == nil {
		return a, 0, nil, errInvalid(off)
	}
	if a.SpaceID != nil {
		id, ok := wholeNumber(a.SpaceID)
		if !ok || id < 1 {
			return a, 0, nil, invalidf("space_id must be a space's id, from list_spaces; leave it out to search every space")
		}
		sp, err := s.space(ctx, c, id)
		if err != nil {
			return a, 0, nil, err
		}
		return a, limit, []access.Space{sp}, nil
	}
	spaces, err := s.o.Access.Readable(ctx, c.User, c.Workspace)
	return a, limit, spaces, err
}

// maxWhole is the largest whole number a JSON number carries exactly (2^53).
const maxWhole = 1 << 53

// wholeNumber reads a JSON number that is a whole number, as JSON Schema's "integer" says
// ("10.0" is one): not null, not a string, not fractional or out of range.
func wholeNumber(raw json.RawMessage) (int64, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n != math.Trunc(n) || n < -maxWhole || n > maxWhole {
		return 0, false
	}
	return int64(n), true
}

type cardHit struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	BoardID   int64  `json:"board_id"`
	BoardName string `json:"board_name"`
	CardID    int64  `json:"card_id"`
	Title     string `json:"title"`
	Column    string `json:"column"`
	Snippet   string `json:"snippet"`
}

// searchCards searches the cards of the boards in one space, or in every space the user can
// read in the call's workspace, by full text.
func (s *Service) searchCards(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	a, limit, spaces, err := s.searchSpaces(ctx, c, args, "search is off on this server: use list_boards and list_cards")
	if err != nil {
		return nil, err
	}
	res, err := s.o.Search.SearchCards(ctx, s.o.Access, c.User, c.Workspace, spaces, search.Query{Text: a.Query, Limit: limit, Offset: a.offset})
	if err != nil {
		return nil, err
	}
	hits := []cardHit{}
	for _, r := range res.Results {
		var snippet strings.Builder
		for _, seg := range r.Snippet {
			snippet.WriteString(seg.Text)
		}
		text, _ := cut(snippet.String(), 1000)
		hits = append(hits, cardHit{SpaceID: r.SpaceID, SpaceName: r.SpaceName, BoardID: r.BoardID, BoardName: r.BoardName,
			CardID: r.CardID, Title: r.Title, Column: r.Column, Snippet: text})
	}
	reply := map[string]any{"results": hits}
	if res.Incomplete {
		reply["incomplete"] = true
	}
	if res.NextOffset != nil {
		reply["next_offset"] = *res.NextOffset
	}
	return reply, nil
}

// searchDocs searches the knowledge base: one space, or every space the user can read in
// the call's workspace.
func (s *Service) searchDocs(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	a, limit, spaces, err := s.searchSpaces(ctx, c, args, "search is off on this server: use list_docs and read_doc")
	if err != nil {
		return nil, err
	}
	res, err := s.o.Search.Search(ctx, c.Workspace, spaces, search.Query{Text: a.Query, Limit: limit, Offset: a.offset})
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
	// The store decides what the file is before it loads it: unversioned, too large for text, not
	// UTF-8 or with a NUL byte are refused there, whatever the file's name.
	doc, err := s.o.Knowledge.ReadText(ctx, sp, a.Path, a.At, maxTextFile)
	if err != nil {
		return nil, err
	}
	if a.Offset < 0 || a.Offset > len(doc.Content) {
		return nil, invalidf("offset must be 0 to %d, the document's size", len(doc.Content))
	}
	// next_offset is always at a character's start, so one that isn't is a made-up number: a read
	// from inside a character would return replacement characters, not the text.
	if a.Offset < len(doc.Content) && !utf8.RuneStart(doc.Content[a.Offset]) {
		return nil, invalidf("offset %d is inside a character: use 0 or a next_offset from an earlier read", a.Offset)
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

// moveDoc renames a document or folder, as the user, committed by the agent. A rename that
// would change a file between versioned and unversioned storage is refused: it takes the file's
// older versions out of history.
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
	return s.o.Knowledge.MoveKept(ctx, sp, a.From, a.To, a.BaseVersion, by, a.Message)
}

// deleteDoc deletes a document; its history stays, so restore_doc brings it back. An unversioned
// file is refused: its bytes would be gone for good, and the built-in agent has no one to ask.
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
	return s.o.Knowledge.DeleteKept(ctx, sp, a.Path, a.BaseVersion, by, a.Message)
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
	return s.o.Knowledge.RestoreKept(ctx, sp, a.Path, a.Commit, a.BaseVersion, by, a.Message)
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

type recordHit struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	AppID     int64  `json:"app_id"`
	AppName   string `json:"app_name"`
	RecordID  int64  `json:"record_id"`
	Snippet   string `json:"snippet"`
}

// searchRecords searches the records of the apps in one space, or in every space the user can
// read in the call's workspace, by full text: only the records the user may read in each app
// (their roles in it decide: every record, or only those they made), and only of the values of
// the fields the app's live release declares.
func (s *Service) searchRecords(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	a, limit, spaces, err := s.searchSpaces(ctx, c, args, "search is off on this server: use list_apps and list_app_rows")
	if err != nil {
		return nil, err
	}
	res, err := s.o.Search.SearchRecords(ctx, s.o.Access, c.User, c.Workspace, spaces, search.Query{Text: a.Query, Limit: limit, Offset: a.offset})
	if err != nil {
		return nil, err
	}
	hits := []recordHit{}
	for _, r := range res.Results {
		text, _ := cut(snippetText(r.Snippet), 1000)
		hits = append(hits, recordHit{SpaceID: r.SpaceID, SpaceName: r.SpaceName, AppID: r.AppID, AppName: r.AppName,
			RecordID: r.RecordID, Snippet: text})
	}
	return fitSearchReply(hits, a.offset, res.Incomplete, res.NextOffset)
}

type conversationHit struct {
	ConversationID int64  `json:"conversation_id"`
	Title          string `json:"title"`
	Archived       bool   `json:"archived,omitempty"`
	UpdatedAt      string `json:"updated_at"`
	Snippet        string `json:"snippet,omitempty"`
}

// searchConversations searches the conversations the user owns in the call's workspace, by
// their title and what was said in them: never anyone else's, whatever the arguments.
func (s *Service) searchConversations(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	// Not at the tokens' endpoint (no_token in tools.json): this is the check behind it.
	if c.Conversation == 0 {
		return nil, &Refusal{Status: http.StatusForbidden, Message: "search_conversations works only in a TaskTo.AI conversation"}
	}
	var a searchArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	// space_id is not an argument of this tool: conversations belong to a person, not a space.
	if a.SpaceID != nil {
		return nil, invalidf("search_conversations takes no space_id: it searches the user's own conversations")
	}
	limit, err := searchLimit(a.Limit)
	if err != nil {
		return nil, err
	}
	if a.offset, err = searchOffset(a.Offset); err != nil {
		return nil, err
	}
	if len(a.Query) > search.MaxQueryLen {
		return nil, invalidf("query is at most %d bytes of UTF-8", search.MaxQueryLen)
	}
	if s.o.Search == nil {
		return nil, errInvalid("search is off on this server")
	}
	res, err := s.o.Search.SearchConversations(ctx, c.User, c.Workspace, search.Query{Text: a.Query, Limit: limit, Offset: a.offset})
	if err != nil {
		return nil, err
	}
	hits := []conversationHit{}
	for _, r := range res.Results {
		text, _ := cut(snippetText(r.Snippet), 1000)
		hits = append(hits, conversationHit{ConversationID: r.ConversationID, Title: r.Title, Archived: r.Archived,
			UpdatedAt: r.UpdatedAt, Snippet: text})
	}
	return fitSearchReply(hits, a.offset, res.Incomplete, res.NextOffset)
}

// snippetText is a snippet's text, without its marks.
func snippetText(segs []search.Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		b.WriteString(seg.Text)
	}
	return b.String()
}

// searchLimit reads a search's limit, which is raw to tell "not given" from null and 0.
func searchLimit(raw json.RawMessage) (int, error) {
	if raw == nil {
		return 10, nil
	}
	n, ok := wholeNumber(raw)
	if !ok || n < 1 || n > maxSearchLimit {
		return 0, invalidf("limit must be 1 to %d", maxSearchLimit)
	}
	return int(n), nil
}

// searchOffset reads a search's offset, which is raw to tell "not given" from null: a whole
// number from 0 to search.MaxOffset, as the schema says ("10.0" is one).
func searchOffset(raw json.RawMessage) (int, error) {
	if raw == nil {
		return 0, nil
	}
	n, ok := wholeNumber(raw)
	if !ok || n < 0 || n > search.MaxOffset {
		return 0, invalidf("offset must be 0 to %d", search.MaxOffset)
	}
	return int(n), nil
}

// fitSearchReply is a search's reply, with as many of its hits as fit in MaxResult once encoded
// (JSON escapes some characters of a snippet as six bytes): when it has to leave hits out, its
// next_offset is the first of them, so the next page returns them, if that offset is one a search
// accepts (search.MaxOffset: the schema's maximum). An incomplete reply has no next_offset,
// whatever fits: it is searched again instead.
func fitSearchReply[H any](hits []H, offset int, incomplete bool, next *int) (any, error) {
	build := func(n int) any {
		reply := map[string]any{"results": hits[:n]}
		if incomplete {
			reply["incomplete"] = true
		}
		switch {
		case incomplete:
		case n < len(hits):
			if offset+n <= search.MaxOffset {
				reply["next_offset"] = offset + n
			}
		case next != nil:
			reply["next_offset"] = *next
		}
		return reply
	}
	_, reply, err := fitting(len(hits), build)
	return reply, err
}
