// refguard fails when a tracked file refers to something outside the repository: an
// issue-tracker or planning-board number (a hash sign and two to four digits), or a
// private name (a host, a network, another product, a person, a private tool). Docs and
// comments keep the reasoning and drop the reference.
//
// The private names are listed by the sha256 of the lowercased word, so this file doesn't
// spell them out. A word is a run of letters and digits; an IPv4 address is checked by its
// first three numbers. To add one: printf '%s' word | sha256sum.
//
// Usage: go run .github/scripts/refguard.go   (from the repository's root; CI runs it)
//
// Standard library only, and outside ./... on purpose: it is CI's glue, not the product.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var private = map[string]bool{
	"cc61e69d583fdc3d1e46d442dab62f788367d0f84478e3b60f9812631e146aeb": true,
	"440b5043ca1192db50f3dde34bbbd2207f6861d10c998fcad4007cc088d3f799": true,
	"3ca255d76a63a7164d92d793969c7ddbcb704c10c9bde11b644f35543ca5a9ca": true,
	"d1e07e66af0633f911539651356143841822593f8c57514bdd96d6547bfc2be4": true,
	"8be0b5445fca6eb16d14c6f4eaff1cf1d1a3362ef660bfb2b66e74bf309b7e61": true,
	"8bec29be9ea395025f020a05d8f2fe8afef63250325cf01b9a8c85078aaa613e": true,
	"f24fd3749c1368328e2b149bec149cb6795619f244c5b584e844961215dadd16": true,
	"3cff4e7f66517364ce498fcb77846abebed392b881795fccd951a40dec64d1d2": true,
	"12e1c862edf7a9cc6b7319a8e44d1ef881a97ecbf2ebf2987823697e7139f861": true,
	"c6de3c105315372cbbc427cb3a96544cb9edc0f91b557deccfe10442fad08854": true,
	"b13dce85bf17fe7df536339e5fdd3ab2a8732584e5bf30161adadead0e6872ec": true,
	"d85b40105dd0b9cdbe46a1ee1b96abdf1474ae96fb86bd28ca701f44f017ac3b": true,
	"9790b8f4a5994bc873370e0d26fe4364543e14d155bd4e18e29a25a2478b665f": true,
	"61091bd615a3ccfd29a7a47f9755093e0bf594ba4e47244e6f1b3b03b1a29845": true,
}

var (
	// A hash sign and two to four digits, unless right after an ampersand (an HTML entity),
	// a slash (a URL's fragment) or a letter or digit. A CSS colour of digits only trips it
	// too: write it with rgb() or a six-digit form that has a letter, or in a CSS variable.
	numberRef = regexp.MustCompile(`(^|[^&\w/])#[0-9]{2,4}\b`)
	words     = regexp.MustCompile(`[a-z0-9]+`)
	ipv4      = regexp.MustCompile(`\b([0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})\.[0-9]{1,3}\b`)
)

// Generated or third-party files: lockfiles hold hashes, not prose.
var skip = map[string]bool{"pnpm-lock.yaml": true, "go.sum": true, "tools/go.sum": true}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func main() {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "refguard: git ls-files:", err)
		os.Exit(2)
	}
	found := 0
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name == "" || skip[name] {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the working tree
			}
			fmt.Fprintln(os.Stderr, "refguard:", err)
			os.Exit(2)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			var why []string
			if numberRef.MatchString(line) {
				why = append(why, "a #number reference")
			}
			lower := strings.ToLower(line)
			for _, w := range words.FindAllString(lower, -1) {
				if private[hash(w)] {
					why = append(why, "a private name")
					break
				}
			}
			for _, m := range ipv4.FindAllStringSubmatch(lower, -1) {
				if private[hash(m[1])] {
					why = append(why, "a private network")
					break
				}
			}
			if len(why) > 0 {
				found++
				fmt.Printf("%s:%d: %s\n", name, n, strings.Join(why, ", "))
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "refguard: %s: %v\n", name, err)
			os.Exit(2)
		}
	}
	if found > 0 {
		fmt.Printf("refguard: %d line(s) refer to something outside the repository. Keep the reasoning, drop the reference.\n", found)
		os.Exit(1)
	}
	fmt.Println("refguard: OK")
}
