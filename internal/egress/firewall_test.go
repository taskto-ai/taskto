package egress

import (
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// deploy/worker-firewall.sh closes every IPv4 address that isn't public to the CLI worker
// (docs/cli-workers.md#the-workers-network): the same list as this package's.
func TestWorkerFirewallHasTheSpecialRanges(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/worker-firewall.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nranges="([^"]*)"`).FindSubmatch(raw)
	if m == nil {
		t.Fatal(`no ranges="…" in deploy/worker-firewall.sh`)
	}
	var script []string
	for _, f := range strings.Fields(string(m[1])) {
		p, err := netip.ParsePrefix(f)
		if err != nil || p != p.Masked() {
			t.Errorf("%q isn't a range", f)
		}
		script = append(script, p.String())
	}
	var want []string
	for _, r := range specialRanges {
		if r.prefix.Addr().Is4() {
			want = append(want, r.prefix.String())
		}
	}
	slices.Sort(script)
	slices.Sort(want)
	if !slices.Equal(script, want) {
		t.Errorf("deploy/worker-firewall.sh's ranges:\n%v\nthis package's IPv4 ones:\n%v", script, want)
	}
}
