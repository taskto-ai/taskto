package dictation

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

const GlossaryFile = "glossary.md"

const (
	maxTerms   = 500
	maxTermLen = 60
)

type Docs interface {
	Read(ctx context.Context, sp access.Space, name, at string) (knowledge.Doc, error)
}

var (
	listItem = regexp.MustCompile(`^\s{0,3}(?:[-*+]|\d{1,3}[.)])\s+(?:\[[ xX]\]\s+)?(.+)$`)
	mdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

func ParseGlossary(md []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(md), "\r\n", "\n"), "\n")
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				start = i + 1
				break
			}
		}
	}
	var terms []string
	fence := ""
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence = trimmed[:3]
			continue
		}
		m := listItem.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if t := termOf(m[1]); t != "" {
			terms = append(terms, t)
		}
	}
	return terms
}

func termOf(item string) string {
	item = mdLink.ReplaceAllString(item, "$1")
	for _, sep := range []string{":", " — ", " – ", " - ", "—", "–"} {
		if i := strings.Index(item, sep); i >= 0 {
			item = item[:i]
		}
	}
	item = strings.NewReplacer("**", "", "__", "", "`", "").Replace(item)
	item = strings.Trim(item, " \t*_.,;!?\"'()")
	if item == "" || utf8.RuneCountInString(item) > maxTermLen || !strings.ContainsFunc(item, isWordRune) {
		return ""
	}
	for _, r := range item {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return item
}

func glossary(ctx context.Context, c *access.Checker, docs Docs, userID, workspaceID int64) []string {
	spaces, err := c.Readable(ctx, userID, workspaceID)
	if err != nil {
		slog.Warn("dictation: listing the spaces for the glossary", "workspace", workspaceID, "err", err)
		return nil
	}
	seen := map[string]bool{}
	var terms []string
	for _, sp := range spaces {
		doc, err := docs.Read(ctx, sp, GlossaryFile, "")
		if errors.Is(err, knowledge.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.Warn("dictation: reading a space's glossary", "space", sp.ID, "err", err)
			continue
		}
		for _, t := range ParseGlossary(doc.Content) {
			k := strings.ToLower(t)
			if seen[k] {
				continue
			}
			seen[k] = true
			terms = append(terms, t)
			if len(terms) == maxTerms {
				return terms
			}
		}
	}
	return terms
}
