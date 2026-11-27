package apitypes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var kept = map[string]struct {
	file  string
	tests []string
}{
	"taskto.ai/taskto/internal/access": {"access/nonnull_test.go", []string{"TestAppAccessRolesAreNeverNil"}},
	"taskto.ai/taskto/internal/auth":   {"auth/nonnull_test.go", []string{"TestMeWorkspacesAreNeverNil"}},
	"taskto.ai/taskto/internal/boards": {"boards/nonnull_test.go", []string{"TestCardListsAreNeverNil"}},
	"taskto.ai/taskto/internal/cliaccounts": {
		"cliaccounts/nonnull_test.go", []string{"TestSwitchingListsAreNeverNil"},
	},
	"taskto.ai/taskto/internal/notifications": {
		"notifications/nonnull_test.go", []string{"TestEmptyInboxIsNeverNil"},
	},
	"taskto.ai/taskto/internal/desk": {
		"desk/nonnull_test.go", []string{"TestDeskListsAreNeverNil"},
	},
	"taskto.ai/taskto/internal/integrations": {
		"integrations/nonnull_test.go", []string{"TestIntegrationListsAreNeverNil"},
	},
	"taskto.ai/taskto/internal/apps": {
		"apps/nonnull_test.go",
		[]string{"TestAppListsAreNeverNil", "TestRecordDataIsNeverNil", "TestHomeListsAreNeverNil", "TestTemplateListsAreNeverNil"},
	},
}

func TestNonNullFieldsAreKept(t *testing.T) {
	fields, err := NonNullFields(Roots())
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) == 0 {
		t.Fatal("no field is tagged nonnull: the check is not looking")
	}
	used := map[string]bool{}
	for _, f := range fields {
		pkg := f[:strings.LastIndex(f[:strings.LastIndex(f, ".")], ".")]
		k, ok := kept[pkg]
		if !ok {
			t.Errorf("%s is tagged nonnull and %s has no test that it is kept: add one and list it in kept", f, pkg)
			continue
		}
		used[pkg] = true
		src, err := os.ReadFile(filepath.Join("..", k.file))
		if err != nil {
			t.Errorf("%s: %v", pkg, err)
			continue
		}
		for _, name := range k.tests {
			if !strings.Contains(string(src), "func "+name+"(t *testing.T)") {
				t.Errorf("%s: %s has no test %s", pkg, k.file, name)
			}
		}
		if !strings.Contains(string(src), "nonnull.Require(") {
			t.Errorf("%s: %s never calls nonnull.Require", pkg, k.file)
		}
	}
	for pkg := range kept {
		if !used[pkg] {
			t.Errorf("%s has tests for nonnull fields, but none is tagged", pkg)
		}
	}
}
