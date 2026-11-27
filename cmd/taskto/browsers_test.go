package main

import (
	"context"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliowner"
)

// namedBrowsers is the browsers as a purge reaches them: it records the account each Forget named,
// and answers the first ones with refuse.
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

// A CLI worker that started again without its list of boots asks for a census before a browser
// purge: the server sends it and tries the purge again, once.
func TestABrowserPurgeTriesAgainAfterTheCensusTheWorkerAsksFor(t *testing.T) {
	cli := cliaccounts.New(nil, cliaccounts.Container, nil)
	t.Cleanup(cli.Close)
	b := &namedBrowsers{refuse: []error{cliowner.ErrCensusNeeded}}
	if err := (purgeBrowsers{b: b, cli: cli}).Forget(cliowner.With(context.Background(), strings.Repeat("ab", 16)), 7); err != nil {
		t.Fatalf("the purge: %v", err)
	}
	if len(b.named) != 2 {
		t.Errorf("the purge was tried %d times, want 2 (again after the census)", len(b.named))
	}
	b = &namedBrowsers{refuse: []error{cliowner.ErrCensusNeeded, cliowner.ErrCensusNeeded}}
	if err := (purgeBrowsers{b: b, cli: cli}).Forget(cliowner.With(context.Background(), strings.Repeat("ab", 16)), 7); err == nil || len(b.named) != 2 {
		t.Errorf("a purge refused again after the census: %v, tried %d times, want an error after 2", err, len(b.named))
	}
}

// A deleted account's browser purge names the account by the identity its step keeps and carries
// this server's boot, which the CLI worker's fence for deletions checks.
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
