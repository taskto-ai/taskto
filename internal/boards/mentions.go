package boards

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/notifications"
)

// A mention is how a comment names a person it wants to tell: `@[Name](user:ID)`, where ID is the
// person's user ID (the ID a space's members list and the agent's list_members tool give) and Name
// is how the writer saw them. People's names aren't unique and can change, so the ID is what
// counts, and the name in the text counts for nothing: the web app shows each mention by the
// person's name as the server resolves it from the ID (Comment.Mentions), never by the label the
// writer typed. The web app's comment box writes the token when a person is picked. Plain text
// like "@alice" is just text.
//
// A token tells somebody only where it is a link in the comment's Markdown, as the web app reads
// it: not in a fenced code block, not in a code span, and not after a backslash escape. The label
// holds none of the characters that would end or escape a Markdown link's label.
var mentionPattern = regexp.MustCompile(`@\[[^\]\[\\\n]{1,200}\]\(user:([0-9]{1,18})\)`)

// maxMentions bounds how many people one comment tells, so a comment can't be made to notify a
// whole workspace at once.
const maxMentions = 20

// Mentions returns the IDs of the people a comment's body mentions, each once, in order, at most
// maxMentions of them. It says nothing of whether they exist or may read the board.
func Mentions(body string) []int64 {
	var ids []int64
	seen := map[int64]bool{}
	for _, m := range mentionPattern.FindAllStringSubmatch(markdownText(body), -1) {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || id < 1 || seen[id] {
			continue
		}
		seen[id] = true
		if ids = append(ids, id); len(ids) == maxMentions {
			break
		}
	}
	return ids
}

// The Markdown reader, ported from the web app (web/src/markdown.tsx: parseBlocks and the helpers
// it uses), so that a token counts as a mention exactly where the web app draws one: only the text
// of paragraphs, headings and table cells is read for them, not code blocks, which the reader
// finds also inside block quotes and list items.
var (
	reFence   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^\\s`]*)")
	reHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	reQuote   = regexp.MustCompile(`^ {0,3}> ?`)
	reItem    = regexp.MustCompile(`^( *)([-*+]|[0-9]{1,9}[.)])([ \t]+|$)`)
	reTable   = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	reDigit   = regexp.MustCompile(`[0-9]`)
	// linkDestination follows a link's label: "(" target and an optional title ")".
	linkDestination = regexp.MustCompile(`^\(\s*(?:<[^<>\n]*>|[^\s()]*(?:\([^\s()]*\)[^\s()]*)*)(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	mentionTarget   = regexp.MustCompile(`^\(user:[0-9]{1,18}\)$`)
	autolink        = regexp.MustCompile(`(?i)^<(?:https?://|mailto:)[^\s<>]+>`)
	bareURL         = regexp.MustCompile("(?i)^https?://[^\\s<>\"'`]+")
	urlStart        = regexp.MustCompile(`(?i)^https?://[^/?#]`)
)

const (
	maxMarkdownDepth = 16
	maxTableColumns  = 64
	maxTableRows     = 1000
)

// closesFence says whether line closes a fence of n of the character ch: at most three spaces,
// then at least n of ch, then only spaces and tabs. Counted by hand: a comment's text is never
// compiled into a pattern.
func closesFence(line string, ch byte, n int) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return len(t) >= n && strings.Trim(t, string(ch)) == ""
}

func blankLine(line string) bool { return strings.TrimSpace(line) == "" }

// isRule is a horizontal rule: three or more of -, * or _ and nothing else but spaces.
func isRule(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || t == "" || !strings.ContainsRune("-*_", rune(t[0])) {
		return false
	}
	n := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case t[0]:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

func startsBlock(line string) bool {
	return reFence.MatchString(line) || reHeading.MatchString(line) || isRule(line) || reQuote.MatchString(line) || reItem.MatchString(line)
}

// splitRow is a table row's cells, without its outer pipes; "\|" is a pipe inside a cell.
func splitRow(line string) []string {
	row := strings.TrimSpace(line)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, "\\|") {
		row = row[:len(row)-1]
	}
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(row); i++ {
		switch {
		case row[i] == '\\' && i+1 < len(row) && row[i+1] == '|':
			cell.WriteByte('|')
			i++
		case row[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(row[i])
		}
	}
	cells = append(cells, strings.TrimSpace(cell.String()))
	return cells[:min(len(cells), maxTableColumns)]
}

func tableAt(lines []string, i int) bool {
	return strings.Contains(lines[i], "|") && i+1 < len(lines) && reTable.MatchString(lines[i+1]) &&
		len(splitRow(lines[i+1])) == len(splitRow(lines[i]))
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " \t\n\r\v\f")) }

// inlineTexts appends to out the texts of lines' blocks that the reader reads inline (paragraphs,
// headings and table cells), as the web app's parseBlocks splits them. Code is left out.
func inlineTexts(lines []string, depth int, out *[]string) {
	if depth > maxMarkdownDepth {
		return // the reader shows what is too deep as code
	}
	i := 0
	for i < len(lines) {
		line := lines[i]
		if blankLine(line) {
			i++
			continue
		}
		if f := reFence.FindStringSubmatch(line); f != nil {
			i++
			for i < len(lines) && !closesFence(lines[i], f[1][0], len(f[1])) {
				i++
			}
			i++ // the closing fence, or past the end
			continue
		}
		if h := reHeading.FindStringSubmatch(line); h != nil {
			*out = append(*out, h[2])
			i++
			continue
		}
		if isRule(line) {
			i++
			continue
		}
		if reQuote.MatchString(line) {
			var body []string
			for i < len(lines) && !blankLine(lines[i]) && (reQuote.MatchString(lines[i]) || !startsBlock(lines[i])) {
				body = append(body, reQuote.ReplaceAllString(lines[i], ""))
				i++
			}
			inlineTexts(body, depth+1, out)
			continue
		}
		if item := reItem.FindStringSubmatch(line); item != nil {
			indent := len(item[1])
			ordered := reDigit.MatchString(item[2])
			for i < len(lines) {
				// Blank lines between two items keep them in one list.
				next := i
				for next < len(lines) && blankLine(lines[next]) {
					next++
				}
				if next > i && next < len(lines) {
					if m := reItem.FindStringSubmatch(lines[next]); m != nil && len(m[1]) == indent {
						i = next
					}
				}
				m := reItem.FindStringSubmatch(lines[i])
				if m == nil || len(m[1]) != indent || reDigit.MatchString(m[2]) != ordered {
					break
				}
				// The item's lines: its first, then those indented past the marker, and lazy
				// continuations of its paragraph.
				width := len(m[0])
				body := []string{lines[i][width:]}
				i++
				for i < len(lines) {
					next := lines[i]
					inner := reItem.MatchString(next)
					if blankLine(next) {
						if i+1 < len(lines) && !blankLine(lines[i+1]) && indentOf(lines[i+1]) > indent {
							body = append(body, "")
							i++
							continue
						}
						break
					}
					nextIndent := indentOf(next)
					switch {
					case nextIndent > indent:
						body = append(body, next[min(nextIndent, width):])
					case !inner && !startsBlock(next):
						body = append(body, next)
					default:
						goto itemDone
					}
					i++
				}
			itemDone:
				inlineTexts(body, depth+1, out)
			}
			continue
		}
		if tableAt(lines, i) {
			head := splitRow(line)
			*out = append(*out, head...)
			i += 2
			rows := 0
			for i < len(lines) && !blankLine(lines[i]) && strings.Contains(lines[i], "|") {
				if rows < maxTableRows {
					// A row shows no more cells than the head has.
					cells := splitRow(lines[i])
					*out = append(*out, cells[:min(len(cells), len(head))]...)
					rows++
				}
				i++
			}
			continue
		}
		var para []string
		for i < len(lines) && !blankLine(lines[i]) && (len(para) == 0 || (!startsBlock(lines[i]) && !tableAt(lines, i))) {
			para = append(para, strings.TrimSpace(lines[i]))
			i++
		}
		*out = append(*out, strings.Join(para, "\n"))
	}
}

// markdownText is the running text of body, where a mention token can be: the inline texts of its
// paragraphs, headings and table cells, one per line, with the escaped characters, the code spans
// and the link destinations in them blanked out.
func markdownText(body string) string {
	var texts []string
	inlineTexts(strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(body), "\n"), 0, &texts)
	for i, t := range texts {
		texts[i] = maskInline(t)
	}
	return strings.Join(texts, "\n")
}

// isEscapable is whether a backslash before c escapes it: ASCII punctuation, as in the web app.
func isEscapable(c byte) bool {
	return c >= 0x21 && c <= 0x2f || c >= 0x3a && c <= 0x40 || c >= 0x5b && c <= 0x60 || c >= 0x7b && c <= 0x7e
}

// linkAt reads a link's label and destination from the "[" at b[at], as the web app does: the
// label's brackets balance, and the destination follows at once. end is where the link ends, and
// dest where its destination starts. ok is false if there is no link.
func linkAt(text string, at int) (dest, end int, ok bool) {
	const maxLabel, maxTarget = 999, 4096
	depth := 0
	j := at
	stop := min(len(text), at+maxLabel+2)
	for ; j < stop; j++ {
		if text[j] == '\\' {
			j++
		} else if text[j] == '[' {
			depth++
		} else if text[j] == ']' {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if j >= stop || j+1 >= len(text) || text[j+1] != '(' {
		return 0, 0, false
	}
	m := linkDestination.FindString(text[j+1 : min(len(text), j+1+maxTarget)])
	if m == "" {
		return 0, 0, false
	}
	return j + 1, j + 1 + len(m), true
}

// wordBefore says whether the character before text[i] is a letter, a digit or "_".
func wordBefore(text string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// bareAddress is the http(s) address a word starts with, as the web app takes it: trailing
// punctuation, and a closing parenthesis it didn't open, belong to the sentence. "" if none.
func bareAddress(text string) string {
	u := bareURL.FindString(text)
	if u == "" {
		return ""
	}
	const trail = ".,;:!?*_~"
	u = strings.TrimRight(u, trail)
	for strings.HasSuffix(u, ")") && strings.Count(u, "(") < strings.Count(u, ")") {
		u = strings.TrimRight(u[:len(u)-1], trail)
	}
	if !urlStart.MatchString(u) {
		return ""
	}
	return u
}

// maskInline blanks the escaped characters (with their backslash), the code spans, the link
// destinations, the addresses (autolinks and bare ones) and the images of one paragraph of
// Markdown, except the destination of a mention: the token is what it looks for. A run of backticks with no matching run after it is only text.
func maskInline(text string) string {
	b := []byte(text)
	blankRange := func(from, to int) {
		for k := from; k < to; k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	for i := 0; i < len(b); {
		switch {
		case b[i] == '\\' && i+1 < len(b) && isEscapable(b[i+1]):
			blankRange(i, i+2)
			i += 2
		case b[i] == '`':
			j := i
			for j < len(b) && b[j] == '`' {
				j++
			}
			run := text[i:j]
			if end := strings.Index(text[j:], run); end >= 0 {
				blankRange(i, j+end+len(run))
				i = j + end + len(run)
			} else {
				i = j
			}
		case b[i] == '<' && autolink.MatchString(text[i:min(len(text), i+4096)]):
			end := i + len(autolink.FindString(text[i:min(len(text), i+4096)]))
			blankRange(i, end) // a link: its address isn't text
			i = end
		case (b[i] == 'h' || b[i] == 'H') && !wordBefore(text, i):
			if u := bareAddress(text[i:min(len(text), i+4096)]); u != "" {
				blankRange(i, i+len(u))
				i += len(u)
			} else {
				i++
			}
		case b[i] == '!' && i+1 < len(b) && b[i+1] == '[':
			// An image: its label is shown as it is, not read as Markdown.
			if _, end, ok := linkAt(string(b), i+1); ok {
				blankRange(i, end)
				i = end
			} else {
				i++
			}
		case b[i] == '[':
			dest, end, ok := linkAt(string(b), i)
			switch {
			case !ok:
				i++
			case i > 0 && b[i-1] == '@' && mentionTarget.MatchString(string(b[dest:end])):
				i = end // a mention: its token stays
			default:
				// A link: what is in its label is read on, its destination isn't text.
				blankRange(dest, end)
				i++
			}
		default:
			i++
		}
	}
	return string(b)
}

// recipient is a person a comment tells, and how.
type recipient struct {
	user int64
	kind string
}

// commentRecipients says who a comment on a card tells: each person it mentions and the card's
// assignee, never the commenter, and only those who may read the space (a mention of someone who
// can't, or isn't a member, tells nobody and says nothing of whether they exist). Someone who is
// both is told once, of the mention. It reads inside the comment's own write transaction, which
// holds the write lock, so the assignee and everyone's access are those at the moment the comment
// is written: a reassignment or a removal can't fall between choosing who is told and telling them.
func commentRecipients(ctx context.Context, tx *sql.Tx, sp access.Space, a Actor, boardID, cardID int64, body string) ([]recipient, error) {
	var out []recipient
	told := map[int64]bool{a.UserID: true}
	add := func(id int64, kind string) error {
		if told[id] {
			return nil
		}
		told[id] = true
		ok, err := access.CanReadIn(ctx, tx, id, sp.ID)
		if err != nil || !ok {
			return err
		}
		out = append(out, recipient{id, kind})
		return nil
	}
	for _, id := range Mentions(body) {
		if err := add(id, notifications.KindCardMention); err != nil {
			return nil, err
		}
	}
	var assignee sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT c.assignee_id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE c.id = ? AND b.id = ? AND b.space_id = ?`, cardID, boardID, sp.ID).Scan(&assignee)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if assignee.Valid {
		if err := add(assignee.Int64, notifications.KindCardComment); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// mentionedPeople fills in the Mentions of comments: for each person a comment mentions, the name
// the server has for their ID, if they may read the space (the people the viewer can see in it
// already, who a card is assigned to). Whoever else is mentioned, a stranger, someone who left or
// an ID that doesn't exist, is left out, so a comment can't be used to look up names. The web app
// shows these names and never the label inside the comment's text.
func (s *Service) mentionedPeople(ctx context.Context, sp access.Space, viewer int64, comments []Comment) error {
	var named map[int64]string
	for i := range comments {
		cm := &comments[i]
		cm.Mentions = []MentionedPerson{}
		ids := Mentions(cm.Body)
		if len(ids) == 0 {
			continue
		}
		if named == nil {
			_, members, err := s.access.ReadableSpaceMembers(ctx, viewer, sp.WorkspaceID, sp.ID, auth.RosterPage{})
			if err != nil {
				return err
			}
			named = make(map[int64]string, len(members))
			for _, m := range members {
				named[m.ID] = m.Name
			}
		}
		for _, id := range ids {
			if name, ok := named[id]; ok {
				cm.Mentions = append(cm.Mentions, MentionedPerson{ID: id, Name: name})
			}
		}
	}
	return nil
}

// DeletedMention is the ID a deleted person's mentions are rewritten to: no account has it (IDs
// start at 1, and Mentions skips it), so the token can never resolve to anyone, now or after
// another account registers.
const DeletedMention = 0

// AnonymizeMentions rewrites every mention of userID in the comments of every card to label and
// DeletedMention, for the purge of an account (internal/accountdeletion): the web app never shows a
// mention's label, only the name the server resolves from the ID, but the stored text holds what the
// writer typed, which is the person's name, and the ID would go on naming whoever the number is
// given to. The ID in a token is read as a number, whatever its digits (user:007 is user 7), so a
// padded token is found too. It returns how many comments it changed, and runs in tx, with the
// write lock.
func AnonymizeMentions(ctx context.Context, tx *sql.Tx, userID int64, label string) (int64, error) {
	type row struct {
		id   int64
		body string
	}
	// Every comment with a token; the number inside is compared below, as a number.
	rows, err := tx.QueryContext(ctx, `SELECT id, body FROM card_comments WHERE instr(body, '(user:') > 0`)
	if err != nil {
		return 0, err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.body); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, r := range todo {
		body := mentionPattern.ReplaceAllStringFunc(r.body, func(m string) string {
			sub := mentionPattern.FindStringSubmatch(m)
			if len(sub) != 2 {
				return m
			}
			if id, err := strconv.ParseInt(sub[1], 10, 64); err != nil || id != userID {
				return m
			}
			return "@[" + label + "](user:" + strconv.Itoa(DeletedMention) + ")"
		})
		if body == r.body {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_comments SET body = ? WHERE id = ?`, body, r.id); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
