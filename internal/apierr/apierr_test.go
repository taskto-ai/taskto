package apierr

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusBadRequest, CodeNameLength, "a name of 1 to 100 characters is required", Params{"max": 100})
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	if got := rec.Body.String(); got != `{"error":"a name of 1 to 100 characters is required","code":"name_length","params":{"max":100}}`+"\n" {
		t.Fatalf("body = %s", got)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, New(http.StatusNotFound, CodeNotFound, "not found"))
	if got := rec.Body.String(); rec.Code != http.StatusNotFound || got != `{"error":"not found","code":"not_found"}`+"\n" {
		t.Fatalf("%d %s", rec.Code, got)
	}
	rec = httptest.NewRecorder()
	Internal(rec)
	var b map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != 500 || b["code"] != "internal" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func codes(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			v := s.(*ast.ValueSpec)
			var lit *ast.BasicLit
			if len(v.Names) == 1 && len(v.Values) == 1 {
				lit, _ = v.Values[0].(*ast.BasicLit)
			}
			if lit == nil || lit.Kind != token.STRING {
				t.Fatalf("codes.go: declare each code as `CodeName Code = \"name\"`, one per line (%s)", v.Names[0].Name)
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			out[v.Names[0].Name] = value
		}
	}
	return out
}

func TestCodesAreUniqueAndDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	seen := map[string]string{}
	for name, value := range codes(t) {
		if !strings.HasPrefix(name, "Code") {
			t.Errorf("%s: a code's constant is named Code…", name)
		}
		if !snake.MatchString(value) {
			t.Errorf("%s = %q: codes are lower_snake_case", name, value)
		}
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s are both %q", name, other, value)
		}
		seen[value] = name
		if !strings.Contains(string(doc), "`"+value+"`") {
			t.Errorf("docs/api.md doesn't list %q", value)
		}
	}
}

func TestCodesAreConstants(t *testing.T) {
	root := "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "web" || d.Name() == "agent" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		inside := f.Name.Name == "apierr"
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if isName(n.Fun, "Code", inside) && len(n.Args) == 1 {
					if _, lit := n.Args[0].(*ast.BasicLit); lit {
						t.Errorf("%s: a code made from a string; add a constant to internal/apierr/codes.go", fset.Position(n.Pos()))
					}
				}
				for name, at := range map[string]int{"Write": 2, "New": 1} {
					if isName(n.Fun, name, inside) && len(n.Args) > at {
						if lit, ok := n.Args[at].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							t.Errorf("%s: a literal code; use a constant from internal/apierr/codes.go", fset.Position(n.Pos()))
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isName(fun ast.Expr, name string, inside bool) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "apierr" && f.Sel.Name == name
	case *ast.Ident:
		return inside && f.Name == name
	}
	return false
}

func TestParamsMerge(t *testing.T) {
	if e := New(400, CodeBadRequest, "x"); e.Params != nil {
		t.Errorf("no params: %v, want nil", e.Params)
	}
	e := New(400, CodeNameLength, "x", Params{"max": 1, "min": 0}, Params{"max": 2})
	if e.Params["max"] != 2 || e.Params["min"] != 0 {
		t.Errorf("merged params: %v", e.Params)
	}
}
