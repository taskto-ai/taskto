package desk

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/db"
)

func TestASaveByIDAnswersWithTheRowItsOwnUpdateWrote(t *testing.T) {
	ctx := context.Background()
	hooks := &db.Hooks{}
	sqlDB, err := db.OpenHooked(ctx, filepath.Join(t.TempDir(), db.FileName), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (email, name, password_hash) VALUES ('person@example.test', 'Person', 'hash')`,
		`INSERT INTO workspaces (name) VALUES ('Workspace')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin')`,
	} {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	service := New(sqlDB)
	first, err := service.SaveLayout(ctx, 1, 1, 0, 0, homeLayout(1, "home"))
	if err != nil {
		t.Fatal(err)
	}

	selects := 0
	var count func()
	count = func() {
		selects++
		hooks.BeforeQuery("SELECT id, name, layout, version, updated_at FROM desks", count)
		if selects == 2 {
			if _, err := sqlDB.Exec(`UPDATE desks SET version = version + 1, layout = ? WHERE id = ?`, string(emptyLayout), first.ID); err != nil {
				t.Error(err)
			}
		}
	}
	hooks.BeforeQuery("SELECT id, name, layout, version, updated_at FROM desks", count)

	saved, err := service.SaveLayout(ctx, 1, 1, first.ID, first.Version, homeLayout(1, "boards"))
	if err != nil {
		t.Fatal(err)
	}
	if selects != 1 {
		t.Fatalf("a save by id read the desk %d times, want only the check that it exists", selects)
	}
	if saved.Version != first.Version+1 || string(saved.Layout) != normalized(t, 1, homeLayout(1, "boards")) {
		t.Fatalf("the save answered %+v, not the row its own UPDATE wrote", saved)
	}
}

func invalidReasons(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "layout.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if name, ok := call.Fun.(*ast.Ident); !ok || name.Name != "invalid" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			reason, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			seen[reason] = true
		}
		return true
	})
	var out []string
	for reason := range seen {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func TestEveryReasonOfAnInvalidLayoutHasItsOwnTranslatedMessage(t *testing.T) {
	reasons := invalidReasons(t)
	if len(reasons) < 10 {
		t.Fatalf("found only %v", reasons)
	}
	for _, tag := range []string{"en", "pt-BR"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "i18n", "locales", tag+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var locale struct {
			Errors map[string]json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(raw, &locale); err != nil {
			t.Fatal(err)
		}
		var messages map[string]string
		if err := json.Unmarshal(locale.Errors["desk_invalid_reason"], &messages); err != nil {
			t.Fatalf("%s: errors.desk_invalid_reason: %v", tag, err)
		}
		for _, reason := range reasons {
			if strings.TrimSpace(messages[reason]) == "" {
				t.Errorf("%s: errors.desk_invalid_reason.%s has no message", tag, reason)
			}
		}
		for key := range messages {
			if !slices.Contains(reasons, key) {
				t.Errorf("%s: errors.desk_invalid_reason.%s is no reason the server sends", tag, key)
			}
		}
	}
}
