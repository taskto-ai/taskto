package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// internal/integrations/testprovider is a fake Stripe for tests, with a signed-event sender. It
// is a normal package, so nothing but the compiler keeps it out of the server's binary: this test
// does. It lists the packages the binary is built from, on each platform the server ships for,
// and fails if the fake is one of them, and then lists every package's own imports (a test
// file's imports aren't among them) to name the one that imports it.
func TestTheServerDoesNotLinkTheTestProvider(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool isn't installed")
	}
	const fake = "taskto.ai/taskto/internal/integrations/testprovider"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	goList := func(goos string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", append([]string{"list"}, args...)...)
		cmd.Env = append(cmd.Environ(), "GOOS="+goos, "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list %v (GOOS=%s): %v\n%s", args, goos, err, out)
		}
		return string(out)
	}

	for _, goos := range []string{"linux", "darwin", "windows"} {
		deps := goList(goos, "-deps", "-f", "{{.ImportPath}}", ".")
		if !strings.Contains(deps, "taskto.ai/taskto/internal/integrations\n") {
			t.Fatalf("GOOS=%s: the list of the server's packages is missing the integrations package: the check proves nothing", goos)
		}
		if strings.Contains(deps, fake+"\n") {
			importers := goList(goos, "-f", `{{.ImportPath}}: {{join .Imports " "}}`, "../../...")
			var who []string
			for _, line := range strings.Split(importers, "\n") {
				pkg, imports, _ := strings.Cut(line, ":")
				if pkg != fake && strings.Contains(imports, fake) {
					who = append(who, pkg)
				}
			}
			t.Errorf("GOOS=%s: the server's binary links %s, imported by %v", goos, fake, who)
		}
	}
}
