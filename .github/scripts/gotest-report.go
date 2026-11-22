// gotest-report prints `go test -json` output the way people read it: one line per package
// (ok or FAIL, with its time), then the whole output of every test that failed, of every test
// still running when its package died (a panic or a timeout), and of every package that
// failed outside a test or didn't build. CI's Go job runs it on the JSON it keeps as an
// artifact, so a failure that happens once in a while can be read from the log.
//
// Usage: go run .github/scripts/gotest-report.go go-test.json
//
// Standard library only, and outside ./... on purpose: it is CI's glue, not the product.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// event is one line of `go test -json` (cmd/test2json), build events included.
type event struct {
	Action     string
	Package    string
	ImportPath string // build-output and build-fail
	Test       string
	Elapsed    float64
	Output     string
}

type test struct {
	pkg, name string
	out       strings.Builder
	done      bool // passed, failed or skipped
	failed    bool
}

type pkg struct {
	name    string
	out     strings.Builder // output outside any test
	result  string          // pass, fail or skip; "" while running
	elapsed float64
	tests   []*test
	byName  map[string]*test
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gotest-report go-test.json")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	if err := report(f, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func report(r io.Reader, w io.Writer) error {
	pkgs := map[string]*pkg{}
	var order []string
	builds := map[string]*strings.Builder{}
	var buildFailed []string
	get := func(name string) *pkg {
		p := pkgs[name]
		if p == nil {
			p = &pkg{name: name, byName: map[string]*test{}}
			pkgs[name] = p
			order = append(order, name)
		}
		return p
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e event
		if err := json.Unmarshal(line, &e); err != nil || e.Action == "" {
			// go test prints some lines outside JSON (a vet failure, for one): keep them.
			fmt.Fprintf(w, "%s\n", line)
			continue
		}
		switch e.Action {
		case "build-output":
			b := builds[e.ImportPath]
			if b == nil {
				b = &strings.Builder{}
				builds[e.ImportPath] = b
			}
			b.WriteString(e.Output)
			continue
		case "build-fail":
			buildFailed = append(buildFailed, e.ImportPath)
			continue
		}
		if e.Package == "" {
			continue
		}
		p := get(e.Package)
		if e.Test == "" {
			switch e.Action {
			case "output":
				p.out.WriteString(e.Output)
			case "pass", "fail", "skip":
				p.result, p.elapsed = e.Action, e.Elapsed
			}
			continue
		}
		t := p.byName[e.Test]
		if t == nil {
			t = &test{pkg: e.Package, name: e.Test}
			p.byName[e.Test] = t
			p.tests = append(p.tests, t)
		}
		switch e.Action {
		case "output":
			t.out.WriteString(e.Output)
		case "pass", "skip":
			t.done = true
		case "fail":
			t.done, t.failed = true, true
		}
	}
	if err := sc.Err(); err != nil {
		// A line too long to read (one test printing 64 MB without a newline): report what
		// was read before it rather than nothing.
		fmt.Fprintf(w, "gotest-report: stopped reading: %v; what follows covers the lines before it\n\n", err)
	}

	failures := 0
	for _, name := range order {
		p := pkgs[name]
		switch p.result {
		case "pass":
			fmt.Fprintf(w, "ok  \t%s\t%.3fs\n", name, p.elapsed)
		case "fail":
			failures++
			fmt.Fprintf(w, "FAIL\t%s\t%.3fs\n", name, p.elapsed)
		case "skip":
			fmt.Fprintf(w, "?   \t%s\t[no test files]\n", name)
		default:
			failures++
			fmt.Fprintf(w, "????\t%s\t(no result: the run stopped)\n", name)
		}
	}

	sort.Strings(buildFailed)
	didntBuild := map[string]bool{} // "pkg [pkg.test]" names the package first
	for _, ip := range buildFailed {
		if f := strings.Fields(ip); len(f) > 0 {
			didntBuild[f[0]] = true
		}
		failures++
		fmt.Fprintf(w, "\n===== build failed: %s =====\n", ip)
		if b := builds[ip]; b != nil {
			fmt.Fprint(w, b.String())
		}
	}
	for _, name := range order {
		p := pkgs[name]
		if p.result == "pass" || p.result == "skip" {
			continue
		}
		listed := didntBuild[name]
		for _, t := range p.tests {
			// A test still running when its package failed is the one a panic or a timeout
			// stopped; its parent tests are running too, so only the innermost are printed.
			if t.failed || (!t.done && !hasRunningChild(p, t)) {
				state := "FAIL"
				if !t.done {
					state = "never finished"
				}
				failures++
				listed = true
				fmt.Fprintf(w, "\n===== %s: %s %s =====\n%s", state, name, t.name, t.out.String())
			}
		}
		// The package's own output: a panic's stack, a timeout, TestMain, a race report.
		out := strings.TrimSpace(p.out.String())
		switch {
		case out != "" && (!listed || hasDetail(out)):
			failures++
			fmt.Fprintf(w, "\n===== %s: output outside a test =====\n%s\n", name, out)
		case !listed:
			fmt.Fprintf(w, "\n===== %s: no failing test and no output =====\n", name)
		}
	}
	if failures == 0 {
		fmt.Fprintln(w, "\nno failing test found in the JSON")
	}
	return nil
}

// hasRunningChild says whether a subtest of t was still running too.
func hasRunningChild(p *pkg, t *test) bool {
	for _, c := range p.tests {
		if !c.done && strings.HasPrefix(c.name, t.name+"/") {
			return true
		}
	}
	return false
}

// hasDetail says whether a package's own output says more than "FAIL" and its exit status.
func hasDetail(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l == "FAIL" || l == "PASS" || strings.HasPrefix(l, "FAIL\t") ||
			strings.HasPrefix(l, "ok  \t") || strings.HasPrefix(l, "exit status ") || strings.HasPrefix(l, "coverage:") {
			continue
		}
		return true
	}
	return false
}
