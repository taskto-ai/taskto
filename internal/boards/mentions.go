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

var mentionPattern = regexp.MustCompile(`@\[[^\]\[\\\n]{1,200}\]\(user:([0-9]{1,18})\)`)

var anonymizedMentionPattern = regexp.MustCompile(`@\[(?:[^\]\[\\\n]|\\\|){1,200}\]\(user:([0-9]{1,18})\)`)

const maxMentions = 20

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

var (
	reFence         = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^\\s`]*)")
	reHeading       = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	reQuote         = regexp.MustCompile(`^ {0,3}> ?`)
	reItem          = regexp.MustCompile(`^( *)([-*+]|[0-9]{1,9}[.)])([ \t]+|$)`)
	reTable         = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	reDigit         = regexp.MustCompile(`[0-9]`)
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

func closesFence(line string, ch byte, n int) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return len(t) >= n && strings.Trim(t, string(ch)) == ""
}

func blankLine(line string) bool { return strings.TrimSpace(line) == "" }

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

func inlineTexts(lines []string, depth int, out *[]string) {
	if depth > maxMarkdownDepth {
		return
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
			i++
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

func markdownText(body string) string {
	var texts []string
	inlineTexts(strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(body), "\n"), 0, &texts)
	for i, t := range texts {
		texts[i] = maskInline(t)
	}
	return strings.Join(texts, "\n")
}

func isEscapable(c byte) bool {
	return c >= 0x21 && c <= 0x2f || c >= 0x3a && c <= 0x40 || c >= 0x5b && c <= 0x60 || c >= 0x7b && c <= 0x7e
}

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

func wordBefore(text string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

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
			blankRange(i, end)
			i = end
		case (b[i] == 'h' || b[i] == 'H') && !wordBefore(text, i):
			if u := bareAddress(text[i:min(len(text), i+4096)]); u != "" {
				blankRange(i, i+len(u))
				i += len(u)
			} else {
				i++
			}
		case b[i] == '!' && i+1 < len(b) && b[i+1] == '[':
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
				i = end
			default:
				blankRange(dest, end)
				i++
			}
		default:
			i++
		}
	}
	return string(b)
}

type recipient struct {
	user int64
	kind string
}

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

const DeletedMention = 0

func AnonymizeMentions(ctx context.Context, tx *sql.Tx, userID int64, label string) (int64, error) {
	type row struct {
		id   int64
		body string
	}
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
		body := anonymizedMentionPattern.ReplaceAllStringFunc(r.body, func(m string) string {
			sub := anonymizedMentionPattern.FindStringSubmatch(m)
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
