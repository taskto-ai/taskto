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
	"com¹": true, "com²": true, "com³": true, "lpt¹": true, "lpt²": true, "lpt³": true,
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
	return finish(shorten(name, limit))
}

// finish makes name a name that can be made a file of: cutting a name short can leave nothing
// (a long run of dots before a long "extension"), and a cut can leave a name Windows reserves. It
// is the last step of every name, after it is cut and after a suffix is added.
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

// foldKey is what two names are the same by, for a file system that treats them so: macOS and
// Windows unpack é written as one character or as two to the same name, and ignore case.
func foldKey(s string) string { return cases.Fold().String(norm.NFC.String(s)) }

// names hands out the entry names of one zip, so no two are the same by foldKey, and none is both
// a folder and a file. A folder is remembered by where it is and the name it had, so the files of
// one folder stay together, and two folders whose names are cleaned to the same name, or differ
// only in case or in how an accent is written, are two folders.
type names struct {
	files   map[string]bool   // foldKeys of entries
	dirs    map[string]bool   // foldKeys of folders that entries are in
	folders map[string]string // an assigned parent path and a folder's own name: the name it was given
}

func newNames() *names {
	return &names{files: map[string]bool{}, dirs: map[string]bool{}, folders: map[string]string{}}
}

// claim returns the entry name for elems, the elements of a path as people named them: each is
// cleaned (cleanElem), and made unique among its siblings with " (2)", " (3)"… before its
// extension if it collides, so the entry's whole name is unique too.
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

// withSuffix puts suffix before name's extension, keeping the result within maxElemBytes.
func withSuffix(name, suffix string) string {
	ext := path.Ext(name)
	if len(ext) > maxExtBytes || ext == name {
		ext = ""
	}
	stem := cutBytes(strings.TrimSuffix(name, ext), maxElemBytes-len(ext)-len(suffix))
	return finish(stem + suffix + ext)
}
