package cliworker

import "testing"

func TestTheUIDBaseKeepsEveryHomesUIDValid(t *testing.T) {
	for _, base := range []int{-1, 0, 1000, minUIDBase - 1, maxUIDBase + 1, maxUID, 1 << 62} {
		if checkUIDBase(base) == nil {
			t.Errorf("UID base %d accepted", base)
		}
	}
	for _, base := range []int{minUIDBase, 100000, maxUIDBase} {
		if err := checkUIDBase(base); err != nil {
			t.Errorf("UID base %d refused: %v", base, err)
			continue
		}
		i := &Isolation{UIDBase: base}
		for _, home := range []int64{firstHome, maxHome - 1} {
			uid, err := i.uidOf(home)
			if err != nil || uid < minUIDBase || uid >= maxUID {
				t.Errorf("base %d, home %d: uid %d %v, want one in [%d, %d)", base, home, uid, err, minUIDBase, maxUID)
			}
		}
		for _, home := range []int64{0, firstHome - 1, maxHome} {
			if _, err := i.uidOf(home); err == nil {
				t.Errorf("base %d: home %d given a uid", base, home)
			}
		}
	}
}
