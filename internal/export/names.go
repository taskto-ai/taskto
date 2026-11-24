package export

import (
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The names inside the zip come from people (a document's path, a board's name, a
// conversation's title) and are never joined to a file system path: the zip is a stream, and
// whoever unpacks it must find each entry inside the folder they unpack to, on any system.
// So every name is cleaned element by element (cleanElem) and made unique (names.claim).

const (
	// maxElemBytes bounds one element of an entry's name, below the 255 bytes most file
	// systems allow, so the extension and a " (2)" still fit.
	maxElemBytes = 120
	// maxExtBytes is how much of an extension is kept when a name is cut short.
	maxExtBytes = 16
	// slugBytes bounds the part of a generated name made of a board's, app's or conversation's
	// own name.
	slugBytes = 60
)

// reservedNames are the names Windows won't make a file of, with or without an extension.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// cleanElem makes s a safe single element of a path in the zip: no separators, control or
// formatting characters, none of the characters Windows refuses, no ".." or "." and no trailing
// dot or space, not a name Windows reserves, valid UTF-8, and at most limit bytes with its
// extension kept. It never returns "".
func cleanElem(s string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case r == '/' || r == '\\' || strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			// dropped: it can hide or reorder what a name says
		default:
			b.WriteRune(r)
		}
	}
	name := strings.TrimLeft(b.String(), " ")
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "_"
	}
	if stem, _, _ := strings.Cut(name, "."); reservedNames[strings.ToLower(strings.TrimRight(stem, " "))] {
		name = "_" + name
	}
	return shorten(name, limit)
}

// shorten cuts name to at most limit bytes at a character boundary, keeping a short extension.
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

// cutBytes is s cut to at most n bytes, never in the middle of a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// slug is a short readable form of a name for an entry's name: its cleaned, shortened text.
func slug(s string) string { return cleanElem(s, slugBytes) }

// names hands out the entry names of one zip, so no two are the same: not even on a file system
// that ignores case, and none is both a folder and a file.
type names struct {
	files map[string]bool // lower-cased full names of entries
	dirs  map[string]bool // lower-cased folders that entries are in
}

func newNames() *names { return &names{files: map[string]bool{}, dirs: map[string]bool{}} }

// claim returns the entry name for elems, a path made of elements that cleanElem made safe: elems
// joined with "/", with " (2)", " (3)"… added to the first element that collides, if any does.
func (n *names) claim(elems ...string) string {
	elems = append([]string(nil), elems...)
	for i := range elems {
		last := i == len(elems)-1
		orig := elems[i]
		for k := 2; ; k++ {
			key := strings.ToLower(strings.Join(elems[:i+1], "/"))
			if last && !n.files[key] && !n.dirs[key] || !last && !n.files[key] {
				break
			}
			elems[i] = withSuffix(orig, " ("+strconv.Itoa(k)+")")
		}
	}
	full := strings.Join(elems, "/")
	n.files[strings.ToLower(full)] = true
	for i := 1; i < len(elems); i++ {
		n.dirs[strings.ToLower(strings.Join(elems[:i], "/"))] = true
	}
	return full
}

// withSuffix puts suffix before name's extension, keeping the result within maxElemBytes.
func withSuffix(name, suffix string) string {
	ext := path.Ext(name)
	if len(ext) > maxExtBytes || ext == name {
		ext = ""
	}
	stem := cutBytes(strings.TrimSuffix(name, ext), maxElemBytes-len(ext)-len(suffix))
	return stem + suffix + ext
}
