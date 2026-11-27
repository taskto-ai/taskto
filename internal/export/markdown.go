package export

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/skills"
)

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
		b.WriteString("\n")
		for line := range strings.SplitSeq(strings.TrimRight(c.Body, "\n"), "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	for _, cm := range c.Comments {
		who := "(removed account)"
		if cm.Author != nil {
			who = oneLine(cm.Author.Name)
			if cm.Author.Agent {
				who += " (agent)"
			}
		}
		fmt.Fprintf(&b, "\n> > **%s**, %s\n", who, cm.CreatedAt)
		for line := range strings.SplitSeq(strings.TrimRight(cm.Body, "\n"), "\n") {
			fmt.Fprintf(&b, "> > %s\n", line)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func yamlText(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(b.String(), "\n")
}

func writeSkillMarkdown(w io.Writer, s skills.Skill) error {
	_, err := fmt.Fprintf(w, "---\nname: %s\ntitle: %s\ndescription: %s\nenabled: %t\n---\n\n%s\n",
		yamlText(s.Name), yamlText(s.Title), yamlText(s.Description), s.Enabled, strings.TrimRight(s.Body, "\n"))
	return err
}
