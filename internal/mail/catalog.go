package mail

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed locales/*.json
var localeFiles embed.FS

type file struct {
	Footer   string             `json:"footer"`
	Messages map[string]message `json:"messages"`
}

type message struct {
	Subject    string   `json:"subject"`
	Title      string   `json:"title"`
	Paragraphs []string `json:"paragraphs"`
	Action     string   `json:"action"`
}

type Catalog struct {
	langs map[string]file
	order []string
}

var (
	defaultOnce    sync.Once
	defaultCatalog *Catalog
)

func DefaultCatalog() *Catalog {
	defaultOnce.Do(func() {
		c, err := loadCatalog(localeFiles, "locales")
		if err != nil {
			panic(err)
		}
		defaultCatalog = c
	})
	return defaultCatalog
}

func loadCatalog(fsys fs.FS, dir string) (*Catalog, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	c := &Catalog{langs: map[string]file{}}
	for _, e := range entries {
		tag, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		raw, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		var f file
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("mail: %s: %w", e.Name(), err)
		}
		c.langs[tag] = f
		c.order = append(c.order, tag)
	}
	if _, ok := c.langs[fallback]; !ok {
		return nil, fmt.Errorf("mail: no %s templates", fallback)
	}
	slices.Sort(c.order)
	return c, nil
}

const fallback = "en"

func (c *Catalog) Languages() []string { return slices.Clone(c.order) }

func (c *Catalog) Resolve(tag string) string {
	if _, ok := c.langs[tag]; ok {
		return tag
	}
	primary, _, _ := strings.Cut(tag, "-")
	if primary == "" {
		return fallback
	}
	if _, ok := c.langs[primary]; ok {
		return primary
	}
	for _, t := range c.order {
		if p, _, _ := strings.Cut(t, "-"); p == primary {
			return t
		}
	}
	return fallback
}

type Content struct {
	Language string
	Subject  string
	Text     string
	HTML     string
}

var placeholder = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

func (c *Catalog) Render(locale, name string, params map[string]string) (Content, error) {
	lang := c.Resolve(locale)
	f := c.langs[lang]
	msg, ok := f.Messages[name]
	if !ok {
		lang, f = fallback, c.langs[fallback]
		if msg, ok = f.Messages[name]; !ok {
			return Content{}, fmt.Errorf("mail: no template %q", name)
		}
	}
	var missing []string
	fill := func(s string, escape func(string) string) string {
		return placeholder.ReplaceAllStringFunc(s, func(m string) string {
			key := m[1 : len(m)-1]
			v, ok := params[key]
			if !ok {
				missing = append(missing, key)
				return m
			}
			return escape(v)
		})
	}
	same := func(s string) string { return s }

	subject := strings.Join(strings.Fields(fill(msg.Subject, same)), " ")
	title := fill(msg.Title, same)
	var paragraphs []string
	for _, p := range msg.Paragraphs {
		paragraphs = append(paragraphs, fill(p, same))
	}
	footer := f.Footer
	actionURL := ""
	if msg.Action != "" {
		actionURL = params["action_url"]
		if actionURL == "" {
			missing = append(missing, "action_url")
		}
	}
	if len(missing) > 0 {
		return Content{}, fmt.Errorf("mail: template %q needs the params %s", name, strings.Join(missing, ", "))
	}
	if actionURL != "" && !webURL(actionURL) {
		return Content{}, fmt.Errorf("mail: template %q needs an absolute http or https action_url", name)
	}

	var text strings.Builder
	text.WriteString(title + "\n\n")
	for _, p := range paragraphs {
		text.WriteString(p + "\n\n")
	}
	if actionURL != "" {
		text.WriteString(msg.Action + ": " + actionURL + "\n\n")
	}
	text.WriteString("-- \n" + footer + "\n")

	var h strings.Builder
	dir := "ltr"
	if rtl(lang) {
		dir = "rtl"
	}
	fmt.Fprintf(&h, `<!doctype html><html lang="%s" dir="%s"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>`, html.EscapeString(lang), dir)
	h.WriteString(`<body style="margin:0;padding:24px;background:#f5f5f4;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1c1917;line-height:1.5">`)
	h.WriteString(`<div style="max-width:560px;margin:0 auto;background:#ffffff;border-radius:8px;padding:24px">`)
	fmt.Fprintf(&h, `<h1 style="font-size:20px;margin:0 0 16px">%s</h1>`, html.EscapeString(title))
	for _, p := range paragraphs {
		fmt.Fprintf(&h, `<p style="margin:0 0 16px">%s</p>`, html.EscapeString(p))
	}
	if actionURL != "" {
		fmt.Fprintf(&h, `<p style="margin:0 0 16px"><a href="%s" style="display:inline-block;padding:10px 18px;background:#1c1917;color:#ffffff;border-radius:6px;text-decoration:none">%s</a></p>`,
			html.EscapeString(actionURL), html.EscapeString(msg.Action))
		fmt.Fprintf(&h, `<p style="margin:0 0 16px;font-size:13px;color:#57534e;word-break:break-all">%s</p>`, html.EscapeString(actionURL))
	}
	fmt.Fprintf(&h, `<p style="margin:24px 0 0;font-size:13px;color:#57534e">%s</p></div></body></html>`, html.EscapeString(footer))

	return Content{Language: lang, Subject: subject, Text: text.String(), HTML: h.String()}, nil
}

func webURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func rtl(lang string) bool {
	primary, _, _ := strings.Cut(lang, "-")
	switch primary {
	case "ar", "he", "fa", "ur", "ps", "sd", "ug", "yi", "dv":
		return true
	}
	return strings.Contains(lang, "-Arab") || strings.Contains(lang, "-Hebr")
}
