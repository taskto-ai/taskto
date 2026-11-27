package export

import (
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxElemBytes = 120
	maxExtBytes  = 16
	slugBytes    = 60
)

var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"com¹": true, "com²": true, "com³": true, "lpt¹": true, "lpt²": true, "lpt³": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func cleanElem(s string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case r == '/' || r == '\\' || strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
		default:
			b.WriteRune(r)
		}
	}
	name := strings.TrimLeft(b.String(), " ")
	name = strings.TrimRight(name, ". ")
	return finish(shorten(name, limit))
}

func finish(name string) string {
	name = strings.TrimRight(name, " ")
	if strings.Trim(name, ".") == "" {
		return "_"
	}
	if stem, _, _ := strings.Cut(name, "."); reservedNames[strings.ToLower(strings.TrimRight(stem, " "))] {
		name = "_" + name
	}
	return name
}

func shorten(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > maxExtBytes || len(ext) >= limit/2 {
		ext = ""
	}
	stem := cutBytes(strings.TrimSuffix(name, ext), limit-len(ext))
	return strings.TrimRight(stem, ". ") + ext
}

func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func slug(s string) string { return cleanElem(s, slugBytes) }

func foldKey(s string) string { return cases.Fold().String(norm.NFC.String(s)) }

type names struct {
	files   map[string]bool
	dirs    map[string]bool
	folders map[string]string
}

func newNames() *names {
	return &names{files: map[string]bool{}, dirs: map[string]bool{}, folders: map[string]string{}}
}

func (n *names) claim(elems ...string) string {
	assigned := make([]string, 0, len(elems))
	for i, raw := range elems {
		last := i == len(elems)-1
		parent := strings.Join(assigned, "/")
		known := parent + "\x00" + raw
		if !last {
			if name, ok := n.folders[known]; ok {
				assigned = append(assigned, name)
				continue
			}
		}
		clean := cleanElem(raw, maxElemBytes)
		name := clean
		var key string
		for k := 2; ; k++ {
			key = foldKey(strings.Join(append(assigned[:len(assigned):len(assigned)], name), "/"))
			if !n.files[key] && !n.dirs[key] {
				break
			}
			name = withSuffix(clean, " ("+strconv.Itoa(k)+")")
		}
		if last {
			n.files[key] = true
		} else {
			n.dirs[key] = true
			n.folders[known] = name
		}
		assigned = append(assigned, name)
	}
	return strings.Join(assigned, "/")
}

func withSuffix(name, suffix string) string {
	ext := path.Ext(name)
	if len(ext) > maxExtBytes || ext == name {
		ext = ""
	}
	stem := cutBytes(strings.TrimSuffix(name, ext), maxElemBytes-len(ext)-len(suffix))
	return finish(stem + suffix + ext)
}
