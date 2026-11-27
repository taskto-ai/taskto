package main

import (
	"context"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliowner"
)

type namedBrowsers struct {
	browser.Browsers
	named  []cliowner.Account
	refuse []error
}

func (n *namedBrowsers) Forget(_ context.Context, a cliowner.Account) error {
	n.named = append(n.named, a)
	if len(n.refuse) > 0 {
		err := n.refuse[0]
		n.refuse = n.refuse[1:]
		return err
	}
	return nil
}

func TestABrowserPurgeCarriesTheServersBoot(t *testing.T) {
	cli := cliaccounts.New(nil, cliaccounts.Container, nil)
	t.Cleanup(cli.Close)
	b := &namedBrowsers{}
	identity := strings.Repeat("ab", 16)
	if err := (purgeBrowsers{b: b, cli: cli}).Forget(cliowner.With(context.Background(), identity), 7); err != nil {
		t.Fatal(err)
	}
	if len(b.named) != 1 || b.named[0] != (cliowner.Account{User: 7, Identity: identity, Boot: cli.Boot()}) || cli.Boot() == "" {
		t.Errorf("the purge named %+v, want user 7, the step's identity and the boot %q", b.named, cli.Boot())
	}
}
