package export

import (
	"fmt"
	"io"
	"strings"

	"taskto.ai/taskto/internal/boards"
)

// A board's Markdown file is for reading: its name, then each card, in the order of their IDs, with
// its details, its body as it was written, and its comments. The JSON file next to it has
// everything.

func writeBoardHeading(w io.Writer, b boards.Board) error {
	_, err := fmt.Fprintf(w, "# %s\n", oneLine(b.Name))
	return err
}

func writeCardMarkdown(w io.Writer, c boards.Thread) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s\n\n", oneLine(c.Title))
	fmt.Fprintf(&b, "- Column: %s\n- Created: %s\n- Updated: %s\n", c.Column, c.CreatedAt, c.UpdatedAt)
	if c.Assignee != nil {
		name := c.Assignee.Name
		if c.Assignee.Kind == boards.AssignAgent {
			name = "agent"
		}
		fmt.Fprintf(&b, "- Assigned to: %s\n", oneLine(name))
	}
	if c.BlockedReason != nil {
		fmt.Fprintf(&b, "- Blocked: %s\n", oneLine(*c.BlockedReason))
	}
	for _, d := range c.Docs {
		fmt.Fprintf(&b, "- Document: %s\n", oneLine(d))
	}
	if c.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(c.Body, "\n"))
	}
	for _, cm := range c.Comments {
		who := "(removed account)"
		if cm.Author != nil {
			who = oneLine(cm.Author.Name)
			if cm.Author.Agent {
				who += " (agent)"
			}
		}
		fmt.Fprintf(&b, "\n> **%s**, %s\n", who, cm.CreatedAt)
		for line := range strings.SplitSeq(strings.TrimRight(cm.Body, "\n"), "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// oneLine is s with its line breaks as spaces, for a heading or a list item.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
