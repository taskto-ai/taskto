package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func decodeQP(t *testing.T, s string) string {
	t.Helper()
	b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every language's file is the reference's shape: no message or placeholder English
// doesn't have, the same placeholders and paragraphs in each message, and a tag the server
// stores. A translation may leave a message out: it is English until someone translates it.
func TestLanguagesMatchEnglish(t *testing.T) {
	c := DefaultCatalog()
	en := c.langs["en"]
	if len(en.Messages) == 0 || en.Footer == "" {
		t.Fatal("English has no messages")
	}
	tag := regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)
	names := func(s string) []string {
		var out []string
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	for _, lang := range c.Languages() {
		if !tag.MatchString(lang) {
			t.Errorf("%s: not a language tag in its canonical case", lang)
		}
		f := c.langs[lang]
		if lang != "en" && !slices.Equal(names(f.Footer), names(en.Footer)) {
			t.Errorf("%s: footer placeholders differ from English", lang)
		}
		for name, m := range f.Messages {
			ref, ok := en.Messages[name]
			if !ok {
				t.Errorf("%s: message %q isn't in English", lang, name)
				continue
			}
			if len(m.Paragraphs) != len(ref.Paragraphs) || (m.Action == "") != (ref.Action == "") || m.Subject == "" || m.Title == "" {
				t.Errorf("%s: message %q isn't English's shape", lang, name)
				continue
			}
			all := func(m message) string { return m.Subject + " " + m.Title + " " + strings.Join(m.Paragraphs, " ") }
			if !slices.Equal(names(all(m)), names(all(ref))) {
				t.Errorf("%s: message %q has other placeholders than English: %v vs %v", lang, name, names(all(m)), names(all(ref)))
			}
		}
	}
	// This card ships English and Brazilian Portuguese with every message.
	pt := c.langs["pt-BR"]
	for name := range en.Messages {
		if _, ok := pt.Messages[name]; !ok {
			t.Errorf("pt-BR lacks %q", name)
		}
	}
}

func TestResolve(t *testing.T) {
	c := DefaultCatalog()
	for tag, want := range map[string]string{
		"": "en", "en": "en", "en-GB": "en", "pt-BR": "pt-BR", "pt": "pt-BR", "pt-PT": "pt-BR",
		"de": "en", "zh-Hant-TW": "en", "xx-YY": "en",
	} {
		if got := c.Resolve(tag); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestRenderTranslates(t *testing.T) {
	c := DefaultCatalog()
	params := map[string]string{"name": "Ana"}
	en, err := c.Render("", "test", params)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := c.Render("pt-PT", "test", params)
	if err != nil {
		t.Fatal(err)
	}
	if en.Language != "en" || pt.Language != "pt-BR" || en.Subject == pt.Subject {
		t.Fatalf("en %q (%s), pt %q (%s)", en.Subject, en.Language, pt.Subject, pt.Language)
	}
	if !strings.Contains(pt.Text, "Ana") || !strings.Contains(pt.Text, "servidor") || !strings.Contains(pt.HTML, `lang="pt-BR"`) {
		t.Fatalf("pt: %q / %q", pt.Text, pt.HTML)
	}
	if !strings.Contains(en.Text, "-- \n") || !strings.HasSuffix(en.Text, "\n") {
		t.Fatalf("text = %q", en.Text)
	}
}

func TestRenderEscapesHTMLAndKeepsTheSubjectOnOneLine(t *testing.T) {
	c := &Catalog{langs: map[string]file{"en": {Footer: "Footer", Messages: map[string]message{
		"m": {Subject: "Hi {name}", Title: "Title {name}", Paragraphs: []string{"Hello {name}"}, Action: "Open"},
	}}}, order: []string{"en"}}
	got, err := c.Render("en", "m", map[string]string{"name": "<b>x</b>\r\nBcc: y", "action_url": `https://example.com/?a=1&b="2"`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got.Subject, "\r\n") {
		t.Fatalf("subject = %q", got.Subject)
	}
	if strings.Contains(got.HTML, "<b>x</b>") || !strings.Contains(got.HTML, "&lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("a value reached the HTML unescaped: %s", got.HTML)
	}
	if !strings.Contains(got.HTML, `href="https://example.com/?a=1&amp;b=&#34;2&#34;"`) {
		t.Fatalf("link = %s", got.HTML)
	}
	if !strings.Contains(got.Text, "Open: https://example.com/?a=1&b=\"2\"") {
		t.Fatalf("text = %q", got.Text)
	}
}

func TestRenderRefusesWhatIsMissing(t *testing.T) {
	c := DefaultCatalog()
	if _, err := c.Render("en", "no-such", nil); err == nil {
		t.Error("an unknown template rendered")
	}
	_, err := c.Render("en", "test", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("a missing placeholder: %v", err)
	}
}

func TestRenderFallsBackToEnglishForAMessageALanguageLacks(t *testing.T) {
	fsys := fstest.MapFS{
		"l/en.json":    {Data: []byte(`{"footer":"F","messages":{"a":{"subject":"A","title":"A","paragraphs":["a"]},"b":{"subject":"B","title":"B","paragraphs":["b"]}}}`)},
		"l/pt-BR.json": {Data: []byte(`{"footer":"R","messages":{"a":{"subject":"Á","title":"Á","paragraphs":["á"]}}}`)},
	}
	c, err := loadCatalog(fsys, "l")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Render("pt-BR", "a", nil)
	b, _ := c.Render("pt-BR", "b", nil)
	if a.Subject != "Á" || a.Language != "pt-BR" || b.Subject != "B" || b.Language != "en" || !strings.Contains(b.Text, "F") {
		t.Fatalf("a %+v b %+v", a, b)
	}
}

func TestRightToLeftLanguages(t *testing.T) {
	for _, tag := range []string{"ar", "he", "fa", "ur", "az-Arab"} {
		if !rtl(tag) {
			t.Errorf("%s isn't right to left", tag)
		}
	}
	for _, tag := range []string{"en", "pt-BR", "de", "zh-Hans"} {
		if rtl(tag) {
			t.Errorf("%s is right to left", tag)
		}
	}
}

func TestServiceSendsATemplateInTheRecipientsLanguage(t *testing.T) {
	var sent []Message
	svc := NewService(senderFunc(func(m Message) error { sent = append(sent, m); return nil }), Info{Host: "h"})
	if err := svc.SendTemplate(ctx(t), "ana@example.com", "pt-BR", "test", map[string]string{"name": "Ana"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].To[0] != "ana@example.com" || !strings.Contains(sent[0].Subject, "teste") || sent[0].HTML == "" {
		t.Fatalf("%+v", sent)
	}
	var none *Service
	if none.Configured() || !errors.Is(none.Send(ctx(t), hello), ErrNotConfigured) ||
		!errors.Is(none.SendTemplate(ctx(t), "a@example.com", "", "test", nil), ErrNotConfigured) || none.Info() != (Info{}) {
		t.Fatal("a nil service must be not configured")
	}
	if got := fmt.Sprint(NewService(nil, Info{}).Configured()); got != "false" {
		t.Fatal("a service without a sender is configured")
	}
}

type senderFunc func(Message) error

func (f senderFunc) Send(_ context.Context, m Message) error { return f(m) }

// A link in a message is an absolute http or https URL: the value comes from elsewhere in the
// server, and javascript:, data: or a path would make a message that attacks its reader.
func TestRenderOnlyLinksToWebAddresses(t *testing.T) {
	c := &Catalog{langs: map[string]file{"en": {Footer: "Footer", Messages: map[string]message{
		"m": {Subject: "Hi", Title: "Title", Paragraphs: []string{"Hello"}, Action: "Open"},
	}}}, order: []string{"en"}}
	for _, ok := range []string{"https://taskto.example.com/reset?t=abc", "http://localhost:8080/x", "HTTPS://Example.com/"} {
		if _, err := c.Render("en", "m", map[string]string{"action_url": ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"javascript:alert(1)", "data:text/html,<b>x</b>", "file:///etc/passwd", "mailto:a@example.com", "ftp://example.com/",
		"//example.com/x", "/reset?t=abc", "example.com/reset", "https:///path", "https://", "https://example.com/a b", "https://exa mple.com",
	} {
		if got, err := c.Render("en", "m", map[string]string{"action_url": bad}); err == nil {
			t.Errorf("%q: rendered %q", bad, got.Text)
		}
	}
}
